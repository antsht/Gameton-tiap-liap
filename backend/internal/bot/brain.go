package bot

import (
	"fmt"
	"piratesbot/internal/api"
	"sync"
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

	// 1. ПОДГОТОВКА ДАННЫХ И ОПТИМИЗАЦИЯ КЕША
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

	// Оптимизация: индексируем карту для быстрого доступа O(1)
	occupiedMap := make(map[string]bool)
	for _, m := range arena.Mountains {
		occupiedMap[fmt.Sprintf("%d,%d", m[0], m[1])] = true
	}
	for _, p := range arena.Plantations {
		occupiedMap[fmt.Sprintf("%d,%d", p.Position[0], p.Position[1])] = true
	}
	for _, e := range arena.Enemy {
		occupiedMap[fmt.Sprintf("%d,%d", e.Position[0], e.Position[1])] = true
	}
	for _, c := range arena.Construction {
		occupiedMap[fmt.Sprintf("%d,%d", c.Position[0], c.Position[1])] = true
	}

	fastIsOccupied := func(pos []int) bool {
		if pos[0] < 0 || pos[1] < 0 || pos[0] >= arena.Size[0] || pos[1] >= arena.Size[1] {
			return true
		}
		return occupiedMap[fmt.Sprintf("%d,%d", pos[0], pos[1])]
	}

	// Локальная функция проверки безопасности (вынесена из замыкания для удобства)
	isSafe := func(pos []int) bool {
		for _, m := range arena.MeteoForecasts {
			if m.Kind == "sandstorm" {
				if distSq(pos, m.Position) <= m.Radius*m.Radius || distSq(pos, m.NextPosition) <= m.Radius*m.Radius {
					return false
				}
			}
			if m.Kind == "earthquake" && m.TurnsUntil <= 2 {
				if distSq(pos, m.Position) <= m.Radius*m.Radius {
					return false
				}
			}
		}
		return true
	}

	// 2. КОНКУРЕНТНОЕ ФОРМИРОВАНИЕ ЗАДАЧ
	type targetTask struct {
		name     string
		pos      []int
		needed   int
		assigned int
		priority int
	}

	var (
		tasks   []targetTask
		tasksMu sync.Mutex
		wg      sync.WaitGroup
	)

	// Задача А: Достройка (Prio 1500+)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var local []targetTask
		for _, constr := range arena.Construction {
			if !isSafe(constr.Position) {
				continue
			}
			needed, prio := 5, 1500
			if constr.Progress >= 90 {
				needed, prio = 50, 2000
			}
			local = append(local, targetTask{"Finish", constr.Position, needed, 0, prio})
		}
		tasksMu.Lock()
		tasks = append(tasks, local...)
		tasksMu.Unlock()
	}()

	// Задача Б: Охота и Саботаж (Prio 400-700)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var local []targetTask
		for _, bvr := range arena.Beavers {
			local = append(local, targetTask{"Hunt", bvr.Position, 4, 0, 400})
		}
		for _, enemy := range arena.Enemy {
			local = append(local, targetTask{"Sabotage", enemy.Position, 4, 0, 700})
		}
		tasksMu.Lock()
		tasks = append(tasks, local...)
		tasksMu.Unlock()
	}()

	// Задача В: Экспансия и Побег (Prio 1-500)
	limit := b.getMaxPlantations(arena)
	currentCount := len(arena.Plantations) + len(arena.Construction)
	if currentCount < limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			expansionOffsets := [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}}
			if currentCount == 1 {
				expansionOffsets = [][]int{{-1, 0}, {1, 0}}
			} else if currentCount == 2 {
				expansionOffsets = [][]int{{0, -1}, {0, 1}}
			}

			dedup := make(map[string]targetTask)

			// Fill expansion candidates
			for _, p := range arena.Plantations {
				if p.IsIsolated {
					continue
				}
				for _, off := range expansionOffsets {
					n := []int{p.Position[0] + off[0], p.Position[1] + off[1]}
					key := fmt.Sprintf("%d,%d", n[0], n[1])
					if _, ok := dedup[key]; !ok && !fastIsOccupied(n) && isSafe(n) {
						d := manhattanDistance(n, mainPlantation.Position)
						prio := d
						for _, neighborsOff := range [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
							if b.isOurControl(arena, []int{n[0] + neighborsOff[0], n[1] + neighborsOff[1]}) {
								prio += 100
							}
						}
						dedup[key] = targetTask{"Expansion", n, 4, 0, prio}
					}
				}
			}

			tasksMu.Lock()
			for _, t := range dedup {
				tasks = append(tasks, t)
			}
			tasksMu.Unlock()
		}()
	}

	wg.Wait()

	// 3. СОРТИРОВКА
	for i := 0; i < len(tasks); i++ {
		for j := i + 1; j < len(tasks); j++ {
			if tasks[j].priority > tasks[i].priority {
				tasks[i], tasks[j] = tasks[j], tasks[i]
			}
		}
	}

	// 4. РАСПРЕДЕЛЕНИЕ (Слой за слоем)
	maxActions := 50
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

				// Клетка никогда не действует на саму себя
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
