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
			time.Sleep(1500 * time.Millisecond) // Back off on 429 or any error
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

		// Умный тайминг: если ловим 429, сервер нас разбанит через секунду.
		// Базово сканируем раз в 200мс или по NextTurnIn.
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

	isAggressive := len(arena.Plantations) < 10
	if isAggressive {
		b.Log("Aggressive Mode Active (< 10 plantations). Repairs disabled for colonies.")
	}

	// 2. ФОРМИРУЕМ ОЧЕРЕДЬ ЗАДАЧ
	type targetTask struct {
		name     string
		pos      []int
		needed   int
		assigned int
		priority int
	}
	isSafe := func(pos []int) bool {
		for _, m := range arena.MeteoForecasts {
			// Sandstorm: don't build in current or next position
			if m.Kind == "sandstorm" {
				if distSq(pos, m.Position) <= m.Radius*m.Radius {
					return false
				}
				if distSq(pos, m.NextPosition) <= m.Radius*m.Radius {
					return false
				}
			}
			// Earthquake: destroy everything in radius
			if m.Kind == "earthquake" && m.TurnsUntil <= 2 {
				if distSq(pos, m.Position) <= m.Radius*m.Radius {
					return false
				}
			}
		}
		return true
	}

	var tasks []targetTask

	/* РЕМОНТ ОТКЛЮЧЕН
	// ПРИОРИТЕТ 1000: Ремонт ЦУ
	if mainPlantation.Hp > 0 && mainPlantation.Hp <= 45 {
		tasks = append(tasks, targetTask{"Repair CU", mainPlantation.Position, 5, 0, 1000})
	}
	*/

	// ПРИОРИТЕТ 900: Охота на бобров
	for _, bvr := range arena.Beavers {
		tasks = append(tasks, targetTask{"Hunt Beaver", bvr.Position, 4, 0, 400})
	}

	// ПРИОРИТЕТ 1500: Достройка текущих объектов (Всегда выше экспансии и ремонта)
	for _, constr := range arena.Construction {
		if !isSafe(constr.Position) {
			b.Log(fmt.Sprintf("Skipping construction at %v due to meteo threat", constr.Position))
			continue
		}
		needed := 5
		prio := 1500
		if constr.Progress >= 90 {
			needed = 50 // Ultra-Finish
			prio = 2000
		}
		tasks = append(tasks, targetTask{"Finish Construction", constr.Position, needed, 0, prio})
	}

	// ПРИОРИТЕТ 700: Саботаж врагов
	for _, enemy := range arena.Enemy {
		tasks = append(tasks, targetTask{"Sabotage Enemy", enemy.Position, 4, 0, 700})
	}

	/* РЕМОНТ ОТКЛЮЧЕН
	// ПРИОРИТЕТ 600: Ремонт обычных плантаций (ТОЛЬКО ЕСЛИ НЕ АГРЕССИВНЫ)
	if !isAggressive {
		for _, p := range arena.Plantations {
			if !p.IsMain && !p.IsIsolated && p.Hp > 0 && p.Hp <= 25 {
				tasks = append(tasks, targetTask{"Repair Colony", p.Position, 2, 0, 600})
			}
		}
	}
	*/

	// ПРИОРИТЕТ 1-500: Экспансия
	limit := b.getMaxPlantations(arena)
	currentCount := len(arena.Plantations) + len(arena.Construction)
	if currentCount < limit {
		expansionOffsets := [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}}
		if currentCount == 1 {
			expansionOffsets = [][]int{{-1, 0}, {1, 0}}
		} else if currentCount == 2 {
			expansionOffsets = [][]int{{0, -1}, {0, 1}}
		}

		// Escape route build
		if mainPlantation.Hp > 0 {
			hasSafe := false
			cuProg := b.getCellProgress(arena, mainPlantation.Position)
			// Neighbor check is always in 4 directions
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
				for _, offset := range expansionOffsets {
					n := []int{mainPlantation.Position[0] + offset[0], mainPlantation.Position[1] + offset[1]}
					if !b.isOccupied(arena, n) && isSafe(n) {
						tasks = append(tasks, targetTask{"Expansion (Escape)", n, 5, 0, 500})
						break
					}
				}
			}
		}

		// Fill expansion candidates
		for _, p := range arena.Plantations {
			if p.IsIsolated {
				continue
			}
			for _, offset := range expansionOffsets {
				n := []int{p.Position[0] + offset[0], p.Position[1] + offset[1]}
				if !b.isOccupied(arena, n) && isSafe(n) {
					// Deduplicate Expansion
					exists := false
					for _, t := range tasks {
						if t.name == "Expansion" && t.pos[0] == n[0] && t.pos[1] == n[1] {
							exists = true
							break
						}
					}
					if exists {
						continue
					}

					d := manhattanDistance(n, mainPlantation.Position)
					prio := d
					if n[0]%7 == 0 && n[1]%7 == 0 {
						prio += 50
					} else if n[0]%7 == 0 || n[1]%7 == 0 {
						prio += 1
					}

					// Бонус за "плотность" - чем больше соседей-плантаций, тем выше приоритет
					ourNeighbors := 0
					for _, off := range [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
						if b.isOurControl(arena, []int{n[0] + off[0], n[1] + off[1]}) {
							ourNeighbors++
						}
					}
					prio += ourNeighbors * 100

					minB, minE := 999, 999
					for _, bvr := range arena.Beavers {
						if d := manhattanDistance(n, bvr.Position); d < minB {
							minB = d
						}
					}
					for _, enm := range arena.Enemy {
						if d := manhattanDistance(n, enm.Position); d < minE {
							minE = d
						}
					}
					if minB < 25 {
						prio += (25 - minB) * 10
					}
					if minE < 25 {
						prio += (25 - minE) * 8
					}

					tasks = append(tasks, targetTask{"Expansion", n, 4, 0, prio})
				}
			}
		}
	}

	// SORT TASKS
	for i := 0; i < len(tasks); i++ {
		for j := i + 1; j < len(tasks); j++ {
			if tasks[j].priority > tasks[i].priority {
				tasks[i], tasks[j] = tasks[j], tasks[i]
			}
		}
	}

	// 3. LAYERED DISTRIBUTION
	maxActions := 50 // Лимит против 429
	currentActions := 0

	for layer := 0; layer < 5; layer++ {
		for i := range tasks {
			if tasks[i].assigned >= tasks[i].needed || currentActions >= maxActions {
				continue
			}
			for _, w := range workers {
				if tasks[i].assigned >= tasks[i].needed || currentActions >= maxActions {
					break
				}
				if w.usedActions != layer {
					continue
				}

				// Клетка никогда не действует на саму себя (ремонт отключен)
				if w.p.Position[0] == tasks[i].pos[0] && w.p.Position[1] == tasks[i].pos[1] {
					continue
				}

				if manhattanDistance(w.p.Position, tasks[i].pos) <= actionRange {
					actions = append(actions, api.PlantationAction{
						Path: [][]int{{w.p.Position[0], w.p.Position[1]}, {tasks[i].pos[0], tasks[i].pos[1]}},
					})
					w.usedActions++
					tasks[i].assigned++
					currentActions++
				}
			}
		}
	}

	return actions
}

func (b *Bot) chooseBestUpgrade(arena *api.PlayerResponse) string {
	tierMap := make(map[string]api.PlantationUpgradeTierItem)
	for _, t := range arena.PlantationUpgrades.Tiers {
		tierMap[t.Name] = t
	}

	// 0. ЭКСТРЕННО
	for _, m := range arena.MeteoForecasts {
		if m.Kind == "earthquake" && m.TurnsUntil <= 3 {
			if t, ok := tierMap["earthquake_mitigation"]; ok && t.Current < t.Max {
				return "earthquake_mitigation"
			}
		}
	}

	// 1. Агрессивный старт: settlement_limit
	if len(arena.Plantations) < 10 {
		if t, ok := tierMap["settlement_limit"]; ok && t.Current < t.Max {
			return "settlement_limit"
		}
	}

	// 2. SIGNAL RANGE (Пункт 3 плана)
	if t, ok := tierMap["signal_range"]; ok && t.Current < t.Max {
		return "signal_range"
	}

	// 3. Остальное
	priority := []string{"settlement_limit", "decay_mitigation", "max_hp", "beaver_damage_mitigation", "repair_power", "vision_range"}
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

func (b *Bot) isOurControl(arena *api.PlayerResponse, pos []int) bool {
	for _, p := range arena.Plantations {
		if p.Position[0] == pos[0] && p.Position[1] == pos[1] {
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
