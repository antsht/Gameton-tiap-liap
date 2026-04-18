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
				// Relay to neighbor with immunity or high HP
				if manhattanDistance(mainPos, p.Position) == 1 && p.Hp >= 15 && p.ImmunityUntilTurn > arena.TurnNo+eqTurns {
					cmd.RelocateMain = [][]int{{mainPos[0], mainPos[1]}, {p.Position[0], p.Position[1]}}
					b.Log(fmt.Sprintf("EVACUATE (EQ AVOID!) CU %v→%v", mainPos, p.Position))
					relocated = true
					break
				}
			}
		}

		// Bug #6: Порог эвакуации 70% и выбор по деградации
		if !relocated && (progress >= 70 || mainPlantation.Hp <= 20) {
			bestTurns := -1
			var bestPos []int
			for _, p := range arena.Plantations {
				if p.IsMain || p.IsIsolated {
					continue
				}
				if manhattanDistance(mainPos, p.Position) == 1 {
					degTurns := b.getCellDegradeTurns(arena, p.Position)
					if degTurns > bestTurns && p.Hp >= 20 {
						bestTurns = degTurns
						bestPos = []int{p.Position[0], p.Position[1]}
					}
				}
			}
			if bestPos != nil {
				cmd.RelocateMain = [][]int{{mainPos[0], mainPos[1]}, {bestPos[0], bestPos[1]}}
				b.Log(fmt.Sprintf("EVACUATE CU %v→%v (prog=%d%%, degTurns=%d)", mainPos, bestPos, progress, bestTurns))
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

func (t *target) marginalGain(workerUsedActions int, outletUsedActions int, isWorkerOutlet bool) float64 {
	// Если воркер и есть выходная точка, штраф за использование лимита и штраф за проход команды — это одно и то же.
	usagePenalty := outletUsedActions
	if !isWorkerOutlet {
		// Если это РАЗНЫЕ сущности (relay), то штраф суммируется: 1 команда тратит 1 лимит воркера И 1 пропускную способность аутлета.
		// Но лимит воркера (5) уже проверен в цикле greedy. Здесь считаем именно падение эффективности.
		usagePenalty = outletUsedActions 
	}
	
	eff := 5 - usagePenalty
	if eff <= 0 {
		return 0
	}
	// Parallelization bias for constructions: favor focusing if we have few workers.
	bias := 1.0
	// BIAS REMOVED: It was causing target splitting. We now focus on finishing what we started.
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

	// 2. Применяем build-команды И считаем простой
	activePos := make(map[[2]int]bool)
	for pos, count := range buildCmds {
		s.constructions[pos] += count
		if count > 0 {
			activePos[pos] = true
		}
	}

	// 3. Завершённые стройки → новые плантации. Простой → штраф.
	for pos, prog := range s.constructions {
		if prog >= 50 {
			s.plants = append(s.plants, pos)
			delete(s.constructions, pos)
			continue
		}
		if !activePos[pos] {
			// DECAY RULE: -5 progress if no commands.
			newProg := prog - 5
			if newProg <= 0 {
				delete(s.constructions, pos)
			} else {
				s.constructions[pos] = newProg
			}
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
		// Цель — всегда ПОСЛЕДНИЙ элемент пути
		tgtPos := a.Path[len(a.Path)-1]
		tgt := [2]int{tgtPos[0], tgtPos[1]}
		if !plantSet[tgt] {
			cmds[tgt]++ // это build-команда
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
	allPlants []api.Plantation,
	actionRange int,
	signalRange int,
	maxActions int,
) []api.PlantationAction {
	var actions []api.PlantationAction
	currentActions := 0
	exitUsage := make(map[[2]int]int)
	var lastBestTarget *target

	for currentActions < maxActions {
		bestScore := -1.0
		var bestWorker *worker
		var bestTarget *target
		var bestExit []int

		for _, w := range workers {
			if w.usedActions >= 5 {
				continue
			}
			for i := range targets {
				t := &targets[i]
				if t.usage >= t.maxUsage {
					continue
				}

				// Self-repair check: worker cannot repair itself
				if t.name == "Repair CU" || t.name == "Repair Colony" || t.name == "Repair Bridge" {
					if w.p.Position[0] == t.pos[0] && w.p.Position[1] == t.pos[1] {
						continue
					}
				} else if w.p.Position[0] == t.pos[0] && w.p.Position[1] == t.pos[1] {
					// Non-repair actions also shouldn't target self location unless it's sabotage (impossible on self)
					continue
				}

				// Find best exit point (relay)
				var chosenExit []int
				// Default is the worker itself
				if chebyshevDistance(w.p.Position, t.pos) <= actionRange {
					chosenExit = w.p.Position
				}

				// Check other plants as outlets within Signal Range
				for _, relay := range allPlants {
					if chebyshevDistance(w.p.Position, relay.Position) <= signalRange &&
						chebyshevDistance(relay.Position, t.pos) <= actionRange {
						// Found a relay. If it's better (less used?), we could pick it.
						// For now, if we don't have an exit yet, or this relay has less usage than current chosenExit
						exitKey := [2]int{relay.Position[0], relay.Position[1]}
						if chosenExit == nil || exitUsage[exitKey] < exitUsage[[2]int{chosenExit[0], chosenExit[1]}] {
							chosenExit = relay.Position
						}
					}
				}

				if chosenExit == nil {
					continue
				}

				exitKey := [2]int{chosenExit[0], chosenExit[1]}
				isWorkerOutlet := (w.p.Position[0] == chosenExit[0] && w.p.Position[1] == chosenExit[1])
				score := t.marginalGain(w.usedActions, exitUsage[exitKey], isWorkerOutlet)

				// STICKY BONUS: Prefer the target we just picked in previous action iteration of this loop.
				if lastBestTarget != nil && t.pos[0] == lastBestTarget.pos[0] && t.pos[1] == lastBestTarget.pos[1] {
					score *= 1.001
				}

				if score > bestScore {
					bestScore = score
					bestWorker = w
					bestTarget = t
					bestExit = chosenExit
				}
			}
		}

		if bestWorker == nil || bestTarget == nil || bestScore <= 0 {
			break
		}

		lastBestTarget = bestTarget

		exitKey := [2]int{bestExit[0], bestExit[1]}
		actions = append(actions, api.PlantationAction{
			Path: [][]int{
				{bestWorker.p.Position[0], bestWorker.p.Position[1]},
				{bestExit[0], bestExit[1]},
				{bestTarget.pos[0], bestTarget.pos[1]},
			},
		})
		bestWorker.usedActions++
		exitUsage[exitKey]++
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

	// Изолированные не дают команд — спасти их можно только мостом/ремонтом сети.
	var isolatedPlants []api.Plantation
	var connectedPlants []api.Plantation
	for _, p := range arena.Plantations {
		if p.IsIsolated {
			isolatedPlants = append(isolatedPlants, p)
		} else {
			connectedPlants = append(connectedPlants, p)
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
	// При изоляции colony repair нужен
	isAggressive := plantCount < 10 && len(isolatedPlants) == 0

	if isAggressive {
		b.Log(fmt.Sprintf("Aggressive Mode (%d plantations). Repairs disabled for colonies.", plantCount))
	}

	isSafe := func(pos []int) bool {
		for _, m := range arena.MeteoForecasts {
			if m.Kind == "sandstorm" {
				if m.Position != nil && len(m.Position) == 2 {
					if d := chebyshevDistance(pos, m.Position); d <= m.Radius {
						return false
					}
				}
				if m.NextPosition != nil && len(m.NextPosition) == 2 {
					if d := chebyshevDistance(pos, m.NextPosition); d <= m.Radius {
						return false
					}
				}
			}
		}
		return true
	}

	canStartNewBuild := true
	maxParallelBuilds := 2
	if plantCount >= 3 {
		maxParallelBuilds = 4
	}
	if plantCount >= 10 {
		maxParallelBuilds = 99
	}

	if len(arena.Construction) >= maxParallelBuilds {
		canStartNewBuild = false
	}

	// --- СТРОИМ СПИСОК ЦЕЛЕЙ ---
	var targets []target

	// FIX #2: CU Repair must be prioritized above all else. 1M score ensures it wins.
	if mainPlantation.Hp > 0 && mainPlantation.Hp <= 30 {
		targets = append(targets, target{
			name: "Repair CU", pos: mainPlantation.Position,
			baseVal: 1000000, maxUsage: 5,
		})
	}

	// Достройка текущих стройплощадок (flat=true)
	// Now uses buildScore to ensure it's on the same ~100k scale as new build targets.
	for _, constr := range arena.Construction {
		val := b.buildScore(arena, constr.Position, mainPlantation.Position, overbuiltRatio)
		targets = append(targets, target{
			name: "Finish Construction", pos: constr.Position,
			baseVal: val, maxUsage: 50, flat: true,
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
					baseVal: 125000, maxUsage: 3, // Scaled up to ~100k+ ROI
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
				baseVal: 140000, maxUsage: 3, // Prioritize bridges over generic colonies
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
	reconnectBase := 12000.0
	if len(isolatedPlants) > 0 {
		// Выше «Finish Construction» (15000) и типичного expansion — иначе greedy жрёт одну клетку.
		reconnectBase = 85000.0
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
								if targets[i].baseVal < reconnectBase {
									targets[i].baseVal = reconnectBase
								}
								targets[i].name = "Reconnect"
								boosted = true
							}
						}
						if !boosted && !b.isOccupied(arena, n) {
							targets = append(targets, target{
								name: "Reconnect", pos: n,
								baseVal: reconnectBase + 20000, // Ensure it's slightly better than generic expansion
								maxUsage: 50, flat: true,
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
	// FOCUS LOCK: In the early game, if we have active constructions, do NOT start new once.
	// This prevents "darting" across the map.
	focusLock := isAggressive && len(arena.Construction) > 0

	if currentCount < limit && canStartNewBuild && !focusLock {
		seen := map[[2]int]bool{}
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
				targets = append(targets, target{
					name: "Expansion", pos: n,
					baseVal: val, maxUsage: 50, flat: true,
				})
			}
		}
	}

	// ================================================================
	// FIX #3: BEAM SEARCH — multi-turn lookahead (2 хода)
	// Генерируем несколько вариантов распределения команд,
	// симулируем каждый на 2 хода вперёд, выбираем лучший.
	// ================================================================
	if len(targets) <= 1 || len(workers) == 0 {
		// Тривиальный случай — beam search не нужен
		return greedyAllocate(workers, targets, arena.Plantations, actionRange, 3, 50)
	}

	type beamCandidate struct {
		actions []api.PlantationAction
		label   string
	}
	var candidates []beamCandidate

	signalRange := 3
	for _, t := range arena.PlantationUpgrades.Tiers {
		if t.Name == "signal_range" {
			signalRange = 3 + t.Current
			break
		}
	}

	maxActions := len(arena.Plantations) * 5
	candidates = append(candidates, beamCandidate{
		actions: greedyAllocate(cloneWorkers(workers), cloneTargets(targets), arena.Plantations, actionRange, signalRange, maxActions),
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
			actions: greedyAllocate(cloneWorkers(workers), modTargets, arena.Plantations, actionRange, signalRange, maxActions),
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
			actions: greedyAllocate(cloneWorkers(workers), modTargets, arena.Plantations, actionRange, signalRange, maxActions),
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
			actions: greedyAllocate(cloneWorkers(workers), modTargets, arena.Plantations, actionRange, signalRange, maxActions),
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
			actions: greedyAllocate(cloneWorkers(workers), modTargets, arena.Plantations, actionRange, signalRange, maxActions),
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

		// Connectivity Bonus: +50 per neighbor link. Favors compact/redundant networks.
		for i, p1 := range sim.plants {
			for j := i + 1; j < len(sim.plants); j++ {
				p2 := sim.plants[j]
				if abs(p1[0]-p2[0])+abs(p1[1]-p2[1]) == 1 {
					totalScore += math.Pow(discount, 3) * 50
				}
			}
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

	// REDUNDANCY BONUS: +3000 for each neighbor beyond the first.
	// This encourages building "loops" and grids instead of "tails".
	neighbors := 0
	for _, p := range arena.Plantations {
		if manhattanDistance(pos, p.Position) == 1 {
			neighbors++
		}
	}
	if neighbors > 1 {
		score += float64(neighbors-1) * 15000 // Huge bonus for grid/loop redundancy
	}

	// FOCUS BONUS: Favor existing progress heavily.
	for _, c := range arena.Construction {
		if c.Position[0] == pos[0] && c.Position[1] == pos[1] {
			// Progressive bonus to override any spatial ties.
			// At 5% progress, bonus is +15k. At 45%, it's +55k.
			score += 10000 + float64(c.Progress)*1000
			break
		}
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

	// 0. Bug #7: ЭКСТРЕННО: землетрясение. Масштабируем до turnsUntil <= 2 для надежности.
	for _, m := range arena.MeteoForecasts {
		if m.Kind == "earthquake" && m.TurnsUntil <= 2 {
			if t, ok := tierMap["earthquake_mitigation"]; ok && t.Current == 0 {
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
func chebyshevDistance(a, b []int) int {
	return int(math.Max(math.Abs(float64(a[0]-b[0])), math.Abs(float64(a[1]-b[1]))))
}

func manhattanDistance(a, b []int) int {
	return int(math.Abs(float64(a[0]-b[0])) + math.Abs(float64(a[1]-b[1])))
}
func distSq(a, b []int) int {
	if a == nil || b == nil {
		return 999999
	}
	dx, dy := a[0]-b[0], a[1]-b[1]
	return dx*dx + dy*dy
}
