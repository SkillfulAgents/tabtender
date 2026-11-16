<#
.SYNOPSIS
    Removes the tabtenderd Windows Service.

.PARAMETER InstallDir
    Where tabtenderd.exe was installed. Defaults to %ProgramFiles%\Tabtender.

.PARAMETER PurgeData
    Also delete %ProgramData%\Tabtender (configuration, profiles, secrets, logs).
#>
[CmdletBinding()]
param(
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'Tabtender'),
    [switch]$PurgeData
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'tabtenderd'

$principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'uninstall.ps1 must be run from an elevated (Administrator) PowerShell prompt.'
}

$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($service) {
    if ($service.Status -ne 'Stopped') {
        Write-Host "Stopping $ServiceName"
        Stop-Service -Name $ServiceName -Force
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(90))
    }
    & sc.exe delete $ServiceName | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "sc.exe delete failed ($LASTEXITCODE)"
    }
    Write-Host "Removed the $ServiceName service."
} else {
    Write-Host "The $ServiceName service is not installed."
}

if (Test-Path -LiteralPath $InstallDir) {
    Remove-Item -LiteralPath $InstallDir -Recurse -Force
}
if ($PurgeData) {
    $dataDir = Join-Path $env:ProgramData 'Tabtender'
    if (Test-Path -LiteralPath $dataDir) {
        Remove-Item -LiteralPath $dataDir -Recurse -Force
        Write-Host "Deleted $dataDir"
    }
}
