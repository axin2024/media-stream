param(
    [Parameter(Mandatory)][string]$Fixture,
    [Parameter(Mandatory)][string]$FFmpeg,
    [Parameter(Mandatory)][string]$MySQLBin,
    [Parameter(Mandatory)][int]$MySQLPort,
    [Parameter(Mandatory)][string]$MySQLVersion,
    [Parameter(Mandatory)][string]$EvidenceDirectory,
    [string]$PreviousCLI,
    [string]$Node
)

$ErrorActionPreference = 'Stop'
$fixture = (Resolve-Path -LiteralPath $Fixture).Path
$evidence = (New-Item -ItemType Directory -Path $EvidenceDirectory -Force).FullName
$binary = Join-Path $fixture 'media-stream.exe'
$liveBinary = if ($PreviousCLI) { $PreviousCLI } else { $binary }
$store = Join-Path $fixture 'broadcast'
$restoredStore = Join-Path $fixture 'restored'
$playerEvidence = Join-Path $evidence 'browser.json'
$processes = [System.Collections.Generic.List[System.Diagnostics.Process]]::new()
$originalPush = $env:MEDIA_STREAM_PUSH_TOKEN
$originalDSN = $env:MEDIA_STREAM_MYSQL_DSN

function Get-Port {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try { return $listener.LocalEndpoint.Port } finally { $listener.Stop() }
}

function Start-RehearsalProcess([string]$Name, [string]$File, [string[]]$Arguments, [hashtable]$Environment = @{}) {
    $quoted = foreach ($arg in $Arguments) {
        if ($arg -match '[\s"]') { '"' + $arg.Replace('"', '\"') + '"' } else { $arg }
    }
    $process = Start-Process -FilePath $File -ArgumentList $quoted -PassThru -WindowStyle Hidden -Environment $Environment `
        -RedirectStandardOutput (Join-Path $evidence "$Name.stdout.log") -RedirectStandardError (Join-Path $evidence "$Name.stderr.log")
    $processes.Add($process)
    return $process
}

function Invoke-Media([string]$Base, [string]$Token, [string]$Method, [string]$Path, $Body, [string]$Key = '') {
    $headers = @{ Authorization = "Bearer $Token" }
    if ($Key) { $headers['Idempotency-Key'] = $Key }
    $request = @{ Uri = "$Base$Path"; Method = $Method; Headers = $headers; TimeoutSec = 15 }
    if ($null -ne $Body) {
        $request.Body = ($Body | ConvertTo-Json -Depth 8 -Compress)
        $request.ContentType = 'application/json'
    }
    return Invoke-RestMethod @request
}

function Wait-Until([string]$Name, [int]$Seconds, [scriptblock]$Read) {
    $stop = [DateTimeOffset]::UtcNow.AddSeconds($Seconds)
    do {
        $result = & $Read
        if ($result) { return $result }
        Start-Sleep -Milliseconds 300
    } while ([DateTimeOffset]::UtcNow -lt $stop)
    throw "$Name did not reach the expected state"
}

function Wait-Ready([string]$Base, [System.Diagnostics.Process]$Process) {
    Wait-Until 'Origin' 16 {
        if ($Process.HasExited) { throw 'Origin exited early; inspect isolated logs' }
        try { (Invoke-RestMethod "$Base/readyz" -TimeoutSec 1).status -eq 'ready' } catch { $false }
    } | Out-Null
}

function Stop-Isolated([System.Diagnostics.Process]$Process) {
    if ($Process -and !$Process.HasExited) {
        Stop-Process -InputObject $Process
        [void]$Process.WaitForExit(5000)
    }
}

function Get-MediaFile([string]$Base, [string]$Token, [string]$Path, [string]$Target) {
    $result = Invoke-WebRequest "$Base$Path" -Headers @{ Authorization = "Bearer $Token" } -TimeoutSec 20 -OutFile $Target -PassThru
    if ($result.StatusCode -ne 200) { throw "Media request failed for $Path" }
    return (Get-FileHash -LiteralPath $Target -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Read-Manifest([string]$Url, [string]$Token) {
    $response = Invoke-WebRequest $Url -Headers @{ Authorization = "Bearer $Token" } -TimeoutSec 10
    if ($response.Content -is [byte[]]) { return [Text.Encoding]::UTF8.GetString($response.Content) }
    return [string]$response.Content
}

try {
    go build -trimpath -o $binary ./cmd/media-stream
    if ($LASTEXITCODE -ne 0) { throw 'Cannot build the rehearsal executable' }
    $previousVersion = if ($PreviousCLI) { & $PreviousCLI version } else { $null }
    & $liveBinary migrate
    if ($LASTEXITCODE -ne 0) { throw 'Cannot migrate the rehearsal database' }
    & $liveBinary node -name rehearsal -slots 2
    if ($LASTEXITCODE -ne 0) { throw 'Cannot register the rehearsal worker' }
    $owner = (& $liveBinary grant -tenant rehearsal -name producer -role owner | ConvertFrom-Json).secret
    $viewer = (& $liveBinary grant -tenant rehearsal -name viewer -role viewer | ConvertFrom-Json).secret
    $outsider = (& $liveBinary grant -tenant outside -name viewer -role viewer | ConvertFrom-Json).secret
    $port = Get-Port
    $origin = "http://127.0.0.1:$port"
    $hostProcess = Start-RehearsalProcess 'origin' $liveBinary @('serve','-listen',"127.0.0.1:$port",'-media',$store,'-ffmpeg',$FFmpeg,'-threads','1')
    $worker = Start-RehearsalProcess 'worker' $liveBinary @('worker','-node','rehearsal','-concurrency','2','-media',$store,'-ffmpeg',$FFmpeg,'-threads','2')
    Wait-Ready $origin $hostProcess
    $now = [DateTimeOffset]::UtcNow
    Invoke-Media $origin $owner 'POST' '/api/channels' @{
        key='rehearsal'; title='Rehearsal Studio'; available_from=$now.AddMinutes(-1).ToString('o'); available_until=$now.AddHours(1).ToString('o')
    } | Out-Null
    $opened = Invoke-Media $origin $owner 'POST' '/api/channels/rehearsal/sessions' @{
        preset='standard'; segment_ms=4000; primary_source_zero_ms=0; backup_source_zero_ms=0; push_ttl_seconds=900
    } 'rehearsal-opening'
    $session = $opened.session.id
    $originRoot = "/api/sessions/$session"
    $source = Join-Path $fixture 'source.mp4'
    $primarySource = Join-Path $fixture 'primary.mp4'
    & $binary sample -ffmpeg $FFmpeg -out $source -seconds 120
    if ($LASTEXITCODE -ne 0) { throw 'Cannot create the synthetic media sample' }
    & $FFmpeg -v error -nostdin -n -i $source -t 45 -c copy $primarySource
    if ($LASTEXITCODE -ne 0) { throw 'Cannot create the 45-second primary input' }
    $browser = $null
    if ($Node) {
        $browser = Start-RehearsalProcess 'browser' $Node @((Join-Path $PSScriptRoot 'browser-rehearsal.cjs')) @{
            MEDIA_REHEARSAL_ORIGIN=$origin; MEDIA_REHEARSAL_VIEWER=$viewer; MEDIA_REHEARSAL_OUTPUT=$playerEvidence
        }
    }
    $pusherArgs = @('push','-origin',$origin,'-session',$session,'-segment-ms','4000','-ffmpeg',$FFmpeg,'-threads','1','-source')
    $starting = [DateTimeOffset]::UtcNow
    $primary = Start-RehearsalProcess 'primary' $liveBinary ($pusherArgs + @($primarySource,'-notify-lost')) @{ MEDIA_STREAM_PUSH_TOKEN=$opened.inputs.primary.secret }
    $backup = Start-RehearsalProcess 'backup' $liveBinary ($pusherArgs + @($source)) @{ MEDIA_STREAM_PUSH_TOKEN=$opened.inputs.backup.secret }
    $switch = Wait-Until 'Backup input switch' 83 {
        if (($primary.HasExited -and $primary.ExitCode -ne 0) -or ($backup.HasExited -and $backup.ExitCode -ne 0)) {
            throw 'A source exited unsuccessfully; inspect isolated push logs'
        }
        if ($worker.HasExited) { throw 'The FFmpeg node exited; inspect isolated logs' }
        $current = Invoke-Media $origin $owner 'GET' $originRoot $null
        if ($current.active_source -eq 'backup' -and $current.state -eq 'live') { $current }
    }
    $seenLive = [DateTimeOffset]::UtcNow
    $liveManifest = Read-Manifest "$origin/media/live/$session/360p.m3u8" $viewer
    if ($liveManifest -notmatch '(?m)^#EXT-X-DISCONTINUITY$') { throw 'Live switch lacks a discontinuity marker' }
    Wait-Until 'End of backup source' 110 { $backup.HasExited } | Out-Null
    if ($backup.ExitCode -ne 0 -or !$primary.HasExited -or $primary.ExitCode -ne 0) { throw 'Source encoders did not finish cleanly' }
    $primaryExit = [DateTimeOffset]$primary.ExitTime
    Invoke-Media $origin $owner 'POST' "$originRoot/end" $null | Out-Null
    Wait-Until 'Archive' 30 {
        (Invoke-Media $origin $owner 'GET' $originRoot $null).state -eq 'archived'
    } | Out-Null
    Invoke-Media $origin $owner 'POST' "$originRoot/end" $null | Out-Null
    $recording = Invoke-Media $origin $viewer 'GET' "/api/recordings/$session" $null
    if ($recording.state -ne 'published' -or $recording.duration_ms -lt 110000) { throw 'Unexpected finalized recording duration' }
    $recordingList = Invoke-Media $origin $viewer 'GET' '/api/recordings' $null
    if (@($recordingList.items | Where-Object id -eq $session).Count -ne 1) { throw 'Repeat end created more than one recording' }
    $events = Invoke-Media $origin $owner 'GET' "$originRoot/events" $null
    $loss = $events | Where-Object { $_.kind -eq 'input_lost' -and $_.details.source -eq 'primary' } | Select-Object -First 1
    $selected = $events | Where-Object { $_.kind -eq 'input_selected' -and $_.details.source -eq 'backup' } | Select-Object -First 1
    $first = $events | Where-Object { $_.kind -eq 'segment_committed' -and $_.details.source -eq 'backup' } | Select-Object -First 1
    if (!$loss -or !$selected -or !$first) { throw 'Missing main failure and standby publication receipts' }
    $recoverMS = ([DateTimeOffset]$first.at - [DateTimeOffset]$loss.at).TotalMilliseconds
    if ($recoverMS -lt 0 -or $recoverMS -gt 12000) { throw "Source failure-to-published time $recoverMS ms missed the goal" }
    $index = Invoke-Media $origin $viewer 'GET' "$originRoot/segments" $null
    $groups = @($index.items | Group-Object sequence | Sort-Object { [int]$_.Name })
    if ($groups.Count -ne $index.watermark -or @($groups | Where-Object Count -ne 2).Count -ne 0) { throw 'Archive lacks matching variants' }
    for ($sequence = 0; $sequence -lt $groups.Count; $sequence++) {
        if ([int]$groups[$sequence].Name -ne $sequence) { throw 'Archive has an unexpected sequence gap' }
    }
    $captionFile = (Resolve-Path 'examples/rehearsal-en.vtt').Path
    $caption = Invoke-RestMethod "$origin/api/recordings/$session/captions/en" -Method Post `
        -Headers @{ Authorization = "Bearer $owner"; 'Idempotency-Key' = 'rehearsal-caption' } -ContentType 'text/vtt' -InFile $captionFile -TimeoutSec 10
    $editRequest = @{ recording_revision=$recording.revision; from_ms=20000; to_ms=40000; caption_id=$caption.id }
    $edit = Invoke-Media $origin $owner 'POST' "/api/recordings/$session/edits" $editRequest 'rehearsal-clip'
    $replay = Invoke-Media $origin $owner 'POST' "/api/recordings/$session/edits" $editRequest 'rehearsal-clip'
    if ($edit.id -ne $replay.id) { throw 'Clip retry has a different ID' }
    $delivered = Wait-Until '20–40 second clip' 50 {
        $value = Invoke-Media $origin $owner 'GET' "/api/edits/$($edit.id)" $null
        if ($value.state -eq 'failed') { throw 'Clip rendering exhausted retries' }
        if ($value.state -eq 'published') { $value }
    }
    if ($delivered.actual_from_ms -ne 20000 -or $delivered.actual_to_ms -ne 40000) { throw 'Unexpected submitted clip range' }
    $clipPath = "/media/clip/$($edit.id)"
    $clipFile = Join-Path $fixture 'clip.mp4'
    $clipSHA = Get-MediaFile $origin $viewer "$clipPath/video.mp4" $clipFile
    & $FFmpeg -v error -xerror -nostdin -threads 1 -i $clipFile -f null -
    if ($LASTEXITCODE -ne 0) { throw 'Delivered clip failed to decode' }
    $cueFile = Join-Path $evidence 'clip-en.vtt'
    $cueSHA = Get-MediaFile $origin $viewer "$clipPath/subtitles.vtt" $cueFile
    $cues = Get-Content -LiteralPath $cueFile -Raw
    if (!$cues.Contains('00:00:00.000 --> 00:00:03.500') -or !$cues.Contains('00:00:19.000 --> 00:00:20.000')) {
        throw 'Delivered VTT did not trim and rebase both boundaries'
    }
    $outside = Invoke-WebRequest "$origin$clipPath/video.mp4" -Headers @{ Authorization = "Bearer $outsider" } -SkipHttpErrorCheck -TimeoutSec 10
    if ($outside.StatusCode -ne 404) { throw 'Cross-tenant clip read was visible' }
    $invalid = Invoke-WebRequest "$origin/api/recordings/$session/edits" -Method Post -ContentType 'application/json' `
        -Body (@{ recording_revision=$recording.revision; from_ms=40000; to_ms=20000; caption_id=$caption.id } | ConvertTo-Json -Compress) `
        -Headers @{ Authorization = "Bearer $owner"; 'Idempotency-Key' = 'rehearsal-invalid-edit' } -SkipHttpErrorCheck -TimeoutSec 10
    if ($invalid.StatusCode -ne 400) { throw 'Reversed clip bounds were accepted' }
    $player = $null
    if ($browser) {
        Wait-Until 'Headless player and two VOD seeks' 65 { $browser.HasExited } | Out-Null
        if ($browser.ExitCode -ne 0 -or !(Test-Path -LiteralPath $playerEvidence)) { throw 'Browser rehearsal did not complete; inspect browser logs' }
        $player = Get-Content -LiteralPath $playerEvidence -Raw | ConvertFrom-Json
    }
    $quality = Invoke-Media $origin $owner 'GET' "$originRoot/quality" $null
    $vodManifest = Read-Manifest "$origin/media/vod/$session/360p.m3u8" $viewer
    if ($vodManifest -notmatch '(?m)^#EXT-X-ENDLIST$') { throw 'VOD lacks a final marker' }
    $vodAsset = "/media/vod/$session/parts/360p/$($first.details.seq).ts"
    $backupSHA = Get-MediaFile $origin $viewer $vodAsset (Join-Path $fixture 'backup-original.ts')
    Stop-Isolated $worker
    if ($PreviousCLI) {
        Stop-Isolated $hostProcess
        & $binary migrate
        if ($LASTEXITCODE -ne 0) { throw 'Could not upgrade the previous release database' }
        $hostProcess = Start-RehearsalProcess 'upgraded' $binary @('serve','-listen',"127.0.0.1:$port",'-media',$store,'-ffmpeg',$FFmpeg,'-threads','1','-schedule=false')
        Wait-Ready $origin $hostProcess
        $upgraded = Invoke-Media $origin $viewer 'GET' "/api/recordings/$session" $null
        $upgradedClip = Get-MediaFile $origin $viewer "$clipPath/video.mp4" (Join-Path $fixture 'upgraded.mp4')
        $upgradedCue = Get-MediaFile $origin $viewer "$clipPath/subtitles.vtt" (Join-Path $fixture 'upgraded.vtt')
        $upgradedBackup = Get-MediaFile $origin $viewer $vodAsset (Join-Path $fixture 'upgraded.ts')
        if ($upgraded.id -ne $recording.id -or $upgraded.revision -ne $recording.revision -or
            $upgradedClip -ne $clipSHA -or $upgradedCue -ne $cueSHA -or $upgradedBackup -ne $backupSHA) {
            throw 'Upgrade changed an existing recording, clip, subtitle, or source fragment'
        }
    }
    $batchPath = '/api/visibility-batches/rehearsal-preserved-selection'
    $batchInput = @{
        hide=$false; visible_until=[DateTimeOffset]::UtcNow.AddHours(2).ToString('o')
        items=@(@{recording_id=$recording.id;expected_revision=$recording.revision}, @{recording_id='ffffffffffffffffffffffff';expected_revision=1})
    }
    $pending = Invoke-Media $origin $owner 'PUT' $batchPath $batchInput
    if ($pending.state -ne 'prepared' -or $pending.processed -ne 0) { throw 'The visibility selection was executed early' }
    Stop-Isolated $hostProcess

    $dump = Join-Path $fixture 'backup.sql'
    $dbArgs = @('--no-defaults','--protocol=tcp','--host=127.0.0.1',"--port=$MySQLPort",'--user=test_media')
    & (Join-Path $MySQLBin 'mysqldump.exe') @dbArgs --single-transaction --hex-blob --no-tablespaces "--result-file=$dump" media_stream
    if ($LASTEXITCODE -ne 0) { throw 'Rehearsal database backup failed' }
    & (Join-Path $MySQLBin 'mysql.exe') @dbArgs --database=media_restore "--execute=source $($dump.Replace('\','/'))"
    if ($LASTEXITCODE -ne 0) { throw 'Rehearsal database restore failed' }
    Copy-Item -LiteralPath $store -Destination $restoredStore -Recurse
    $restorePort = Get-Port
    $restoredOrigin = "http://127.0.0.1:$restorePort"
    $restoredDSN = $originalDSN -replace '/media_stream$', '/media_restore'
    $restoredHost = Start-RehearsalProcess 'restore' $binary @('serve','-listen',"127.0.0.1:$restorePort",'-media',$restoredStore,
        '-ffmpeg',$FFmpeg,'-threads','1','-schedule=false') @{ MEDIA_STREAM_MYSQL_DSN=$restoredDSN }
    Wait-Ready $restoredOrigin $restoredHost
    $restoredRecord = Invoke-Media $restoredOrigin $viewer 'GET' "/api/recordings/$session" $null
    if ($restoredRecord.id -ne $recording.id -or $restoredRecord.revision -ne $recording.revision) { throw 'Restored recording changed identity or revision' }
    $restoredClip = Get-MediaFile $restoredOrigin $viewer "$clipPath/video.mp4" (Join-Path $fixture 'restored.mp4')
    $restoredCue = Get-MediaFile $restoredOrigin $viewer "$clipPath/subtitles.vtt" (Join-Path $fixture 'restored.vtt')
    $restoredBackup = Get-MediaFile $restoredOrigin $viewer $vodAsset (Join-Path $fixture 'backup-restored.ts')
    if ($restoredClip -ne $clipSHA -or $restoredCue -ne $cueSHA -or $restoredBackup -ne $backupSHA) { throw 'Restored media SHA-256 mismatch' }
    $restoredBatch = Invoke-Media $restoredOrigin $owner 'GET' $batchPath $null
    if ($restoredBatch.id -ne $pending.id -or $restoredBatch.state -ne 'prepared' -or $restoredBatch.items.Count -ne 2) {
        throw 'Restoration lost the prepared visibility batch or its selection'
    }
    $applied = Invoke-Media $restoredOrigin $owner 'POST' "$batchPath/execute" $null
    if ($applied.items[0].applied_revision -ne ($recording.revision + 1) -or $applied.items[1].reason -ne 'not_found') {
        throw 'Unexpected restoration batch outcome'
    }
    $batchBrowser = $null
    if ($Node) {
        $batchEvidence = Join-Path $evidence 'visibility-browser.json'
        $batchBrowserProcess = Start-RehearsalProcess 'visibility-browser' $Node @((Join-Path $PSScriptRoot 'browser-visibility.cjs')) @{
            MEDIA_REHEARSAL_ORIGIN=$restoredOrigin; MEDIA_REHEARSAL_EDITOR=$owner; MEDIA_REHEARSAL_OUTPUT=$batchEvidence
        }
        Wait-Until 'Browser reconnect and visibility receipt' 35 { $batchBrowserProcess.HasExited } | Out-Null
        if ($batchBrowserProcess.ExitCode -ne 0 -or !(Test-Path -LiteralPath $batchEvidence)) {
            throw 'Browser visibility rehearsal failed; inspect isolated logs'
        }
        $batchBrowser = Get-Content -LiteralPath $batchEvidence -Raw | ConvertFrom-Json
        $hidden = Invoke-WebRequest "$restoredOrigin/media/vod/$session/index.m3u8" -Headers @{ Authorization = "Bearer $viewer" } -SkipHttpErrorCheck -TimeoutSec 10
        if ($hidden.StatusCode -ne 404) { throw 'Browser visibility operation did not remove the source from public playback' }
        $stillDelivered = Get-MediaFile $restoredOrigin $viewer "$clipPath/video.mp4" (Join-Path $fixture 'after-hide.mp4')
        if ($stillDelivered -ne $clipSHA) { throw 'Browser visibility operation affected the independently released clip' }
    }

    $cpu = Get-CimInstance Win32_Processor | Select-Object -First 1 Name, NumberOfCores, NumberOfLogicalProcessors
    $result = [ordered]@{
        session=$session; mysql=$MySQLVersion; ffmpeg=(& $FFmpeg -version | Select-Object -First 1)
        go=(& go version); cpu=$cpu; width=1280; height=720; source_ms=120000; planned_cut_ms=45000; fragment_ms=4000
        input_start=$starting.ToString('o'); primary_exit=$primaryExit.ToUniversalTime().ToString('o')
        loss_at=$loss.at; selection_at=$selected.at; first_backup_at=$first.at; live_seen_at=$seenLive.ToString('o')
        recovery_ms=[Math]::Round($recoverMS,3); first_backup_sequence=$first.details.seq; first_backup_gap_ms=$first.details.gap_ms
        source_epoch=$switch.source_epoch; sequences=$groups.Count; archived_duration_ms=$recording.duration_ms
        variants=@('360p','720p'); quality=$quality; browser_observed=[bool]$player; clip_id=$edit.id
        clip_actual_from_ms=$delivered.actual_from_ms; clip_actual_to_ms=$delivered.actual_to_ms
        clip_sha256=$clipSHA; vtt_sha256=$cueSHA; backup_fragment_sha256=$backupSHA; restore_verified=$true
        upgraded_from=$previousVersion; upgrade_verified=[bool]$PreviousCLI; reverse_clip_status=$invalid.StatusCode
        restored_batch=@{ request_key=$applied.request_key; processed=$applied.processed; items=$applied.items }
        visibility_browser_verified=[bool]$batchBrowser
    }
    $result | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath (Join-Path $evidence 'rehearsal.json') -Encoding utf8
    $vodManifest | Set-Content -LiteralPath (Join-Path $evidence 'vod.m3u8') -Encoding utf8
    $index.items | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $evidence 'media-index.json') -Encoding utf8
    $result | ConvertTo-Json -Depth 10
} finally {
    foreach ($process in $processes) { Stop-Isolated $process }
    [Environment]::SetEnvironmentVariable('MEDIA_STREAM_PUSH_TOKEN', $originalPush, 'Process')
}
