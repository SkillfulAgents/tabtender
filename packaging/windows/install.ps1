<#
.SYNOPSIS
    Installs tabtenderd as a native Windows Service.

.DESCRIPTION
    Copies tabtenderd.exe to the install directory, creates the data directory
    (%ProgramData%\Tabtender) with an ACL restricted to SYSTEM and
    Administrators, registers the service with the Service Control Manager
    (automatic start at boot, restart on failure), and starts it.

    Run from an elevated PowerShell prompt.

.PARAMETER BinaryPath
    The tabtenderd.exe to install. Defaults to tabtenderd.exe next to this script.

.PARAMETER InstallDir
    Where the executable is installed. Defaults to %ProgramFiles%\Tabtender.

.PARAMETER ApiKey
    Optional administrator-supplied API key (at least 32 characters). It is
    handed to the service through a provisioning file that the service imports
    into DPAPI-protected storage and deletes on first start. When omitted, the
    service generates a key; read it once from
    %ProgramData%\Tabtender\secrets\initial-api-key.txt (the file is removed
    after the key is first used).

.PARAMETER Environment
    Optional TABTENDER_* environment variables for the service, for example
    @{ TABTENDER_PORT = '8080' }. Do not pass secrets here: service
    environment blocks are readable by local users.

.PARAMETER NoStart
    Register the service without starting it.

.EXAMPLE
    .\install.ps1

.EXAMPLE
    .\install.ps1 -ApiKey (Get-Content .\key.txt) -Environment @{ TABTENDER_PORT = '8080' }
#>
[CmdletBinding()]
param(
    [string]$BinaryPath = (Join-Path $PSScriptRoot 'tabtenderd.exe'),
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'Tabtender'),
    [string]$ApiKey,
    [hashtable]$Environment = @{},
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'tabtenderd'
$DataDir = Join-Path $env:ProgramData 'Tabtender'

$principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'install.ps1 must be run from an elevated (Administrator) PowerShell prompt.'
}
if (-not (Test-Path -LiteralPath $BinaryPath -PathType Leaf)) {
    throw "tabtenderd.exe not found at $BinaryPath"
}
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    throw "The $ServiceName service is already installed. Run uninstall.ps1 first."
}
if ($ApiKey -and $ApiKey.Length -lt 32) {
    throw 'The API key must be at least 32 characters long.'
}
foreach ($name in $Environment.Keys) {
    if ($name -notlike 'TABTENDER_*') {
        throw "Only TABTENDER_* variables may be set for the service, got $name"
    }
    if ($name -in 'TABTENDER_API_KEY', 'TABTENDER_CLOUDFLARE_TUNNEL_TOKEN') {
        throw "$name is a secret; provision it with -ApiKey or a .provision file instead of the service environment."
    }
}

function Invoke-Native {
    param([string]$File, [string[]]$Arguments)
    $output = & $File @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "$File $($Arguments -join ' ') failed ($LASTEXITCODE): $output"
    }
}

Write-Host "Installing tabtenderd to $InstallDir"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$exe = Join-Path $InstallDir 'tabtenderd.exe'
Copy-Item -LiteralPath $BinaryPath -Destination $exe -Force

# The data directory holds the configuration, browser profiles, and secrets:
# only SYSTEM (the service account) and Administrators may access it.
Write-Host "Preparing $DataDir"
$secretsDir = Join-Path $DataDir 'secrets'
New-Item -ItemType Directory -Force -Path $secretsDir | Out-Null
Invoke-Native icacls @($DataDir, '/inheritance:r', '/grant:r', '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F', '/Q')

if ($ApiKey) {
    # Imported into DPAPI storage and deleted by the service at startup.
    [IO.File]::WriteAllText((Join-Path $secretsDir 'api-key.provision'), $ApiKey)
}

Write-Host "Registering the $ServiceName service"
New-Service -Name $ServiceName `
    -BinaryPathName "`"$exe`" run" `
    -DisplayName 'Tabtender' `
    -Description 'Tabtender browser session server: native browser sessions for AI agents over CDP.' `
    -StartupType Automatic | Out-Null

# Restart after crashes: immediately, then after 5s, then every 30s.
Invoke-Native sc.exe @('failure', $ServiceName, 'reset=', '86400', 'actions=', 'restart/1000/restart/5000/restart/30000')

if ($Environment.Count -gt 0) {
    $block = $Environment.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }
    Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName" `
        -Name Environment -Type MultiString -Value ([string[]]$block)
}

if (-not $NoStart) {
    Start-Service -Name $ServiceName
    Write-Host "The $ServiceName service is running."
}

Write-Host ''
Write-Host "Configuration:  $(Join-Path $DataDir 'config.yaml') (restart the service after editing)"
Write-Host "Log file:       $(Join-Path $DataDir 'logs\tabtenderd.log')"
if (-not $ApiKey) {
    Write-Host "API key:        $(Join-Path $secretsDir 'initial-api-key.txt') (one-time; removed after first use)"
}
