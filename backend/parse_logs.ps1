$files = Get-ChildItem 'd:\!PROGRAMMING\!pirates\backend\logs\turn_*.json' | Sort-Object { [int]($_.BaseName -replace 'turn_','') }

foreach ($f in $files) {
    $j = Get-Content $f.FullName -Raw | ConvertFrom-Json
    $a = $j.received_arena
    $turnNo = $a.turnNo
    if ($turnNo -lt 144 -or $turnNo -gt 399) { continue }

    $plants = $a.plantations.Count
    
    $constrList = @()
    if ($a.construction) {
        foreach ($c in $a.construction) {
            $constrList += "[$($c.position[0]),$($c.position[1])]p=$($c.progress)"
        }
    }
    $constrStr = if ($constrList.Count -gt 0) { $constrList -join ";" } else { "-" }

    $cmds = 0
    if ($j.sent_command.command) { $cmds = $j.sent_command.command.Count }

    $upgrade = ""
    if ($j.sent_command.plantationUpgrade) { $upgrade = $j.sent_command.plantationUpgrade }

    $relocate = ""
    if ($j.sent_command.relocateMain) { $relocate = "RELOCATE" }

    $targets = @{}
    if ($j.sent_command.command) {
        foreach ($cmd in $j.sent_command.command) {
            $tgt = "$($cmd.path[1][0]),$($cmd.path[1][1])"
            if (-not $targets.ContainsKey($tgt)) { $targets[$tgt] = 0 }
            $targets[$tgt]++
        }
    }
    $tgtStr = ($targets.GetEnumerator() | Sort-Object Value -Descending | ForEach-Object { "$($_.Key):$($_.Value)" }) -join " "

    $eq = ""
    if ($a.meteoForecasts) {
        foreach ($m in $a.meteoForecasts) {
            if ($m.kind -eq "earthquake") { $eq = "EQ=$($m.turnsUntil)" }
        }
    }

    $plantHP = @()
    if ($a.plantations) {
        foreach ($p in $a.plantations) {
            $main = if ($p.isMain) { "*" } else { "" }
            $plantHP += "$($p.position[0]),$($p.position[1])${main}hp=$($p.hp)"
        }
    }
    $hpStr = $plantHP -join ";"

    Write-Output "T$turnNo | P=$plants | C=$constrStr | Cmds=$cmds | Targets=$tgtStr | $hpStr | $upgrade $relocate $eq"
}
