package bot

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"piratesbot/internal/api"
)

// Путь к одному последовательному логу (относительно cwd процесса).
const turnLogPath = "logs/game_trace.log"

// dumpTurn пишет снимок dto.PlayerResponse и исходящего command.PlayerDTO в текстовый лог.
// Поля сверены с TASK/openapi.yml (arena + POST /api/command).
func (b *Bot) dumpTurn(arena *api.PlayerResponse, cmd api.PlayerCommand) {
	b.turnLogMu.Lock()
	defer b.turnLogMu.Unlock()

	if err := os.MkdirAll("logs", 0755); err != nil {
		return
	}

	f, err := os.OpenFile(turnLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	b.turnLogOnce.Do(func() {
		fmt.Fprintf(f, "# pirates bot trace v1\n"+
			"# arena: components.schemas.dto.PlayerResponse (TASK/openapi.yml)\n"+
			"# command: components.schemas.command.PlayerDTO\n"+
			"# game rules: TASK/rules.pdf (терраформация, ЦУ, лимиты, катаклизмы)\n"+
			"# session_start_utc=%s\n\n",
			time.Now().UTC().Format(time.RFC3339Nano))
	})

	var sb strings.Builder
	writeTurnSnapshot(&sb, arena, cmd)
	_, _ = f.WriteString(sb.String())
}

func writeTurnSnapshot(sb *strings.Builder, a *api.PlayerResponse, cmd api.PlayerCommand) {
	wall := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(sb, "========== turn=%d wall_utc=%s ==========\n", a.TurnNo, wall)

	w, h := 0, 0
	if len(a.Size) >= 2 {
		w, h = a.Size[0], a.Size[1]
	}
	fmt.Fprintf(sb, "arena nextTurnIn=%.4f size=%dx%d actionRange=%d mountains=%d\n",
		a.NextTurnIn, w, h, a.ActionRange, len(a.Mountains))

	pu := a.PlantationUpgrades
	fmt.Fprintf(sb, "upgrades points=%d turnsUntilPoints=%d maxPoints=%d intervalTurns=%d tiers=",
		pu.Points, pu.TurnsUntilPoints, pu.MaxPoints, pu.IntervalTurns)
	tiers := append([]api.PlantationUpgradeTierItem(nil), pu.Tiers...)
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Name < tiers[j].Name })
	for _, t := range tiers {
		fmt.Fprintf(sb, "%s:%d/%d,", t.Name, t.Current, t.Max)
	}
	sb.WriteString("\n")

	for _, m := range a.MeteoForecasts {
		pos := formatXY(m.Position)
		next := formatXY(m.NextPosition)
		fmt.Fprintf(sb, "meteo kind=%s turnsUntil=%d forming=%t id=%q pos=%s next=%s radius=%d\n",
			m.Kind, m.TurnsUntil, m.Forming, m.Id, pos, next, m.Radius)
	}

	iso := 0
	for _, p := range a.Plantations {
		if p.IsIsolated {
			iso++
		}
	}
	fmt.Fprintf(sb, "counts plantations=%d isolated=%d construction=%d enemy=%d beavers=%d cells=%d commands_sent=%d\n",
		len(a.Plantations), iso, len(a.Construction), len(a.Enemy), len(a.Beavers), len(a.Cells), len(cmd.Command))

	plants := append([]api.Plantation(nil), a.Plantations...)
	sort.Slice(plants, func(i, j int) bool {
		if plants[i].Position[0] != plants[j].Position[0] {
			return plants[i].Position[0] < plants[j].Position[0]
		}
		return plants[i].Position[1] < plants[j].Position[1]
	})
	for _, p := range plants {
		main, isol := 0, 0
		if p.IsMain {
			main = 1
		}
		if p.IsIsolated {
			isol = 1
		}
		boost := cellBoosted(p.Position[0], p.Position[1])
		fmt.Fprintf(sb, "plantation pos=%d,%d main=%d isolated=%d hp=%d immunityUntilTurn=%d boosted=%t id=%s\n",
			p.Position[0], p.Position[1], main, isol, p.Hp, p.ImmunityUntilTurn, boost, p.Id)
	}

	cells := append([]api.TerraformedCell(nil), a.Cells...)
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Position[0] != cells[j].Position[0] {
			return cells[i].Position[0] < cells[j].Position[0]
		}
		return cells[i].Position[1] < cells[j].Position[1]
	})
	for _, c := range cells {
		boost := cellBoosted(c.Position[0], c.Position[1])
		fmt.Fprintf(sb, "cell pos=%d,%d terraformationProgress=%d turnsUntilDegradation=%d boosted=%t\n",
			c.Position[0], c.Position[1], c.TerraformationProgress, c.TurnsUntilDegradation, boost)
	}

	cons := append([]api.Construction(nil), a.Construction...)
	sort.Slice(cons, func(i, j int) bool {
		if cons[i].Position[0] != cons[j].Position[0] {
			return cons[i].Position[0] < cons[j].Position[0]
		}
		return cons[i].Position[1] < cons[j].Position[1]
	})
	for _, c := range cons {
		fmt.Fprintf(sb, "construction pos=%d,%d progress=%d\n", c.Position[0], c.Position[1], c.Progress)
	}

	for _, e := range a.Enemy {
		boost := cellBoosted(e.Position[0], e.Position[1])
		fmt.Fprintf(sb, "enemy pos=%d,%d hp=%d boosted=%t id=%s\n",
			e.Position[0], e.Position[1], e.Hp, boost, e.Id)
	}
	for _, bv := range a.Beavers {
		fmt.Fprintf(sb, "beaver pos=%d,%d hp=%d id=%s\n",
			bv.Position[0], bv.Position[1], bv.Hp, bv.Id)
	}

	if cmd.PlantationUpgrade != "" {
		fmt.Fprintf(sb, "cmd plantationUpgrade=%s\n", cmd.PlantationUpgrade)
	} else {
		sb.WriteString("cmd plantationUpgrade=-\n")
	}
	if len(cmd.RelocateMain) >= 2 && len(cmd.RelocateMain[0]) >= 2 && len(cmd.RelocateMain[1]) >= 2 {
		fmt.Fprintf(sb, "cmd relocateMain from=%d,%d to=%d,%d\n",
			cmd.RelocateMain[0][0], cmd.RelocateMain[0][1],
			cmd.RelocateMain[1][0], cmd.RelocateMain[1][1])
	} else {
		sb.WriteString("cmd relocateMain=-\n")
	}

	byTgt := map[string]int{}
	for _, act := range cmd.Command {
		byTgt[commandTargetKey(act.Path)]++
	}
	keys := make([]string, 0, len(byTgt))
	for k := range byTgt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		sb.WriteString("cmd action_targets=")
		for i, k := range keys {
			if i > 0 {
				sb.WriteString(" ")
			}
			fmt.Fprintf(sb, "%s:%d", k, byTgt[k])
		}
		sb.WriteString("\n")
	}

	for i, act := range cmd.Command {
		sb.WriteString("cmd action[")
		sb.WriteString(fmt.Sprintf("%d] path=", i))
		for j, step := range act.Path {
			if j > 0 {
				sb.WriteString("->")
			}
			if len(step) >= 2 {
				fmt.Fprintf(sb, "%d,%d", step[0], step[1])
			} else {
				sb.WriteString("?")
			}
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
}

func formatXY(p []int) string {
	if len(p) >= 2 {
		return fmt.Sprintf("%d,%d", p[0], p[1])
	}
	return "-"
}

func cellBoosted(x, y int) bool {
	return x%7 == 0 && y%7 == 0
}

func commandTargetKey(path [][]int) string {
	if len(path) < 2 {
		return "?"
	}
	var p []int
	if len(path) > 2 {
		p = path[len(path)-1]
	} else {
		p = path[1]
	}
	if len(p) >= 2 {
		return fmt.Sprintf("%d,%d", p[0], p[1])
	}
	return "?"
}
