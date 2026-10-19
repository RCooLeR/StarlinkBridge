#requires -Version 7.0
# Stages sources and configuration into an existing NAS stack; containers are not restarted.
param(
    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$NasRoot,
    [string]$BootstrapCookie = '',
    [string]$SeedCookieState = ''
)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$stackRoot = Join-Path $NasRoot '80-bridges'
$buildRoot = Join-Path $stackRoot 'build\starlink-bridge'
$configRoot = Join-Path $NasRoot 'volumes\80-bridges\starlink-bridge\config'
$dataRoot = Join-Path $NasRoot 'volumes\80-bridges\starlink-bridge\data'
$backupRoot = Join-Path $stackRoot ('backups\starlink-bridge-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
$composePath = Join-Path $stackRoot 'docker-compose.yaml'
$envPath = Join-Path $stackRoot '.env'
$envExamplePath = Join-Path $stackRoot '.env.example'
$promPaths = @(
    (Join-Path $NasRoot 'volumes\60-observability\prometheus\config\prometheus.yml'),
    (Join-Path $NasRoot '60-observability\config\prometheus\prometheus.yml')
)
$utf8 = [Text.UTF8Encoding]::new($false)

if (!(Test-Path -LiteralPath $composePath)) { throw 'NAS bridges compose file is missing.' }
if (!(Test-Path -LiteralPath $envPath)) { throw 'NAS bridges environment file is missing.' }
# Back up all changed configuration first. Cookie contents are never printed.
New-Item -ItemType Directory -Path $backupRoot -Force | Out-Null
Copy-Item -LiteralPath $composePath -Destination (Join-Path $backupRoot 'docker-compose.yaml')
Copy-Item -LiteralPath $envPath -Destination (Join-Path $backupRoot '.env')
if (Test-Path -LiteralPath $envExamplePath) { Copy-Item -LiteralPath $envExamplePath -Destination (Join-Path $backupRoot '.env.example') }
for ($i=0; $i -lt $promPaths.Count; $i++) {
    if (Test-Path -LiteralPath $promPaths[$i]) { Copy-Item -LiteralPath $promPaths[$i] -Destination (Join-Path $backupRoot "prometheus-$i.yml") }
}
foreach ($path in @($buildRoot,$configRoot,$dataRoot)) { New-Item -ItemType Directory -Path $path -Force | Out-Null }

# Explicit allowlist avoids shipping .git, build outputs, cookies or local data.
$sourceBridge = Join-Path $projectRoot 'bridge'
$targetBridge = Join-Path $buildRoot 'bridge'
New-Item -ItemType Directory -Path $targetBridge -Force | Out-Null
Get-ChildItem -LiteralPath $sourceBridge -Recurse -File | Where-Object {
    $_.Name -like '*.go' -or $_.Name -in @('go.mod','go.sum','Dockerfile','.dockerignore')
} | ForEach-Object {
    $relative = [IO.Path]::GetRelativePath($sourceBridge,$_.FullName)
    $target = Join-Path $targetBridge $relative
    New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force | Out-Null
    Copy-Item -LiteralPath $_.FullName -Destination $target -Force
    if ((Get-FileHash -LiteralPath $_.FullName).Hash -ne (Get-FileHash -LiteralPath $target).Hash) { throw 'NAS source copy verification failed.' }
}
foreach ($name in @('README.md','LICENSE','.dockerignore')) { Copy-Item -LiteralPath (Join-Path $projectRoot $name) -Destination (Join-Path $buildRoot $name) -Force }
New-Item -ItemType Directory -Path (Join-Path $buildRoot 'docs') -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $projectRoot 'docs\nas.md') -Destination (Join-Path $buildRoot 'docs\nas.md') -Force

$service = @'
  starlink-bridge:
    container_name: starlink-bridge
    build:
      context: ./build/starlink-bridge/bridge
    pull_policy: build
    user: 1000:10
    restart: unless-stopped
    stop_grace_period: 15s
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    environment:
      STARLINK_MODE: ${STARLINK_MODE:-cloud}
      STARLINK_DEVICE_ID: ${STARLINK_DEVICE_ID:-}
      STARLINK_LOCAL_ADDRESS: ${STARLINK_LOCAL_ADDRESS:-192.168.100.1:9200}
      STARLINK_CLOUD_WEB_HOST: ${STARLINK_CLOUD_WEB_HOST:-starlink.com}
      STARLINK_LISTEN: :9091
      STARLINK_POLL_INTERVAL: ${STARLINK_POLL_INTERVAL:-60s}
      STARLINK_STALE_AFTER: ${STARLINK_STALE_AFTER:-3m}
      STARLINK_COOKIE_REFRESH_INTERVAL: ${STARLINK_COOKIE_REFRESH_INTERVAL:-30m}
      STARLINK_COOKIE_FILE: /app/config/starlink.cookie
      STARLINK_COOKIE_STATE_FILE: /app/data/cookies.json
      STARLINK_MQTT_ENABLED: "true"
      STARLINK_MQTT_BROKER: tcp://${MQTT_HOST:-mosquitto}:1883
      STARLINK_MQTT_USERNAME: ${MQTT_USERNAME:-}
      STARLINK_MQTT_PASSWORD: ${MQTT_PASSWORD:-}
      STARLINK_MQTT_CLIENT_ID: starlink-bridge
      STARLINK_MQTT_TOPIC_PREFIX: starlinkbridge
    volumes:
      - ../volumes/80-bridges/starlink-bridge/config:/app/config:ro
      - ../volumes/80-bridges/starlink-bridge/data:/app/data
    expose:
      - "9091"
    networks:
      ugos_shared:
        aliases:
          - starlink-bridge
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

'@
$compose = [IO.File]::ReadAllText($composePath)
if ($compose -match '(?m)^  starlink-bridge:\s*$') {
    # Replace only this explicitly named service block, preserving all others.
    $pattern = '(?ms)^  starlink-bridge:\s*\r?\n.*?(?=^  [A-Za-z0-9_-]+:\s*\r?$|^networks:\s*\r?$|\z)'
    $compose = [regex]::Replace($compose,$pattern,[Text.RegularExpressions.MatchEvaluator]{param($m) $service + "`r`n"},1)
} else {
    $anchor = [regex]::Match($compose,'(?m)^networks:\s*$')
    if (!$anchor.Success) { throw 'Cannot locate top-level NAS compose networks block.' }
    $compose = $compose.Insert($anchor.Index,$service + "`r`n")
}
[IO.File]::WriteAllText($composePath,$compose,$utf8)

$defaults = [ordered]@{
    STARLINK_MODE='cloud'; STARLINK_DEVICE_ID=''; STARLINK_LOCAL_ADDRESS='192.168.100.1:9200';
    STARLINK_CLOUD_WEB_HOST='starlink.com'; STARLINK_POLL_INTERVAL='60s';
    STARLINK_STALE_AFTER='3m'; STARLINK_COOKIE_REFRESH_INTERVAL='30m'
}
foreach ($path in @($envPath,$envExamplePath)) {
    $content = if (Test-Path -LiteralPath $path) { [IO.File]::ReadAllText($path) } else { '' }
    $addition = ''
    foreach ($entry in $defaults.GetEnumerator()) {
        if ($content -notmatch ('(?m)^' + [regex]::Escape($entry.Key) + '=')) { $addition += "$($entry.Key)=$($entry.Value)`r`n" }
    }
    if ($addition) { [IO.File]::WriteAllText($path,$content.TrimEnd() + "`r`n`r`n# StarlinkBridge`r`n" + $addition,$utf8) }
}

foreach ($path in $promPaths) {
    if (!(Test-Path -LiteralPath $path)) { continue }
    $content=[IO.File]::ReadAllText($path)
    if ($content -notmatch '(?m)^\s*- job_name:\s*[''"]?starlink[''"]?\s*$') {
        $job=@'

  - job_name: 'starlink'
    scrape_interval: 60s
    scrape_timeout: 10s
    static_configs:
      - targets: ['starlink-bridge:9091']
'@
        [IO.File]::WriteAllText($path,$content.TrimEnd()+"`r`n"+$job+"`r`n",$utf8)
    }
}

if ($BootstrapCookie -and (Test-Path -LiteralPath $BootstrapCookie)) {
    Copy-Item -LiteralPath $BootstrapCookie -Destination (Join-Path $configRoot 'starlink.cookie') -Force
}
$nasState = Join-Path $dataRoot 'cookies.json'
if ($SeedCookieState -and (Test-Path -LiteralPath $SeedCookieState) -and !(Test-Path -LiteralPath $nasState)) {
    Copy-Item -LiteralPath $SeedCookieState -Destination $nasState
}
Write-Output "Staged StarlinkBridge at $buildRoot"
Write-Output "Cookie bootstrap location: $(Join-Path $configRoot 'starlink.cookie')"
Write-Output "Configuration backup: $backupRoot"
Write-Output 'No NAS containers were started or restarted. Rebuild the bridges stack and reload Prometheus.'
