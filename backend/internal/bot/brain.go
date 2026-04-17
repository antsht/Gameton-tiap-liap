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

		// Умный тайминг до следующего запроса, чтобы не спамить API
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

	// Upgrade logic — smart priority
	if arena.PlantationUpgrades.Points > 0 {
		upgrade := b.chooseBestUpgrade(arena)
		if upgrade != "" {
			cmd.PlantationUpgrade = upgrade
			b.Log(fmt.Sprintf("Buying upgrade: %s", upgrade))
		}
	}

	// --- CU Evacuation ---
	var mainPos []int
	var mainHp int
	for _, p := range arena.Plantations {
		if p.IsMain {
			mainPos = []int{p.Position[0], p.Position[1]}
			mainHp = p.Hp
			break
		}
	}

	if mainPos != nil {
		progress := b.getCellProgress(arena, mainPos)

		// Check for incoming earthquakes
		eqTurns := -1
		for _, m := range arena.MeteoForecasts {
			if m.Kind == "earthquake" {
				eqTurns = m.TurnsUntil
				break
			}
		}

		var mainPlantation *api.Plantation
		for i, p := range arena.Plantations {
			if p.IsMain {
				mainPlantation = &arena.Plantations[i]
				break
			}
		}

		eqImminent := eqTurns >= 0 && eqTurns <= 2
		cuNeedsEqEscape := mainPlantation != nil && eqImminent && (mainPlantation.ImmunityUntilTurn <= arena.TurnNo+eqTurns)

		relocated := false
		if cuNeedsEqEscape {
			b.Log(fmt.Sprintf("EMERGENCY! Earthquake in %d turns. CU lacks immunity! Finding safe harbor...", eqTurns))
			for _, p := range arena.Plantations {
				if p.IsMain || p.IsIsolated {
					continue
				}
				dx := p.Position[0] - mainPos[0]
				dy := p.Position[1] - mainPos[1]
				if dx < 0 { dx = -dx }
				if dy < 0 { dy = -dy }
				if dx+dy != 1 {
					continue
				}
				if p.Hp >= 15 && p.ImmunityUntilTurn > arena.TurnNo+eqTurns {
					cmd.RelocateMain = [][]int{{mainPos[0], mainPos[1]}, {p.Position[0], p.Position[1]}}
					b.Log(fmt.Sprintf("EVACUATE (EQ AVOID!) CU %v→%v (Immune until %d)", mainPos, p.Position, p.ImmunityUntilTurn))
					relocated = true
					break
				}
			}
		}

		// ЭВАКУАЦИЯ: relocateMain работает ТОЛЬКО со смежной (кардинально, расст=1) плантацией!
		// Начинаем искать на 50% — через 10 ходов клетка на 100% и плантация исчезнет.
		if !relocated && (progress >= 50 || mainHp <= 20) {
			bestProg := progress // Ищем ЛЮБУЮ смежную с прогрессом МЕНЬШЕ нашего
			var bestPos []int
			for _, p := range arena.Plantations {
				if p.IsMain || p.IsIsolated {
					continue
				}
				dx := p.Position[0] - mainPos[0]
				dy := p.Position[1] - mainPos[1]
				if dx < 0 { dx = -dx }
				if dy < 0 { dy = -dy }
				if dx+dy != 1 {
					continue
				}
				pProg := b.getCellProgress(arena, p.Position)
				if p.Hp >= 15 && pProg < bestProg {
					bestProg = pProg
					bestPos = []int{p.Position[0], p.Position[1]}
				}
			}
			if bestPos != nil {
				from := []int{mainPos[0], mainPos[1]}
				cmd.RelocateMain = [][]int{from, bestPos}
				b.Log(fmt.Sprintf("EVACUATE CU %v→%v (our=%d%% target=%d%%)", from, bestPos, progress, bestProg))
			}
		}

		// ОБЯЗАТЕЛЬНО: если нет аварийного выхода — строим его!
		// Проверяем есть ли хотя бы одна смежная плантация или стройка рядом с ЦУ
		hasAdjacentEscape := false
		for _, p := range arena.Plantations {
			if p.IsMain { continue }
			dx := p.Position[0] - mainPos[0]
			dy := p.Position[1] - mainPos[1]
			if dx < 0 { dx = -dx }
			if dy < 0 { dy = -dy }
			if dx+dy == 1 {
				pProg := b.getCellProgress(arena, p.Position)
				if pProg < progress { // Есть куда бежать
					hasAdjacentEscape = true
					break
				}
			}
		}
		if !hasAdjacentEscape {
			// Нет выхода! Экстренно начинаем стройку рядом с ЦУ
			for _, offset := range [][]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				n := []int{mainPos[0] + offset[0], mainPos[1] + offset[1]}
				if !b.isOccupied(arena, n) {
					// Проверяем нет ли уже стройки тут
					alreadyBuilding := false
					for _, c := range arena.Construction {
						if c.Position[0] == n[0] && c.Position[1] == n[1] {
							alreadyBuilding = true
							break
						}
					}
					if !alreadyBuilding {
						b.Log(fmt.Sprintf("EMERGENCY BUILD escape route at %v for CU at %v (prog=%d%%)", n, mainPos, progress))
						// Будет построено в computeHiveMind через обычный assign
					}
					break
				}
			}
		}
	}

	cmd.Command = b.computeHiveMind(arena)

	if len(cmd.Command) > 0 || cmd.PlantationUpgrade != "" || len(cmd.RelocateMain) > 0 {
		err := b.client.PostCommand(cmd)
		if err != nil {
			b.Log(fmt.Sprintf("Err sending cmd: %v", err))
		} else {
			if len(cmd.Command) > 0 {
				b.Log(fmt.Sprintf("HiveMind sent %d local commands", len(cmd.Command)))
			}
		}
	}

	// Пишем лог с подробностями хода на диск
	go b.dumpTurn(arena, cmd)
}

func (b *Bot) computeHiveMind(arena *api.PlayerResponse) []api.PlantationAction {
	var actions []api.PlantationAction

	// 1. ОПРЕДЕЛЯЕМ ПУЛ РАБОЧИХ (Плантации)
	type worker struct {
		p api.Plantation
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

	distance := func(p1, p2 []int) int {
		dx := p1[0] - p2[0]
		if dx < 0 { dx = -dx }
		dy := p1[1] - p2[1]
		if dy < 0 { dy = -dy }
		return dx + dy
	}

	buildManhattanPath := func(start, dest []int) [][]int {
		path := [][]int{start}
		curr := []int{start[0], start[1]}
		for curr[0] != dest[0] || curr[1] != dest[1] {
			if curr[0] < dest[0] { curr[0]++ } else if curr[0] > dest[0] { curr[0]-- } else if curr[1] < dest[1] { curr[1]++ } else if curr[1] > dest[1] { curr[1]-- }
			path = append(path, []int{curr[0], curr[1]})
		}
		return path
	}

	// 2. ФОРМИРУЕМ ОЧЕРЕДЬ ЗАДАЧ С ПРИОРИТЕТАМИ
	type targetTask struct {
		name     string
		pos      []int
		needed   int
		assigned int
		priority int
	}
	var tasks []targetTask

	// ПРИОРИТЕТ 1000: Ремонт ЦУ
	if mainPlantation.Hp > 0 && mainPlantation.Hp <= 40 {
		tasks = append(tasks, targetTask{"Repair CU", mainPlantation.Position, 5, 0, 1000})
	}

	// ПРИОРИТЕТ 900: Охота на бобров
	for _, bvr := range arena.Beavers {
		tasks = append(tasks, targetTask{"Hunt Beaver", bvr.Position, 4, 0, 900})
	}

	// ПРИОРИТЕТ 800: Достройка текущих объектов
	for _, constr := range arena.Construction {
		// Динамическое количество рабочих в зависимости от доступности
		needed := 5
		if len(workers) > 20 { needed = 8 }
		tasks = append(tasks, targetTask{"Finish Construction", constr.Position, needed, 0, 800})
	}

	// ПРИОРИТЕТ 700: Саботаж врагов
	for _, enemy := range arena.Enemy {
		tasks = append(tasks, targetTask{"Sabotage Enemy", enemy.Position, 4, 0, 700})
	}

	// ПРИОРИТЕТ 600: Ремонт обычных плантаций
	for _, p := range arena.Plantations {
		if !p.IsMain && !p.IsIsolated && p.Hp > 0 && p.Hp <= 20 {
			tasks = append(tasks, targetTask{"Repair Plantation", p.Position, 2, 0, 600})
		}
	}

	// ПРИОРИТЕТ 1-500: Экспансия (Цепочка)
	limit := b.getMaxPlantations(arena)
	currentCount := len(arena.Plantations) + len(arena.Construction)
	if currentCount < limit {
		// Логика поиска кандидатов (как была раньше)
		isSafeFromSandstorms := func(pos []int) bool {
			for _, m := range arena.MeteoForecasts {
				if m.Kind == "sandstorm" {
					if m.Position != nil && len(m.Position) == 2 {
						dx, dy := pos[0]-m.Position[0], pos[1]-m.Position[1]
						if dx*dx+dy*dy <= m.Radius*m.Radius { return false }
					}
					if m.NextPosition != nil && len(m.NextPosition) == 2 {
						dx, dy := pos[0]-m.NextPosition[0], pos[1]-m.NextPosition[1]
						if dx*dx+dy*dy <= m.Radius*m.Radius { return false }
					}
				}
			}
			return true
		}

		// Аварийный выход (ОДИН)
		if mainPlantation.Hp > 0 {
			hasSafeAdjacent := false
			cuProg := b.getCellProgress(arena, mainPlantation.Position)
			for _, offset := range [][]int{{-1,0},{1,0},{0,-1},{0,1}} {
				n := []int{mainPlantation.Position[0]+offset[0], mainPlantation.Position[1]+offset[1]}
				if b.isUnderConstruction(arena, n) { hasSafeAdjacent = true; break }
				for _, p := range arena.Plantations {
					if p.Position[0] == n[0] && p.Position[1] == n[1] && p.Hp > 0 && b.getCellProgress(arena, p.Position) < cuProg {
						hasSafeAdjacent = true; break
					}
				}
			}
			if !hasSafeAdjacent {
				for _, offset := range [][]int{{-1,0},{1,0},{0,-1},{0,1}} {
					n := []int{mainPlantation.Position[0]+offset[0], mainPlantation.Position[1]+offset[1]}
					if !b.isOccupied(arena, n) && !b.isUnderConstruction(arena, n) && isSafeFromSandstorms(n) {
						tasks = append(tasks, targetTask{"Expansion (Escape)", n, 3, 0, 500})
						break
					}
				}
			}
		}

		// Продолжение цепочки
		for _, p := range arena.Plantations {
			if p.IsIsolated { continue }
			for _, offset := range [][]int{{-1,0},{1,0},{0,-1},{0,1}} {
				n := []int{p.Position[0]+offset[0], p.Position[1]+offset[1]}
				if !b.isOccupied(arena, n) && !b.isUnderConstruction(arena, n) && isSafeFromSandstorms(n) {
					// DEDUPLICATE
					alreadyInTasks := false
					for _, t := range tasks {
						if t.name == "Expansion" && t.pos[0] == n[0] && t.pos[1] == n[1] {
							alreadyInTasks = true; break
						}
					}
					if alreadyInTasks { continue }

					dx, dy := n[0]-mainPlantation.Position[0], n[1]-mainPlantation.Position[1]
					if dx < 0 { dx = -dx }; if dy < 0 { dy = -dy }
					prio := dx + dy
					if n[0]%7 == 0 && n[1]%7 == 0 { prio += 50 }
					
					minBvrDist, minEnmDist := 9999, 9999
					for _, bvr := range arena.Beavers { if d := distance(n, bvr.Position); d < minBvrDist { minBvrDist = d } }
					for _, enm := range arena.Enemy { if d := distance(n, enm.Position); d < minEnmDist { minEnmDist = d } }
					if minBvrDist < 20 { prio += (20-minBvrDist)*10 }
					if minEnmDist < 20 { prio += (20-minEnmDist)*8 }
					
					tasks = append(tasks, targetTask{"Expansion", n, 3, 0, prio})
				}
			}
		}
	}

	// Сортируем задачи по приоритету
	for i := 0; i < len(tasks); i++ {
		for j := i + 1; j < len(tasks); j++ {
			if tasks[j].priority > tasks[i].priority {
				tasks[i], tasks[j] = tasks[j], tasks[i]
			}
		}
	}

	// 3. МНОГОСЛОЙНОЕ РАСПРЕДЕЛЕНИЕ (Pass 1: 100%, Pass 2: degraded...)
	for layer := 0; layer < 5; layer++ { // Допустим, макс 5 команд с одной клетки
		for i := range tasks {
			if tasks[i].assigned >= tasks[i].needed {
				continue
			}
			// Ищем воркеров, у которых в этом слое еще нет команды
			for _, w := range workers {
				if tasks[i].assigned >= tasks[i].needed {
					break
				}
				if w.usedActions != layer {
					continue
				}
				// Нельзя использовать плантацию как цель саму для себя (кроме ремонта)
				if tasks[i].name != "Repair CU" && tasks[i].name != "Repair Plantation" {
					if w.p.Position[0] == tasks[i].pos[0] && w.p.Position[1] == tasks[i].pos[1] {
						continue
					}
				}
				
				if distance(w.p.Position, tasks[i].pos) <= actionRange {
					actions = append(actions, api.PlantationAction{
						Path: buildManhattanPath(w.p.Position, tasks[i].pos),
					})
					w.usedActions++
					tasks[i].assigned++
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

	// 0. Экстренная защита ЦУ от землетрясения
	for _, m := range arena.MeteoForecasts {
		if m.Kind == "earthquake" && m.TurnsUntil <= 3 {
			if t, ok := tierMap["earthquake_mitigation"]; ok && t.Current < t.Max {
				return "earthquake_mitigation"
			}
		}
	}

	// 1. Динамический Signal Range: если рядом бобры или враги, нам нужен Reach!
	hasTargetsNearby := false
	for _, bvr := range arena.Beavers {
		for _, p := range arena.Plantations {
			dx := bvr.Position[0] - p.Position[0]
			dy := bvr.Position[1] - p.Position[1]
			if dx < 0 { dx = -dx }
			if dy < 0 { dy = -dy }
			if dx+dy <= 15 {
				hasTargetsNearby = true
				break
			}
		}
		if hasTargetsNearby { break }
	}
	if !hasTargetsNearby {
		for _, enm := range arena.Enemy {
			for _, p := range arena.Plantations {
				dx := enm.Position[0] - p.Position[0]
				dy := enm.Position[1] - p.Position[1]
				if dx < 0 { dx = -dx }
				if dy < 0 { dy = -dy }
				if dx+dy <= 15 {
					hasTargetsNearby = true
					break
				}
			}
			if hasTargetsNearby { break }
		}
	}

	if hasTargetsNearby {
		if t, ok := tierMap["signal_range"]; ok && t.Current < t.Max {
			return "signal_range"
		}
	}

	// 2. Стратегический приоритет:
	priority := []string{
		"settlement_limit",
		"decay_mitigation",
		"earthquake_mitigation",
		"max_hp",
		"beaver_damage_mitigation",
		"signal_range",
		"repair_power",
		"vision_range",
	}
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
		return true // out of bounds
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
