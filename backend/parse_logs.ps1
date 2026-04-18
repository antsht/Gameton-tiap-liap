$tracePath = Join-Path $PSScriptRoot 'logs/game_trace.log'
if (-not (Test-Path $tracePath)) {
    Write-Error "Trace file not found: $tracePath"
    exit 1
}

$turns = @()
$current = $null

function New-TurnState {
    return [ordered]@{
        turn = -1
        wall = ""
        plantations = 0
        isolated = 0
        constructions = 0
        commands = 0
        enemies = 0
        beavers = 0
        cells = 0
        upgrade = "-"
        relocate = $false
        actionTargets = @{}
        mainPos = $null
        mainHp = -1
        eqTurns = $null
        stormPos = $null
        stormRadius = 0
        constructionProgress = @{}
    }
}

function Parse-ActionTargets([string]$value) {
    $result = @{}
    if ([string]::IsNullOrWhiteSpace($value)) { return $result }
    $parts = $value.Trim().Split(' ', [System.StringSplitOptions]::RemoveEmptyEntries)
    foreach ($p in $parts) {
        $kv = $p.Split(':')
        if ($kv.Count -ne 2) { continue }
        $num = 0
        if ([int]::TryParse($kv[1], [ref]$num)) {
            $result[$kv[0]] = $num
        }
    }
    return $result
}

function Get-FocusMetrics($targets, [int]$totalCmds) {
    $maxTarget = "-"
    $maxCount = 0
    if ($targets.Keys.Count -gt 0) {
        foreach ($k in $targets.Keys) {
            if ($targets[$k] -gt $maxCount) {
                $maxCount = $targets[$k]
                $maxTarget = $k
            }
        }
    }
    $share = 0.0
    if ($totalCmds -gt 0) {
        $share = [math]::Round(($maxCount / $totalCmds), 2)
    }
    [ordered]@{
        maxTarget = $maxTarget
        maxCount = $maxCount
        share = $share
    }
}

$lines = Get-Content $tracePath
foreach ($line in $lines) {
    if ($line -match '^========== turn=(\d+) wall_utc=([^ ]+)') {
        if ($null -ne $current) {
            $turns += [PSCustomObject]$current
        }
        $current = New-TurnState
        $current.turn = [int]$Matches[1]
        $current.wall = $Matches[2]
        continue
    }
    if ($null -eq $current) { continue }

    if ($line -match '^counts plantations=(\d+) isolated=(\d+) construction=(\d+) enemy=(\d+) beavers=(\d+) cells=(\d+) commands_sent=(\d+)') {
        $current.plantations = [int]$Matches[1]
        $current.isolated = [int]$Matches[2]
        $current.constructions = [int]$Matches[3]
        $current.enemies = [int]$Matches[4]
        $current.beavers = [int]$Matches[5]
        $current.cells = [int]$Matches[6]
        $current.commands = [int]$Matches[7]
        continue
    }

    if ($line -match '^cmd plantationUpgrade=(.+)$') {
        $current.upgrade = $Matches[1]
        continue
    }

    if ($line -match '^cmd relocateMain=(.+)$') {
        $current.relocate = ($Matches[1] -ne '-')
        continue
    }

    if ($line -match '^cmd action_targets=(.*)$') {
        $current.actionTargets = Parse-ActionTargets $Matches[1]
        continue
    }

    if ($line -match '^meteo kind=earthquake turnsUntil=(\d+)') {
        $current.eqTurns = [int]$Matches[1]
        continue
    }

    if ($line -match '^meteo kind=sandstorm .* pos=([\-\d]+),([\-\d]+) .* radius=(\d+)') {
        $current.stormPos = @([int]$Matches[1], [int]$Matches[2])
        $current.stormRadius = [int]$Matches[3]
        continue
    }

    if ($line -match '^plantation pos=(\d+),(\d+) main=1 isolated=\d hp=(\d+)') {
        $current.mainPos = @([int]$Matches[1], [int]$Matches[2])
        $current.mainHp = [int]$Matches[3]
        continue
    }

    if ($line -match '^construction pos=(\d+),(\d+) progress=(\d+)') {
        $key = "$($Matches[1]),$($Matches[2])"
        $current.constructionProgress[$key] = [int]$Matches[3]
        continue
    }
}
if ($null -ne $current) {
    $turns += [PSCustomObject]$current
}

if ($turns.Count -eq 0) {
    Write-Error "No turns parsed from $tracePath"
    exit 1
}

$turns = $turns |
    Sort-Object turn, wall |
    Group-Object turn |
    ForEach-Object { $_.Group[-1] } |
    Sort-Object turn

Write-Output "Turn | P | dP | I | C | Cmd | Focus | TopTarget | EQ | EQreact | StormMain | Upg | Reloc | Notes"
Write-Output "-----------------------------------------------------------------------------------------------------"

$isolTurns = 0
$overfocusTurns = 0
$plantLossTotal = 0
$dropToOneTurns = 0
$eqUrgentTurns = 0
$eqReactTurns = 0
$stormMainTurns = 0
$stormNoRelocateTurns = 0
$progressGainTurns = 0
$progressGainTotal = 0

for ($i = 0; $i -lt $turns.Count; $i++) {
    $t = $turns[$i]
    $prev = $null
    if ($i -gt 0) { $prev = $turns[$i - 1] }

    $dP = 0
    if ($prev) { $dP = $t.plantations - $prev.plantations }

    if ($t.isolated -gt 0) { $isolTurns++ }

    $focus = Get-FocusMetrics $t.actionTargets $t.commands
    $overfocus = ($t.commands -ge 4 -and $focus.share -ge 0.75)
    if ($overfocus) { $overfocusTurns++ }

    $loss = 0
    if ($dP -lt 0) {
        $loss = -$dP
        $plantLossTotal += $loss
    }
    if ($prev -and $prev.plantations -gt 1 -and $t.plantations -eq 1) {
        $dropToOneTurns++
    }

    $eq = "-"
    $eqReact = "-"
    if ($null -ne $t.eqTurns) {
        $eq = $t.eqTurns
        if ($t.eqTurns -le 2) {
            $eqUrgentTurns++
            $reacted = ($t.upgrade -eq 'earthquake_mitigation' -or $t.relocate)
            if ($reacted) { $eqReactTurns++ }
            $eqReact = if ($reacted) { 'Y' } else { 'N' }
        }
    }

    $stormMain = "-"
    if ($t.mainPos -and $t.stormPos) {
        $dx = $t.mainPos[0] - $t.stormPos[0]
        $dy = $t.mainPos[1] - $t.stormPos[1]
        $inside = (($dx * $dx + $dy * $dy) -le ($t.stormRadius * $t.stormRadius))
        if ($inside) {
            $stormMain = "IN"
            $stormMainTurns++
            if (-not $t.relocate) { $stormNoRelocateTurns++ }
        } else {
            $stormMain = "OUT"
        }
    }

    $consDelta = 0
    if ($prev) {
        foreach ($k in $t.constructionProgress.Keys) {
            $curProg = $t.constructionProgress[$k]
            $prevProg = 0
            if ($prev.constructionProgress.ContainsKey($k)) {
                $prevProg = $prev.constructionProgress[$k]
            }
            if ($curProg -gt $prevProg) {
                $consDelta += ($curProg - $prevProg)
            }
        }
    }
    if ($consDelta -gt 0) {
        $progressGainTurns++
        $progressGainTotal += $consDelta
    }

    $notes = @()
    if ($overfocus) { $notes += "OVERFOCUS" }
    if ($t.isolated -gt 0) { $notes += "ISOLATED" }
    if ($loss -gt 0) { $notes += "LOSS:$loss" }
    if ($consDelta -gt 0) { $notes += "Build+$consDelta" }
    if ($notes.Count -eq 0) { $notes += "-" }

    $reloc = if ($t.relocate) { 'Y' } else { 'N' }

    $rowFmt = "T{0} | {1} | {2,2} | {3} | {4} | {5} | {6,4:P0} | {7}:{8} | {9} | {10} | {11} | {12} | {13} | {14}"
    Write-Output ($rowFmt -f
        $t.turn, $t.plantations, $dP, $t.isolated, $t.constructions, $t.commands,
        $focus.share, $focus.maxTarget, $focus.maxCount, $eq, $eqReact, $stormMain, $t.upgrade, $reloc, ($notes -join ','))
}

$avgBuildGain = 0.0
if ($progressGainTurns -gt 0) {
    $avgBuildGain = [math]::Round($progressGainTotal / $progressGainTurns, 2)
}

$eqReactRate = 0.0
if ($eqUrgentTurns -gt 0) {
    $eqReactRate = [math]::Round(($eqReactTurns / $eqUrgentTurns) * 100, 1)
}

Write-Output ""
Write-Output "Summary"
Write-Output "-------"
Write-Output "Turns parsed: $($turns.Count) (T$($turns[0].turn)..T$($turns[-1].turn))"
Write-Output "Isolation turns (I>0): $isolTurns"
Write-Output "Overfocus turns (>=75% cmds into one target, cmds>=4): $overfocusTurns"
Write-Output "Plantations lost total (sum of negative dP): $plantLossTotal"
Write-Output "Drops to one plantation (from >1 to 1): $dropToOneTurns"
Write-Output "EQ urgent turns (turnsUntil<=2): $eqUrgentTurns, reacted: $eqReactTurns ($eqReactRate%)"
Write-Output "Main inside storm radius turns: $stormMainTurns, no relocate there: $stormNoRelocateTurns"
Write-Output "Construction positive progress turns: $progressGainTurns, avg gain: $avgBuildGain"
