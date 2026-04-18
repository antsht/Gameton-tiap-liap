package bot

import (
	"fmt"
	"math"
	"piratesbot/internal/api"
	"time"
)

func (b *Bot) loop() {
	lastProcessedTurn := -1

	for {
		select {
		case <-b.stopCh:
			return
		default:
		}

		arena, err := b.client.GetArena()
		if err != nil {
			b.Log(fmt.Sprintf("API Arena Error: %v", err))
			time.Sleep(1500 * time.Millisecond)
			continue
		}

		b.state.ArenaLock.Lock()
		b.state.Arena = arena
		b.state.TurnNo = arena.TurnNo
		b.state.ArenaLock.Unlock()

		if arena.TurnNo != lastProcessedTurn {
			b.processTurn(arena)
			lastProcessedTurn = arena.TurnNo
		}

		waitMs := int(arena.NextTurnIn*1000) + 50
		if waitMs < 200 {
			waitMs = 200
		}
		time.Sleep(time.Duration(waitMs) * time.Millisecond)
	}
}

func (b *Bot) processTurn(arena *api.PlayerResponse) {
	if len(arena.Plantations) == 0 {
		b.Log(fmt.Sprintf("Turn %d. We have 0 plantations!", arena.TurnNo))
		return
	}

	cmd := api.PlayerCommand{}

	// Upgrade logic.
	if arena.PlantationUpgrades.Points > 0 {
		upgrade := b.chooseBestUpgrade(arena)
		if upgrade != "" {
			cmd.PlantationUpgrade = upgrade
			b.Log(fmt.Sprintf("Buying upgrade: %s", upgrade))
		}
	}

	// --- CU Evacuation & Survival ---
	var mainPlantation *api.Plantation
	for i := range arena.Plantations {
		if arena.Plantations[i].IsMain {
			mainPlantation = &arena.Plantations[i]
			break
		}
	}

	if mainPlantation != nil {
		mainPos := []int{mainPlantation.Position[0], mainPlantation.Position[1]}
		progress := b.getCellProgress(arena, mainPos)

		eqTurns := -1
		for _, m := range arena.MeteoForecasts {
			if m.Kind == "earthquake" {
				eqTurns = m.TurnsUntil
				break
			}
		}

		eqImminent := eqTurns >= 0 && eqTurns <= 2
		cuNeedsEqEscape := eqImminent && (mainPlantation.ImmunityUntilTurn <= arena.TurnNo+eqTurns)

		relocated := false
		if cuNeedsEqEscape {
			b.Log(fmt.Sprintf("EMERGENCY! Earthquake in %d turns. CU lacks immunity!", eqTurns))
			for _, p := range arena.Plantations {
				if p.IsMain || p.IsIsolated {
					continue
				}
				dx, dy := abs(p.Position[0]-mainPos[0]), abs(p.Position[1]-mainPos[1])
				if dx+dy == 1 && p.Hp >= 15 && p.ImmunityUntilTurn > arena.TurnNo+eqTurns {
					cmd.RelocateMain = [][]int{{mainPos[0], mainPos[1]}, {p.Position[0], p.Position[1]}}
					b.Log(fmt.Sprintf("EVACUATE (EQ AVOID!) CU %v→%v", mainPos, p.Position))
					relocated = true
					break
				}
			}
		}

		// FIX #2: порог был 20, теперь 15 — только критический HP
		if !relocated && (progress >= 85 || mainPlantation.Hp <= 15) {
			bestProg := progress
			var bestPos []int
			for _, p := range arena.Plantations {
				if p.IsMain || p.IsIsolated {
					continue
				}
				dx, dy := abs(p.Position[0]-mainPos[0]), abs(p.Position[1]-mainPos[1])
				if dx+dy == 1 {
					pProg := b.getCellProgress(arena, p.Position)
					if p.Hp >= 15 && pProg < bestProg {
						bestProg = pProg
						bestPos = []int{p.Position[0], p.Position[1]}
					}
				}
			}
			if bestPos != nil {
				cmd.RelocateMain = [][]int{{mainPos[0], mainPos[1]}, {bestPos[0], bestPos[1]}}
				b.Log(fmt.Sprintf("EVACUATE CU %v→%v (prog=%d%%)", mainPos, bestPos, progress))
				relocated = true
			}
		}

		// Strategic Relocation: move towards "center of mass" if current cell is finished
		if !relocated && progress >= 60 {
			var bestPos []int
			bestScore := -1.0

			for _, p := range arena.Plantations {
				if p.IsMain || p.IsIsolated || p.Hp < 25 {
					continue
				}
				dx, dy := abs(p.Position[0]-mainPos[0]), abs(p.Position[1]-mainPos[1])
				if dx+dy == 1 {
					// Score based on how many neighbors this new position has
					neighbors := 0
					for _, other := range arena.Plantations {
						if manhattanDistance(p.Position, other.Position) == 1 {
							neighbors++
						}
					}
					pProg := b.getCellProgress(arena, p.Position)
					score := float64(neighbors)*100.0 - float64(pProg)

					if score > bestScore {
						bestScore = score
						bestPos = []int{p.Position[0], p.Position[1]}
					}
				}
			}
			// Only move if the improvement is significant or current cell is very high progress
			if bestPos != nil && (bestScore > 150 || progress >= 95) {
				cmd.RelocateMain = [][]int{{mainPos[0], mainPos[1]}, {bestPos[0], bestPos[1]}}
				b.Log(fmt.Sprintf("STRATEGIC RELOCATE CU %v→%v (score=%.1f)", mainPos, bestPos, bestScore))
			}
		}
	}

	cmd.Command = b.computeHiveMind(arena)

	if len(cmd.Command) > 0 || cmd.PlantationUpgrade != "" || len(cmd.RelocateMain) > 0 {
		err := b.client.PostCommand(cmd)
		if err != nil {
			b.Log(fmt.Sprintf("Err sending cmd: %v", err))
		} else if len(cmd.Command) > 0 {
			b.Log(fmt.Sprintf("HiveMind sent %d local commands", len(cmd.Command)))
		}
	}

	go b.dumpTurn(arena, cmd)
}

// ========================================================================
// Types
// ========================================================================

type target struct {
	name     string
	pos      []int
	baseVal  float64
	usage    int
	maxUsage int
	flat     bool // если true — нет diminishing returns (стройка)
}

func (t *target) marginalGain(workerUsedActions int, chainPenalty int) float64 {
	eff := 5 - workerUsedActions - chainPenalty
	if eff <= 0 {
		return 0
	}
	// Parallelization bias for constructions: slightly favor new targets if many commands are already assigned.
	bias := 1.0
	if t.flat && t.usage > 0 {
		bias = math.Max(0.7, 1.0-float64(t.usage)*0.03)
	}
	return t.baseVal * float64(eff) * bias
}

type worker struct {
	p           api.Plantation
	usedActions int
}

// ========================================================================
// Beam Search Simulation
// ========================================================================

type simState struct {
	plants        [][2]int
	constructions map[[2]int]int // pos -> progress (0..50)
	score         float64
}

func newSimState(arena *api.PlayerResponse) *simState {
	s := &simState{constructions: make(map[[2]int]int)}
	for _, p := range arena.Plantations {
		if !p.IsIsolated {
			s.plants = append(s.plants, [2]int{p.Position[0], p.Position[1]})
		}
	}
	for _, c := range arena.Construction {
		s.constructions[[2]int{c.Position[0], c.Position[1]}] = c.Progress
	}
	return s
}

func (s *simState) clone() *simState {
	ns := &simState{
		plants:        make([][2]int, len(s.plants)),
		constructions: make(map[[2]int]int),
		score:         s.score,
	}
	copy(ns.plants, s.plants)
	for k, v := range s.constructions {
		ns.constructions[k] = v
	}
	return ns
}

// advance — один ход вперёд: терраформация + применение build команд + завершение строек.
func (s *simState) advance(buildCmds map[[2]int]int) float64 {
	turnScore := 0.0

	// 1. Терраформация: каждая плантация приносит очки
	for _, p := range s.plants {
		val := 10.0
		if p[0]%7 == 0 && p[1]%7 == 0 {
			val = 15.0
		}
		turnScore += val * 5 // TS = 5
	}

	// 2. Применяем build-команды
	for pos, count := range buildCmds {
		s.constructions[pos] += count
	}

	// 3. Завершённые стройки → новые плантации
	for pos, prog := range s.constructions {
		if prog >= 50 {
			s.plants = append(s.plants, pos)
			delete(s.constructions, pos)
		}
	}

	s.score += turnScore
	return turnScore
}

// ========================================================================
// Clone helpers for beam search
// ========================================================================

func cloneTargets(targets []target) []target {
	cloned := make([]target, len(targets))
	for i, t := range targets {
		cloned[i] = t
		cloned[i].pos = append([]int{}, t.pos...)
		cloned[i].usage = 0
	}
	return cloned
}

func cloneWorkers(workers map[string]*worker) map[string]*worker {
	cloned := make(map[string]*worker, len(workers))
	for k, v := range workers {
		w := *v
		w.usedActions = 0
		cloned[k] = &w
	}
	return cloned
}

// extractBuildCmds — из списка PlantationAction вытаскивает, сколько build-команд идёт на каждую позицию.
func extractBuildCmds(actions []api.PlantationAction, arena *api.PlayerResponse) map[[2]int]int {
	plantSet := make(map[[2]int]bool)
	for _, p := range arena.Plantations {
		plantSet[[2]int{p.Position[0], p.Position[1]}] = true
	}
	cmds := make(map[[2]int]int)
	for _, a := range actions {
		if len(a.Path) < 2 {
			continue
		}
		tgt := [2]int{a.Path[1][0], a.Path[1][1]}
		if !plantSet[tgt] {
			cmds[tgt]++ // это build-команда (не repair/sabotage на свою плантацию)
		}
	}
	return cmds
}

// ========================================================================
// Greedy Allocation
// ========================================================================

func greedyAllocate(
	workers map[string]*worker,
	targets []target,
	actionRange int,
	maxActions int,
) []api.PlantationAction {
	var actions []api.PlantationAction
	currentActions := 0

	for currentActions < maxActions {
		bestScore := -1.0
		var bestWorker *worker
		var bestTarget *target

		for _, w := range workers {
			if w.usedActions >= 5 {
				continue
			}
			for i := range targets {
				t := &targets[i]
				if t.usage >= t.maxUsage {
					continue
				}
				if t.name != "Repair CU" && t.name != "Repair Colony" {
					if w.p.Position[0] == t.pos[0] && w.p.Position[1] == t.pos[1] {
						continue
					}
				}
				dist := manhattanDistance(w.p.Position, t.pos)
				if dist > actionRange {
					continue
				}
				chainPenalty := dist
				score := t.marginalGain(w.usedActions, chainPenalty)
				if score > bestScore {
					bestScore = score
					bestWorker = w
					bestTarget = t
				}
			}
		}

		if bestWorker == nil || bestTarget == nil || bestScore <= 0 {
			break
		}

		actions = append(actions, api.PlantationAction{
			Path: [][]int{
				{bestWorker.p.Position[0], bestWorker.p.Position[1]},
				{bestTarget.pos[0], bestTarget.pos[1]},
			},
		})
		bestWorker.usedActions++
		bestTarget.usage++
		currentActions++
	}

	return actions
}

// ========================================================================
// computeHiveMind — main decision engine
// ========================================================================

func (b *Bot) computeHiveMind(arena *api.PlayerResponse) []api.PlantationAction {
	workers := make(map[string]*worker)
	var mainPlantation api.Plantation
	for _, p := range arena.Plantations {
		if !p.IsIsolated {
			workers[p.Id] = &worker{p: p, usedActions: 0}
		}
		if p.IsMain {
			mainPlantation = p
		}
	}

	actionRange := arena.ActionRange
	if actionRange <= 0 {
		actionRange = 1
	}

	limit := b.getMaxPlantations(arena)
	currentCount := len(arena.Plantations) + len(arena.Construction)
	overbuiltRatio := float64(currentCount) / float64(limit)

	plantCount := len(arena.Plantations)
	isEarlyGame := plantCount < 5
	isAggressive := plantCount < 10

	if isAggressive {
		b.Log(fmt.Sprintf("Aggressive Mode (%d plantations). Repairs disabled for colonies.", plantCount))
	}

	isSafe := func(pos []int) bool {
		for _, m := range arena.MeteoForecasts {
			if m.Kind == "sandstorm" {
				if d := distSq(pos, m.Position); d <= m.Radius*m.Radius {
					return false
				}
				if d := distSq(pos, m.NextPosition); d <= m.Radius*m.Radius {
					return false
				}
			}
		}
		return true
	}

	// --- СТРОИМ СПИСОК ЦЕЛЕЙ ---
	var targets []target

	// FIX #2: порог Repair CU снижен с 45 до 30. При hp>30 — не тратим команды на repair.
	if mainPlantation.Hp > 0 && mainPlantation.Hp <= 30 {
		targets = append(targets, target{
			name: "Repair CU", pos: mainPlantation.Position,
			baseVal: 10000, maxUsage: 5,
		})
	}

	// Достройка текущих стройплощадок (flat=true)
	for _, constr := range arena.Construction {
		val := 3000.0
		maxU := 50
		if isEarlyGame {
			val = 8000.0
		}
		if constr.Progress >= 40 {
			val *= 1.5
		}
		if constr.Progress >= 90 {
			val = 15000.0
		}
		targets = append(targets, target{
			name: "Finish Construction", pos: constr.Position,
			baseVal: val, maxUsage: maxU, flat: true,
		})
	}

	// Охота на бобров
	for _, bvr := range arena.Beavers {
		targets = append(targets, target{
			name: "Hunt Beaver", pos: bvr.Position,
			baseVal: 2000, maxUsage: 4,
		})
	}

	// Диверсия врагов
	for _, enemy := range arena.Enemy {
		val := b.sabotageScore(enemy)
		targets = append(targets, target{
			name: "Sabotage Enemy", pos: enemy.Position,
			baseVal: val, maxUsage: 4,
		})
	}

	// Ремонт обычных плантаций
	if !isAggressive {
		for _, p := range arena.Plantations {
			if !p.IsMain && !p.IsIsolated && p.Hp > 0 && p.Hp <= 25 {
				targets = append(targets, target{
					name: "Repair Colony", pos: p.Position,
					baseVal: 600, maxUsage: 2,
				})
			}
		}
	}

	// ============================================================
	// BRIDGE REPAIR — ремонт плантаций, чья смерть изолирует других.
	// Работает ВСЕГДА, даже в aggressive mode.
	// ============================================================
	for _, p := range arena.Plantations {
		if p.IsMain || p.IsIsolated || p.Hp > 25 {
			continue
		}
		neighborCount := 0
		for _, other := range arena.Plantations {
			if other.Id == p.Id || other.IsIsolated {
				continue
			}
			if manhattanDistance(p.Position, other.Position) == 1 {
				neighborCount++
			}
		}
		if neighborCount >= 2 {
			targets = append(targets, target{
				name: "Repair Bridge", pos: p.Position,
				baseVal: 8000, maxUsage: 3,
			})
			b.Log(fmt.Sprintf("BRIDGE REPAIR: %v hp=%d (connects %d neighbors)", p.Position, p.Hp, neighborCount))
		}
	}

	// ============================================================
	// PREEMPTIVE BRIDGE — если мост скоро деградирует, построить обходной
	// путь ЗАРАНЕЕ, пока мост ещё жив.
	// ============================================================
	for _, p := range arena.Plantations {
		if p.IsIsolated {
			continue
		}
		degTurns := b.getCellDegradeTurns(arena, p.Position)
		if degTurns < 0 || degTurns > 15 {
			continue
		}

		// Считаем соседей-плантации
		var neighborPos [][]int
		for _, other := range arena.Plantations {
			if other.Id == p.Id || other.IsIsolated {
				continue
			}
			if manhattanDistance(p.Position, other.Position) == 1 {
				neighborPos = append(neighborPos, other.Position)
			}
		}
		if len(neighborPos) < 2 {
			continue // не мост — деградация не опасна
		}

		b.Log(fmt.Sprintf("DEGRADATION WARNING: bridge %v degrades in %d turns (connects %d)",
			p.Position, degTurns, len(neighborPos)))

		// Ищем лучшую позицию для обходного моста:
		// Цель: позиция, смежная с максимумом соседей dying_bridge
		var bestBypass []int
		bestConnects := 0
		for _, offset := range [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			n := []int{p.Position[0] + offset[0], p.Position[1] + offset[1]}
			if b.isOccupied(arena, n) || !isSafe(n) {
				continue
			}
			// skip if n is the same position as the bridge itself
			connects := 0
			for _, np := range neighborPos {
				if manhattanDistance(n, np) <= 1 {
					connects++
				}
			}
			if connects > bestConnects {
				bestConnects = connects
				bestBypass = n
			}
		}

		if bestBypass != nil && bestConnects >= 1 {
			val := 7000.0
			if degTurns <= 8 {
				val = 8500.0
			}
			// Boost existing target if already present
			boosted := false
			for i := range targets {
				if targets[i].pos[0] == bestBypass[0] && targets[i].pos[1] == bestBypass[1] {
					if targets[i].baseVal < val {
						targets[i].baseVal = val
					}
					targets[i].name = "Preemptive Bridge"
					boosted = true
				}
			}
			if !boosted {
				targets = append(targets, target{
					name:     "Preemptive Bridge",
					pos:      bestBypass,
					baseVal:  val,
					maxUsage: 50,
					flat:     true,
				})
			}
			b.Log(fmt.Sprintf("PREEMPTIVE BRIDGE: build %v to bypass dying %v (degrades in %d, connects %d)",
				bestBypass, p.Position, degTurns, bestConnects))
		}
	}

	// ============================================================
	// RECONNECT — если есть isolated плантации, построить мост обратно.
	// Приоритет ВЫШЕ expansion, НИЖЕ CU repair.
	// ============================================================
	var isolatedPlants []api.Plantation
	var connectedPlants []api.Plantation
	for _, p := range arena.Plantations {
		if p.IsIsolated {
			isolatedPlants = append(isolatedPlants, p)
		} else {
			connectedPlants = append(connectedPlants, p)
		}
	}

	if len(isolatedPlants) > 0 && len(connectedPlants) > 0 {
		b.Log(fmt.Sprintf("WARNING: %d isolated plantations! Reconnect priority!", len(isolatedPlants)))

		reconnectSeen := map[[2]int]bool{}
		// Ищем позиции, смежные с connected, которые также смежны с isolated
		for _, cp := range connectedPlants {
			for _, offset := range [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				n := []int{cp.Position[0] + offset[0], cp.Position[1] + offset[1]}
				key := [2]int{n[0], n[1]}
				if reconnectSeen[key] {
					continue
				}
				reconnectSeen[key] = true

				// Уже занята плантацией? Тогда не нужно строить
				isPlant := false
				for _, pp := range arena.Plantations {
					if pp.Position[0] == n[0] && pp.Position[1] == n[1] {
						isPlant = true
						break
					}
				}
				if isPlant {
					continue
				}

				// Эта позиция смежна с isolated?
				for _, ip := range isolatedPlants {
					if manhattanDistance(n, ip.Position) <= 1 {
						// Нашли мост! Проверяем, есть ли уже такой target (construction)
						boosted := false
						for i := range targets {
							if targets[i].pos[0] == n[0] && targets[i].pos[1] == n[1] {
								if targets[i].baseVal < 9000 {
									targets[i].baseVal = 9000
								}
								targets[i].name = "Reconnect"
								boosted = true
							}
						}
						if !boosted && !b.isOccupied(arena, n) {
							targets = append(targets, target{
								name: "Reconnect", pos: n,
								baseVal: 12000, maxUsage: 50, flat: true,
							})
						}
						b.Log(fmt.Sprintf("RECONNECT target: %v (bridges %v to %v)", n, cp.Position, ip.Position))
						break
					}
				}
			}
		}
	}

	// Escape route для ЦУ
	if mainPlantation.Hp > 0 {
		hasSafe := false
		cuProg := b.getCellProgress(arena, mainPlantation.Position)
		for _, offset := range [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			n := []int{mainPlantation.Position[0] + offset[0], mainPlantation.Position[1] + offset[1]}
			if b.isUnderConstruction(arena, n) {
				hasSafe = true
				break
			}
			for _, p := range arena.Plantations {
				if p.Position[0] == n[0] && p.Position[1] == n[1] && p.Hp > 0 && b.getCellProgress(arena, p.Position) < cuProg {
					hasSafe = true
					break
				}
			}
		}
		if !hasSafe {
			for _, offset := range [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				n := []int{mainPlantation.Position[0] + offset[0], mainPlantation.Position[1] + offset[1]}
				if !b.isOccupied(arena, n) && isSafe(n) {
					targets = append(targets, target{
						name: "Expansion (Escape)", pos: n,
						baseVal: 2500, maxUsage: 50, flat: true,
					})
					break
				}
			}
		}
	}

	// Экспансия
	hasActiveConstruction := len(arena.Construction) > 0
	canStartNewBuild := true
	if isEarlyGame && hasActiveConstruction {
		canStartNewBuild = false
	}

	if currentCount < limit && canStartNewBuild {
		seen := map[[2]int]bool{}
		bestExpVal := 0.0
		var bestExpPos []int

		for _, p := range arena.Plantations {
			if p.IsIsolated {
				continue
			}
			for _, offset := range [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				n := []int{p.Position[0] + offset[0], p.Position[1] + offset[1]}
				key := [2]int{n[0], n[1]}
				if seen[key] || b.isOccupied(arena, n) || !isSafe(n) {
					continue
				}
				seen[key] = true

				val := b.buildScore(arena, n, mainPlantation.Position, overbuiltRatio)

				if isEarlyGame {
					if val > bestExpVal {
						bestExpVal = val
						bestExpPos = n
					}
				} else {
					targets = append(targets, target{
						name: "Expansion", pos: n,
						baseVal: val, maxUsage: 50, flat: true,
					})
				}
			}
		}

		if isEarlyGame && bestExpPos != nil {
			targets = append(targets, target{
				name: "Expansion", pos: bestExpPos,
				baseVal: bestExpVal, maxUsage: 50, flat: true,
			})
		}
	}

	maxActions := 50

	// ================================================================
	// FIX #3: BEAM SEARCH — multi-turn lookahead (2 хода)
	// Генерируем несколько вариантов распределения команд,
	// симулируем каждый на 2 хода вперёд, выбираем лучший.
	// ================================================================
	if len(targets) <= 1 || len(workers) == 0 {
		// Тривиальный случай — beam search не нужен
		return greedyAllocate(workers, targets, actionRange, maxActions)
	}

	type beamCandidate struct {
		actions []api.PlantationAction
		label   string
	}
	var candidates []beamCandidate

	// Candidate 0: baseline (оригинальные веса)
	candidates = append(candidates, beamCandidate{
		actions: greedyAllocate(cloneWorkers(workers), cloneTargets(targets), actionRange, maxActions),
		label:   "baseline",
	})

	// Candidate 2: defensive (focus on survival and connectivity)
	{
		modTargets := cloneTargets(targets)
		for i := range modTargets {
			switch modTargets[i].name {
			case "Repair CU":
				modTargets[i].baseVal *= 2.0
			case "Reconnect", "Repair Bridge", "Preemptive Bridge":
				modTargets[i].baseVal *= 1.8
			case "Repair Colony":
				modTargets[i].baseVal *= 1.5
			case "Expansion", "Finish Construction":
				modTargets[i].baseVal *= 0.5
			}
		}
		candidates = append(candidates, beamCandidate{
			actions: greedyAllocate(cloneWorkers(workers), modTargets, actionRange, maxActions),
			label:   "defensive",
		})
	}

	// Candidate 3: aggressive_expansion
	{
		modTargets := cloneTargets(targets)
		for i := range modTargets {
			if modTargets[i].name == "Expansion" || modTargets[i].name == "Finish Construction" {
				modTargets[i].baseVal *= 2.0
			} else if modTargets[i].name == "Sabotage Enemy" || modTargets[i].name == "Hunt Beaver" {
				modTargets[i].baseVal *= 0.5
			}
		}
		candidates = append(candidates, beamCandidate{
			actions: greedyAllocate(cloneWorkers(workers), modTargets, actionRange, maxActions),
			label:   "aggressive_expansion",
		})
	}

	// Candidate 4: vicious (sabotage and beavers)
	{
		modTargets := cloneTargets(targets)
		for i := range modTargets {
			if modTargets[i].name == "Sabotage Enemy" || modTargets[i].name == "Hunt Beaver" {
				modTargets[i].baseVal *= 2.5
			}
		}
		candidates = append(candidates, beamCandidate{
			actions: greedyAllocate(cloneWorkers(workers), modTargets, actionRange, maxActions),
			label:   "vicious",
		})
	}

	// Candidate 5+: focus each active construction
	for _, constr := range arena.Construction {
		modTargets := cloneTargets(targets)
		for i := range modTargets {
			if modTargets[i].name == "Finish Construction" &&
				modTargets[i].pos[0] == constr.Position[0] && modTargets[i].pos[1] == constr.Position[1] {
				modTargets[i].baseVal = 25000
			}
		}
		candidates = append(candidates, beamCandidate{
			actions: greedyAllocate(cloneWorkers(workers), modTargets, actionRange, maxActions),
			label:   fmt.Sprintf("focus_constr_%d_%d", constr.Position[0], constr.Position[1]),
		})
	}

	// --- SIMULATE EACH CANDIDATE 3 TURNS FORWARD ---
	root := newSimState(arena)
	discount := 0.85
	bestScore := -math.MaxFloat64
	bestIdx := 0

	for i, cand := range candidates {
		sim := root.clone()
		buildCmds := extractBuildCmds(cand.actions, arena)

		totalScore := 0.0
		for t := 0; t < 3; t++ {
			turnScore := sim.advance(buildCmds)
			totalScore += math.Pow(discount, float64(t)) * turnScore
		}
		// Terminal bonus: экспоненциальный рост (плантации + прогресс)
		totalScore += math.Pow(discount, 3) * float64(len(sim.plants)) * 300
		for _, prog := range sim.constructions {
			totalScore += math.Pow(discount, 3) * float64(prog) * 3
		}

		if totalScore > bestScore {
			bestScore = totalScore
			bestIdx = i
		}
	}

	b.Log(fmt.Sprintf("BeamSearch: %d candidates, winner=%s (score=%.0f)",
		len(candidates), candidates[bestIdx].label, bestScore))

	return candidates[bestIdx].actions
}

// ========================================================================
// Scoring Functions
// ========================================================================

// buildScore — ROI от строительства новой плантации.
func (b *Bot) buildScore(arena *api.PlayerResponse, pos []int, cuPos []int, overbuiltRatio float64) float64 {
	cellValue := 1000.0
	if pos[0]%7 == 0 && pos[1]%7 == 0 {
		cellValue = 1500.0 // усиленная клетка +50%
	}

	expectedLifetime := 100.0
	distFromCU := float64(manhattanDistance(pos, cuPos))
	chainPenalty := distFromCU * 10
	buildTimePenalty := 50.0

	score := (cellValue * expectedLifetime) - buildTimePenalty - chainPenalty

	// Anti-overbuild: штраф если >80% лимита
	if overbuiltRatio > 0.8 {
		score *= (1.0 - (overbuiltRatio-0.8)*5)
	}

	// FIX #1: ШТРАФ (а не бонус!) за близость к бобрам/врагам
	minB, minE := 999, 999
	for _, bvr := range arena.Beavers {
		if d := manhattanDistance(pos, bvr.Position); d < minB {
			minB = d
		}
	}
	for _, enm := range arena.Enemy {
		if d := manhattanDistance(pos, enm.Position); d < minE {
			minE = d
		}
	}
	if minB < 25 {
		score -= float64(25-minB) * 8 // WAS += (bug), NOW -= (penalty)
	}
	if minE < 25 {
		score -= float64(25-minE) * 6 // WAS += (bug), NOW -= (penalty)
	}

	if score < 1 {
		score = 1
	}
	return score
}

// sabotageScore — ценность диверсии врага. Kill-finish priority.
func (b *Bot) sabotageScore(enemy api.EnemyPlantation) float64 {
	if enemy.Hp <= 10 {
		return 5000.0 // kill-finish priority MAX
	}

	cellValue := 1000.0
	if enemy.Position[0]%7 == 0 && enemy.Position[1]%7 == 0 {
		cellValue = 1500.0
	}

	return cellValue * 0.8
}

// ========================================================================
// Upgrades
// ========================================================================

func (b *Bot) chooseBestUpgrade(arena *api.PlayerResponse) string {
	tierMap := make(map[string]api.PlantationUpgradeTierItem)
	for _, t := range arena.PlantationUpgrades.Tiers {
		tierMap[t.Name] = t
	}

	// 0. ЭКСТРЕННО: землетрясение
	for _, m := range arena.MeteoForecasts {
		if m.Kind == "earthquake" && m.TurnsUntil <= 3 {
			if t, ok := tierMap["earthquake_mitigation"]; ok && t.Current < t.Max {
				return "earthquake_mitigation"
			}
		}
	}

	// 1. settlement_limit — первый приоритет (x10 расширение)
	if t, ok := tierMap["settlement_limit"]; ok && t.Current < t.Max {
		return "settlement_limit"
	}

	// 2. signal_range — больше радиус → меньше chain penalty
	if t, ok := tierMap["signal_range"]; ok && t.Current < t.Max {
		return "signal_range"
	}

	// 3. max_hp — выживаемость
	if t, ok := tierMap["max_hp"]; ok && t.Current < t.Max {
		return "max_hp"
	}

	// 4. Остальное
	priority := []string{"decay_mitigation", "beaver_damage_mitigation", "repair_power", "vision_range"}
	for _, name := range priority {
		if t, ok := tierMap[name]; ok && t.Current < t.Max {
			return name
		}
	}
	return ""
}

// ========================================================================
// Helpers
// ========================================================================

func (b *Bot) getMaxPlantations(arena *api.PlayerResponse) int {
	for _, t := range arena.PlantationUpgrades.Tiers {
		if t.Name == "settlement_limit" {
			return 30 + t.Current
		}
	}
	return 30
}

func (b *Bot) getCellProgress(arena *api.PlayerResponse, pos []int) int {
	for _, c := range arena.Cells {
		if c.Position[0] == pos[0] && c.Position[1] == pos[1] {
			return c.TerraformationProgress
		}
	}
	return 0
}

func (b *Bot) getCellDegradeTurns(arena *api.PlayerResponse, pos []int) int {
	for _, c := range arena.Cells {
		if c.Position[0] == pos[0] && c.Position[1] == pos[1] {
			return c.TurnsUntilDegradation
		}
	}
	return -1
}

func (b *Bot) isUnderConstruction(arena *api.PlayerResponse, pos []int) bool {
	for _, c := range arena.Construction {
		if c.Position[0] == pos[0] && c.Position[1] == pos[1] {
			return true
		}
	}
	return false
}

func (b *Bot) isOccupied(arena *api.PlayerResponse, pos []int) bool {
	if pos[0] < 0 || pos[1] < 0 || pos[0] >= arena.Size[0] || pos[1] >= arena.Size[1] {
		return true
	}
	for _, m := range arena.Mountains {
		if m[0] == pos[0] && m[1] == pos[1] {
			return true
		}
	}
	for _, p := range arena.Plantations {
		if p.Position[0] == pos[0] && p.Position[1] == pos[1] {
			return true
		}
	}
	for _, e := range arena.Enemy {
		if e.Position[0] == pos[0] && e.Position[1] == pos[1] {
			return true
		}
	}
	for _, c := range arena.Construction {
		if c.Position[0] == pos[0] && c.Position[1] == pos[1] {
			return true
		}
	}
	return false
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
func manhattanDistance(a, b []int) int { return abs(a[0]-b[0]) + abs(a[1]-b[1]) }
func distSq(a, b []int) int {
	if a == nil || b == nil {
		return 999999
	}
	dx, dy := a[0]-b[0], a[1]-b[1]
	return dx*dx + dy*dy
}
