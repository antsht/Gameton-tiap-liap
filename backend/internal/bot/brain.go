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
		waitMs := int(arena.NextTurnIn * 1000) + 50
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

	// Upgrade logic
	points := arena.PlantationUpgrades.Points
	if points > 0 {
		upgradeChosen := false
		for _, t := range arena.PlantationUpgrades.Tiers {
			if t.Name == "settlement_limit" && t.Current < t.Max {
				cmd.PlantationUpgrade = t.Name
				upgradeChosen = true
				break
			}
		}
		if !upgradeChosen {
			cmd.PlantationUpgrade = "repair_power"
		}
		b.Log(fmt.Sprintf("Buying upgrade: %s", cmd.PlantationUpgrade))
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
}

func (b *Bot) computeHiveMind(arena *api.PlayerResponse) []api.PlantationAction {
	var actions []api.PlantationAction

	idle := make(map[string]api.Plantation)
	var mainPlantation api.Plantation
	for _, p := range arena.Plantations {
		idle[p.Id] = p
		if p.IsMain {
			mainPlantation = p
		}
	}

	actionRange := arena.ActionRange
	if actionRange == 0 {
		actionRange = 2
	}

	distance := func(p1, p2 []int) int {
		dx := p1[0] - p2[0]
		if dx < 0 { dx = -dx }
		dy := p1[1] - p2[1]
		if dy < 0 { dy = -dy }
		if dx > dy { return dx }
		return dy
	}

	assign := func(target []int, needed int) {
		assigned := 0
		for id, p := range idle {
			if assigned >= needed {
				break
			}
			if distance(p.Position, target) <= actionRange {
				actions = append(actions, api.PlantationAction{
					Path: [][]int{p.Position, p.Position, target},
				})
				delete(idle, id)
				assigned++
			}
		}
	}

	// 1. Defend CU
	if mainPlantation.Hp > 0 && mainPlantation.Hp <= 40 {
		assign(mainPlantation.Position, 5)
	}

	// 2. Hunt Beavers (Prize x10)
	for _, bvr := range arena.Beavers {
		assign(bvr.Position, 4)
	}

	// 3. Finish Constructions
	for _, constr := range arena.Construction {
		assign(constr.Position, 3)
	}

	// 4. Aggressive Expansion
	limit := b.getMaxPlantations(arena)
	currentCount := len(arena.Plantations) + len(arena.Construction)

	if currentCount < limit && len(idle) > 0 {
		candidates := make(map[string][]int)
		for _, p := range arena.Plantations {
			// Find adjacent empty cells
			for _, offset := range [][]int{{-1,0},{1,0},{0,-1},{0,1},{-1,-1},{1,-1},{-1,1},{1,1}} {
				n := []int{p.Position[0] + offset[0], p.Position[1] + offset[1]}
				if !b.isOccupied(arena, n) {
					key := fmt.Sprintf("%d,%d", n[0], n[1])
					candidates[key] = n
				}
			}
		}

		// Prioritize golden cells
		for key, n := range candidates {
			if len(idle) == 0 || currentCount >= limit { break }
			if n[0]%7 == 0 && n[1]%7 == 0 {
				assign(n, 3) 
				currentCount++
				delete(candidates, key)
			}
		}

		// Fill normal cells
		for _, n := range candidates {
			if len(idle) == 0 || currentCount >= limit { break }
			assign(n, 2)
			currentCount++
		}
	}

	return actions
}

func (b *Bot) getMaxPlantations(arena *api.PlayerResponse) int {
	for _, t := range arena.PlantationUpgrades.Tiers {
		if t.Name == "settlement_limit" {
			return 30 + t.Current
		}
	}
	return 30
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
