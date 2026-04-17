package bot

import (
	"fmt"
	"piratesbot/internal/api"
	"time"
)

func (b *Bot) loop() {
	ticker := time.NewTicker(300 * time.Millisecond) // Poll frequently to catch turn change
	defer ticker.Stop()

	lastProcessedTurn := -1

	for {
		select {
		case <-b.stopCh:
			return
		case <-ticker.C:
		}

		arena, err := b.client.GetArena()
		if err != nil {
			// Log sparingly if error persists
			continue
		}

		b.state.ArenaLock.Lock()
		b.state.Arena = arena
		b.state.TurnNo = arena.TurnNo
		b.state.ArenaLock.Unlock()

		if arena.TurnNo == lastProcessedTurn {
			continue // Already processed this turn
		}

		b.processTurn(arena)
		lastProcessedTurn = arena.TurnNo
	}
}

func (b *Bot) processTurn(arena *api.PlayerResponse) {
	if len(arena.Plantations) == 0 {
		b.Log(fmt.Sprintf("Turn %d. We have 0 plantations!", arena.TurnNo))
		return
	}

	b.mu.Lock()
	strat := b.state.Strategy
	b.mu.Unlock()

	cmd := api.PlayerCommand{}

	// Upgrade logic
	points := arena.PlantationUpgrades.Points
	if points > 0 {
		// Try to max settlement limit first
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

	// Calculate moves based on strategy
	switch strat {
	case StrategyExpansion:
		cmd.Command = b.computeExpansion(arena)
	case StrategyAttack:
		cmd.Command = b.computeAttack(arena)
	}

	if len(cmd.Command) > 0 || cmd.PlantationUpgrade != "" || len(cmd.RelocateMain) > 0 {
		err := b.client.PostCommand(cmd)
		if err != nil {
			b.Log(fmt.Sprintf("Err sending cmd: %v", err))
		} else {
			b.Log(fmt.Sprintf("Sent %d commands", len(cmd.Command)))
		}
	}
}

// Very simple expansion: pick a random plantation, find nearest empty space, try to build
func (b *Bot) computeExpansion(arena *api.PlayerResponse) []api.PlantationAction {
	var actions []api.PlantationAction

	// Limit construction to not spam API and not lose existing plantations randomly
	// Normally we want an advanced logic here
	if len(arena.Plantations) >= b.getMaxPlantations(arena) {
		return actions
	}

	for _, p := range arena.Plantations {
		// Find adjacent empty cells
		neighbors := [][]int{
			{p.Position[0] - 1, p.Position[1]},
			{p.Position[0] + 1, p.Position[1]},
			{p.Position[0], p.Position[1] - 1},
			{p.Position[0], p.Position[1] + 1},
		}

		for _, n := range neighbors {
			if !b.isOccupied(arena, n) {
				// Construct
				actions = append(actions, api.PlantationAction{
					Path: [][]int{p.Position, p.Position, n},
				})
				// Just 1 expansion per turn for now for simplicity
				return actions
			}
		}
	}

	return actions
}

func (b *Bot) computeAttack(arena *api.PlayerResponse) []api.PlantationAction {
	var actions []api.PlantationAction
	// TODO: target enemy or beavers
	return actions
}

func (b *Bot) getMaxPlantations(arena *api.PlayerResponse) int {
	for _, t := range arena.PlantationUpgrades.Tiers {
		if t.Name == "settlement_limit" {
			// Base is 30, each upgrade +1. Let's just trust current level or logic.
			// Actually max limit defaults to 30.
			return 30 + t.Current
		}
	}
	return 30
}

func (b *Bot) isOccupied(arena *api.PlayerResponse, pos []int) bool {
	// check mountains, walls, other plantations
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
	for _, c := range arena.Cells {
		if c.Position[0] == pos[0] && c.Position[1] == pos[1] && c.TerraformationProgress >= 100 {
			// Actually cell is ok to build on unless there's a plantation
		}
	}
	if pos[0] < 0 || pos[1] < 0 || pos[0] >= arena.Size[0] || pos[1] >= arena.Size[1] {
		return true // out of bounds
	}
	return false
}
