param(
    [Parameter(Mandatory)][string]$MySQLBin,
    [string]$FFmpeg,
    [ValidateSet('Integration','Rehearsal')][string]$Scenario = 'Integration',
    [string]$Node,
    [string]$EvidenceDirectory = '.local/rehearsal'
)

$ErrorActionPreference = 'Stop'
$MySQLBin = (Resolve-Path -LiteralPath $MySQLBin).Path
if ($FFmpeg) { $FFmpeg = (Resolve-Path -LiteralPath $FFmpeg).Path }
$project = Split-Path $PSScriptRoot -Parent
$fixtureRoot = (New-Item -ItemType Directory -Force -Path (Join-Path $project '.local')).FullName
$name = 'test-' + [guid]::NewGuid().ToString('N')
$fixture = (New-Item -ItemType Directory -Path (Join-Path $fixtureRoot $name)).FullName
$data = (New-Item -ItemType Directory -Path (Join-Path $fixture 'data')).FullName
$init = Join-Path $fixture 'init.sql'
$mysql = Join-Path $MySQLBin 'mysql.exe'
$mysqladmin = Join-Path $MySQLBin 'mysqladmin.exe'
$mysqld = Join-Path $MySQLBin 'mysqld.exe'
$password = [guid]::NewGuid().ToString('N')
$rootPassword = [guid]::NewGuid().ToString('N')
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$listener.Start()
try { $port = $listener.LocalEndpoint.Port } finally { $listener.Stop() }
$server = $null
$envBefore = @{}
$envValues = @{
    MYSQL_PWD = $password
    MEDIA_STREAM_TEST_DSN = "test_media:$password@tcp(127.0.0.1:$port)/media_stream"
    MEDIA_STREAM_TEST_UPGRADE_DSN = "test_media:$password@tcp(127.0.0.1:$port)/media_upgrade"
    MEDIA_STREAM_TEST_HTTP_DSN = "test_media:$password@tcp(127.0.0.1:$port)/media_http"
    MEDIA_STREAM_TEST_FFMPEG = $FFmpeg
    MEDIA_STREAM_MYSQL_DSN = "test_media:$password@tcp(127.0.0.1:$port)/media_stream"
    MEDIA_STREAM_SIGNING_KEY = [guid]::NewGuid().ToString('N') + [guid]::NewGuid().ToString('N')
}
foreach ($variable in $envValues.Keys) {
    $envBefore[$variable] = [Environment]::GetEnvironmentVariable($variable, 'Process')
}
$baseArgs = @('--no-defaults', '--mysqlx=OFF', '--innodb-buffer-pool-size=32M', '--max-connections=48',
    ('--basedir="{0}"' -f (Split-Path $MySQLBin -Parent)), ('--datadir="{0}"' -f $data),
    ('--log-error="{0}/mysqld.log"' -f $fixture))
$clientArgs = @('--no-defaults', '--protocol=TCP', '--host=127.0.0.1', "--port=$port")
$locationChanged = $false
try {
    $initialize = Start-Process -FilePath $mysqld -ArgumentList ($baseArgs + '--initialize-insecure') -Wait -PassThru -WindowStyle Hidden
    if ($initialize.ExitCode -ne 0) { throw 'Local MySQL initialization failed' }
    @"
ALTER USER 'root'@'localhost' IDENTIFIED BY '$rootPassword';
CREATE DATABASE media_stream CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin;
CREATE DATABASE media_upgrade CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin;
CREATE DATABASE media_http CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin;
CREATE DATABASE media_restore CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin;
CREATE USER 'test_media'@'127.0.0.1' IDENTIFIED BY '$password';
GRANT ALL PRIVILEGES ON media_stream.* TO 'test_media'@'127.0.0.1';
GRANT ALL PRIVILEGES ON media_upgrade.* TO 'test_media'@'127.0.0.1';
GRANT ALL PRIVILEGES ON media_http.* TO 'test_media'@'127.0.0.1';
GRANT ALL PRIVILEGES ON media_restore.* TO 'test_media'@'127.0.0.1';
"@ | Set-Content -LiteralPath $init -Encoding ascii
    $argsForServer = $baseArgs + @('--bind-address=127.0.0.1', "--port=$port", ('--init-file="{0}"' -f $init))
    $server = Start-Process -FilePath $mysqld -ArgumentList $argsForServer -PassThru -WindowStyle Hidden
    foreach ($variable in $envValues.Keys) {
        [Environment]::SetEnvironmentVariable($variable, $envValues[$variable], 'Process')
    }
    $version = $null
    for ($attempt = 0; $attempt -lt 40; $attempt++) {
        if ($server.HasExited) { throw 'MySQL exited before accepting connections' }
        $version = & $mysql @clientArgs --user=test_media --database=media_stream --batch --skip-column-names '--execute=SELECT VERSION()' 2>$null
        if ($LASTEXITCODE -eq 0 -and $version) { break }
        Start-Sleep -Milliseconds 250
    }
    if ($version -notmatch '^8\.4\.') { throw 'Expected an isolated MySQL 8.4 server' }
    Remove-Item -LiteralPath $init
    Write-Output "Isolated MySQL $version"
    Push-Location -LiteralPath $project
    $locationChanged = $true
    if ($Scenario -eq 'Integration') {
        go test -count=1 -timeout 4m -v ./...
        if ($LASTEXITCODE -ne 0) { throw 'Media integration tests failed' }
    } else {
        if (!$FFmpeg) { throw 'Rehearsal needs -FFmpeg' }
        & (Join-Path $PSScriptRoot 'rehearsal.ps1') -Fixture $fixture -FFmpeg $FFmpeg -MySQLBin $MySQLBin -MySQLPort $port `
            -Node $Node -EvidenceDirectory $EvidenceDirectory -MySQLVersion $version
    }
} finally {
    if ($locationChanged) { Pop-Location }
    if (Test-Path -LiteralPath $init) { Remove-Item -LiteralPath $init }
    if ($server -and !$server.HasExited) {
        $env:MYSQL_PWD = $rootPassword
        & $mysqladmin @clientArgs --user=root --connect-timeout=2 shutdown 2>$null
        if (!$server.WaitForExit(5000)) { Stop-Process -InputObject $server }
    }
    foreach ($variable in $envBefore.Keys) {
        [Environment]::SetEnvironmentVariable($variable, $envBefore[$variable], 'Process')
    }
    if (Test-Path -LiteralPath $fixture) {
        $absolute = (Resolve-Path -LiteralPath $fixture).Path
        if ((Split-Path -Parent $absolute) -ne $fixtureRoot -or (Split-Path -Leaf $absolute) -ne $name) {
            throw 'Test fixture cleanup refused the resolved path'
        }
        Remove-Item -LiteralPath $absolute -Force -Recurse
    }
}
