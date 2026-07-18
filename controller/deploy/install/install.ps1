# controller/deploy/install/install.ps1
# AURA one-click enrollment installer (Windows) -- M12 TASK-009.
#
# A brand-new device joins the fleet with one command (copied from the console "Add device" page).
# Two forms (install-command-spec section 1):
#   env-injection form (recommended, copy-paste robust):
#     $env:AURA_TOKEN="<TOKEN>"; $env:AURA_CONTROLLER="<VIP:7443>"; iwr https://<release-host>/install.ps1 -useb | iex
#   scriptblock form (advanced users):
#     & ([scriptblock]::Create((iwr -useb https://<release-host>/install.ps1))) -Token <TOKEN> -Controller <VIP:7443>
#
# Flow (install-command-spec section 3): detect Windows -> pull aura-node.exe -> enroll (generate keypair+CSR
#   locally, private key NEVER leaves the node -> POST /v1/enroll, swap per-node cert to disk) -> scheduled task
#   AtLogOn InteractiveToken Highest (grabs a screenshot-able / injectable desktop context; base:
#   win-install-rev-task.ps1:6-12) -> Start -> per-node cert mTLS reverse-dials :7443 Register -> appears in fleet.
#
# The `aura-node enroll` subcommand is a TASK-006 deliverable (feature enroll); this script invokes it.
# Real-machine one-click onboarding is verified in TASK-011. Release URL binary distribution lands in TASK-012
#   (GitHub public release); this script uses a placeholder <release-host>, aligned to the real URL in T011/T012.
#
# Encoding red line: this file is pure ASCII (no BOM). Windows PowerShell 5.1 reads BOM-less files as the system
#   ANSI codepage; non-ASCII in comments/strings corrupts parsing on GBK hosts and BOM risks mojibake when served
#   without charset=utf-8 over iwr|iex. Pure ASCII is delivery/host/charset agnostic. (install.sh keeps Chinese
#   comments -- POSIX sh is byte-transparent for comments, no such re-decode hazard.)

# Parameters:
#   Token/Controller   required; env-injection form falls back to $env:AURA_TOKEN/$env:AURA_CONTROLLER,
#                      scriptblock form passes -Token/-Controller explicitly
#   Label/Location     optional; reported at enroll + Register (console can edit later, authoritative)
#   DataDir            co-located root for certs + node_id (default C:\aura)
#   ReleaseBase        release distribution base (placeholder; aligned in T011/T012 via -ReleaseBase or
#                      $env:AURA_RELEASE_BASE)
#   CaUrl              CA pin source (defaults to derived from ReleaseBase)
#   TlsDomain/HttpBind steady-state reverse params (same as win-install-rev-task.ps1)
#   EnrollPort         enroll REST port (enroll endpoint = HOST derived from Controller + ':' + EnrollPort)
#   TaskUser           scheduled-task principal (AtLogOn + InteractiveToken); defaults to current identity
#                      (more general than a hardcoded Administrator)
param(
    [string]$Token       = $env:AURA_TOKEN,
    [string]$Controller  = $env:AURA_CONTROLLER,
    [string]$Label       = $env:AURA_LABEL,
    [string]$Location    = $env:AURA_LOCATION,
    [string]$DataDir     = 'C:\aura',
    [string]$ReleaseBase = $(if ($env:AURA_RELEASE_BASE) { $env:AURA_RELEASE_BASE } else { 'https://<release-host>' }),
    [string]$CaUrl       = $env:AURA_CA_URL,
    [string]$TlsDomain   = 'aura-controller',
    [string]$HttpBind    = '0.0.0.0:7100',
    [int]   $EnrollPort  = 18080,
    [string]$TaskUser    = $([System.Security.Principal.WindowsIdentity]::GetCurrent().Name)
)

$ErrorActionPreference = 'Stop'
# Older PowerShell may default to TLS1.0; force TLS1.2 so release/enroll HTTPS handshakes succeed (fail-closed).
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Info($m) { Write-Host "[install] $m" }
function Warn($m) { Write-Warning $m }

# ---- validation ----
if (-not $Token)      { throw '-Token is required (or set $env:AURA_TOKEN)' }
if (-not $Controller) { throw '-Controller is required (HOST:7443, or set $env:AURA_CONTROLLER)' }
if ($ReleaseBase -like '*<release-host>*') {
    Warn 'ReleaseBase still placeholder <release-host>: binary/CA fetch will fail; T011/T012 aligns real release URL via -ReleaseBase or $env:AURA_RELEASE_BASE'
}
# Administrator check: Register-ScheduledTask + InteractiveToken RunLevel Highest require elevation.
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) { throw 'install.ps1 requires Administrator (Register-ScheduledTask + InteractiveToken Highest). Run in an elevated PowerShell.' }

# ---- 1) detect platform (arch) ----
switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { $archAsset = 'amd64' }
    'ARM64' { $archAsset = 'amd64'; Warn 'ARM64 Windows: release spec normalizes only windows-amd64 asset; falling back to amd64 (x64 emulation). Align arm64 asset in T012 if produced.' }
    default { $archAsset = 'amd64'; Warn "Unknown PROCESSOR_ARCHITECTURE=$($env:PROCESSOR_ARCHITECTURE); falling back to amd64" }
}
$asset = "aura-node-windows-$archAsset.exe"
$exe   = Join-Path $DataDir 'aura-node.exe'
Info "platform: windows $archAsset -> asset=$asset"

# ---- 2) pull binary ----
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
$binUrl = "$ReleaseBase/$asset"
Info "fetch binary: $binUrl -> $exe"
try { Invoke-WebRequest -UseBasicParsing -Uri $binUrl -OutFile $exe }
catch { throw "download binary failed: $binUrl ($_)" }

# ---- 3+4) enroll (genkey+CSR -> swap per-node cert) ----
# CA pin: prefer pre-placed ($DataDir\ca.crt already present -> reuse), else pull from release. enroll.exe --ca is
# required: the node pins it to verify the controller :18080 server-TLS (TOFU, closes MITM; design section 3.3).
$caPin = Join-Path $DataDir 'ca.crt'
if (Test-Path $caPin) {
    Info "reuse existing CA pin $caPin"
} else {
    if (-not $CaUrl) { $CaUrl = "$ReleaseBase/ca.crt" }
    Info "fetch CA pin: $CaUrl -> $caPin"
    try { Invoke-WebRequest -UseBasicParsing -Uri $CaUrl -OutFile $caPin }
    catch { throw "download CA pin failed: $CaUrl (pre-place $caPin or pass -CaUrl) ($_)" }
}

# enroll endpoint: derived as HOST:18080 from -Controller HOST (install-command-spec section 2).
$ctrlHost = ($Controller -split ':')[0]
$enrollEp = '{0}:{1}' -f $ctrlHost, $EnrollPort
Info "enroll endpoint: $enrollEp (derived from controller HOST)"

# argv array pass-through: natively handles --label/--location values with spaces/unicode (no manual quoting).
$enrollArgs = @('enroll', '--controller', $enrollEp, '--token', $Token, '--platform', 'windows', '--ca', $caPin, '--data-dir', $DataDir)
if ($Label)    { $enrollArgs += @('--label', $Label) }
if ($Location) { $enrollArgs += @('--location', $Location) }
Info 'aura-node.exe enroll (private key generated locally, never leaves node; swaps per-node cert)...'
& $exe @enrollArgs
if ($LASTEXITCODE -ne 0) {
    throw "enroll failed exit=$LASTEXITCODE (invalid/expired/exhausted token -> 401; CSR rejected -> 400; network/CA pin -> check $enrollEp reachable and $caPin matches)"
}

# ---- 6) install service: scheduled task AtLogOn InteractiveToken Highest (grabs desktop context) ----
# Reverse params (per-node cert self-auth, no token); --label/--location are Register bootstrap values (console editable).
$logPath   = Join-Path $DataDir 'node.log'
$nodeArgs  = "--driver desktop --controller $Controller --ca $DataDir\ca.crt --cert $DataDir\node.crt --key $DataDir\node.key --tls-domain $TlsDomain --data-dir $DataDir"
if ($Label)    { $nodeArgs += " --label `"$Label`"" }
if ($Location) { $nodeArgs += " --location `"$Location`"" }
$nodeArgs += " http --bind $HttpBind"

# cmd /c carries log redirection (base: win-install-rev-task.ps1:8-9).
$action    = New-ScheduledTaskAction -Execute 'cmd' -Argument ('/c "' + $exe + '" ' + $nodeArgs + ' > "' + $logPath + '" 2>&1')
$trigger   = New-ScheduledTaskTrigger -AtLogOn -User $TaskUser
# InteractiveToken: the task runs in $TaskUser's interactive desktop session, so aura-node gets a
# screenshot-able / injectable / UIA-tree context.
$principal = New-ScheduledTaskPrincipal -UserId $TaskUser -LogonType Interactive -RunLevel Highest
$settings  = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Seconds 0)

Info "register scheduled task 'AuraNode' (AtLogOn $TaskUser InteractiveToken Highest)"
Register-ScheduledTask -TaskName 'AuraNode' -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null

# ---- 7) start service, reverse-connect ----
Start-ScheduledTask -TaskName 'AuraNode'
Start-Sleep -Seconds 5
$proc = Get-Process aura-node -ErrorAction SilentlyContinue
if ($proc) {
    Info "OK aura-node pid=$($proc.Id) session=$($proc.SessionId) (real-machine fleet appearance verified in TASK-011)"
} else {
    $tinfo = Get-ScheduledTaskInfo -TaskName 'AuraNode'
    Warn ("aura-node process not ready yet LastResult=0x{0:X} (AtLogOn fires on $TaskUser interactive logon; see $logPath)" -f $tinfo.LastTaskResult)
    if (Test-Path $logPath) { Get-Content $logPath -Tail 8 -ErrorAction SilentlyContinue }
}
