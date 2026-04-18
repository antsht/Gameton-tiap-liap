package bot

import (
	"fmt"
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

		// Check for incoming earthquakes
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

		if !relocated && (progress >= 85 || mainPlantation.Hp <= 20) {
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

// target — цель для распределения команд.
type target struct {
	name     string
	pos      []int
	baseVal  float64 // базовая ценность цели
	usage    int     // сколько команд уже назначено
	maxUsage int     // максимум полезных команд
	flat     bool    // если true — нет diminishing returns (стройка: каждая команда = +1 CS)
}

// marginalGain — убывающая отдача от каждой следующей команды на эту цель.
// Для flat-целей (стройка) — без уменьшения (каждая команда = +1 CS, все одинаково полезны).
// Для остальных — gain = baseVal * max(0, 5 - usage - chainPenalty).
func (t *target) marginalGain(chainPenalty int) float64 {
	if t.flat {
		// Стройка: каждая команда одинаково ценна, только chain penalty
		eff := 5 - chainPenalty
		if eff <= 0 {
			return 0
		}
		return t.baseVal * float64(eff)
	}
	eff := 5 - t.usage - chainPenalty
	if eff <= 0 {
		return 0
	}
	return t.baseVal * float64(eff)
}

func (b *Bot) computeHiveMind(arena *api.PlayerResponse) []api.PlantationAction {
	var actions []api.PlantationAction

	type worker struct {
		p           api.Plantation
		usedActions int
	}
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

	// ============================================================
	// ПРИОРИТЕТ МЕГА: Ремонт ЦУ
	// ============================================================
	if mainPlantation.Hp > 0 && mainPlantation.Hp <= 45 {
		targets = append(targets, target{
			name:     "Repair CU",
			pos:      mainPlantation.Position,
			baseVal:  10000,
			maxUsage: 5,
		})
	}

	// ============================================================
	// ПРИОРИТЕТ: Достройка текущих стройплощадок
	// flat=true — каждая команда одинаково ценна (+1 CS)
	// Если ранняя фаза — baseVal ОГРОМНЫЙ, чтобы не распылять
	// ============================================================
	for _, constr := range arena.Construction {
		val := 3000.0
		maxU := 50 // без лимита usage
		if isEarlyGame {
			val = 8000.0 // ранняя фаза: стройка — абсолютный приоритет
		}
		if constr.Progress >= 40 {
			val *= 1.5 // полупостроенное — ещё важнее дофинишить
		}
		if constr.Progress >= 90 {
			val = 15000.0 // ultra-finish
		}
		targets = append(targets, target{
			name:     "Finish Construction",
			pos:      constr.Position,
			baseVal:  val,
			maxUsage: maxU,
			flat:     true, // КЛЮЧЕВОЕ: каждая команда ценна одинаково
		})
	}

	// ============================================================
	// ПРИОРИТЕТ: Охота на бобров (x10 от клетки — огромный буст)
	// ============================================================
	for _, bvr := range arena.Beavers {
		targets = append(targets, target{
			name:     "Hunt Beaver",
			pos:      bvr.Position,
			baseVal:  2000,
			maxUsage: 4,
		})
	}

	// ============================================================
	// ПРИОРИТЕТ: Диверсия врагов
	// Kill-finish: если HP <= damage → максимальный приоритет
	// ============================================================
	for _, enemy := range arena.Enemy {
		val := b.sabotageScore(enemy)
		targets = append(targets, target{
			name:     "Sabotage Enemy",
			pos:      enemy.Position,
			baseVal:  val,
			maxUsage: 4,
		})
	}

	// ============================================================
	// ПРИОРИТЕТ: Ремонт обычных плантаций (только не в агрессивном режиме)
	// ============================================================
	if !isAggressive {
		for _, p := range arena.Plantations {
			if !p.IsMain && !p.IsIsolated && p.Hp > 0 && p.Hp <= 25 {
				targets = append(targets, target{
					name:     "Repair Colony",
					pos:      p.Position,
					baseVal:  600,
					maxUsage: 2,
				})
			}
		}
	}

	// ============================================================
	// Escape route для ЦУ
	// ============================================================
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
						name:     "Expansion (Escape)",
						pos:      n,
						baseVal:  2500,
						maxUsage: 50,
						flat:     true, // это стройка, каждая команда = +1 CS
					})
					break
				}
			}
		}
	}

	// ============================================================
	// ЭКСПАНСИЯ — только если НЕТ активной стройки ИЛИ у нас достаточно воркеров
	// В ранней фазе: МАКСИМУМ 1 новая стройка (не распылять!)
	// ============================================================
	hasActiveConstruction := len(arena.Construction) > 0
	canStartNewBuild := true

	if isEarlyGame && hasActiveConstruction {
		// В ранней фазе: не начинай новую стройку пока текущая не закончена
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
					// В ранней фазе: выбираем ЛУЧШУЮ одну клетку и спамим её
					if val > bestExpVal {
						bestExpVal = val
						bestExpPos = n
					}
				} else {
					targets = append(targets, target{
						name:     "Expansion",
						pos:      n,
						baseVal:  val,
						maxUsage: 50,
						flat:     true,
					})
				}
			}
		}

		// В ранней фазе — добавляем только лучшую клетку
		if isEarlyGame && bestExpPos != nil {
			targets = append(targets, target{
				name:     "Expansion",
				pos:      bestExpPos,
				baseVal:  bestExpVal,
				maxUsage: 50,
				flat:     true,
			})
		}
	}

	// --- MARGINAL GAIN DISTRIBUTION ---
	maxActions := 50
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
				// self-repair allowed, self-action on other tasks — skip
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
				score := t.marginalGain(chainPenalty)
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

// buildScore — ROI от строительства новой плантации.
// Учитывает: усиленные клетки, chain penalty, anti-overbuild.
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

	// Штраф за близких бобров/врагов
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
		score += float64(25-minB) * 8
	}
	if minE < 25 {
		score += float64(25-minE) * 6
	}

	if score < 1 {
		score = 1
	}
	return score
}

// sabotageScore — ценность диверсии врага.
// Kill-finish: если почти мёртв → максимальный приоритет.
func (b *Bot) sabotageScore(enemy api.EnemyPlantation) float64 {
	if enemy.Hp <= 10 {
		return 5000.0 // kill-finish priority MAX
	}

	cellValue := 1000.0
	if enemy.Position[0]%7 == 0 && enemy.Position[1]%7 == 0 {
		cellValue = 1500.0
	}

	denyFuture := cellValue * 0.8
	return denyFuture
}

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
