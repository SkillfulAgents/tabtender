<#
.SYNOPSIS
    Removes the tabtenderd Windows Service.

.DESCRIPTION
    Stops and unregisters the service and removes tabtenderd.exe. The data
    directory (API key, configuration, browser profiles, logs) is kept unless
    -PurgeData is given, so a later install picks up where this one left off.

    Run elevated. uninstall.cmd next to this script runs it with the
    execution policy bypassed.

.PARAMETER InstallDir
    Where tabtenderd.exe was installed. Defaults to %ProgramFiles%\Tabtender.
    Only tabtenderd.exe is deleted from it, and the directory itself only if
    that leaves it empty.

.PARAMETER PurgeData
    Also delete the data directory (%ProgramData%\Tabtender) and the service
    account's profile. This destroys the API key and all browser profiles.
#>
[CmdletBinding()]
param(
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'Tabtender'),
    [switch]$PurgeData
)

$ErrorActionPreference = 'Stop'

# Report a failure as one readable line and a non-zero exit code instead of
# a PowerShell error record.
trap {
    Write-Host "ERROR: $($_.Exception.Message)"
    exit 1
}

# Started below PowerShell 7 (for instance from a Command Prompt opened there),
# Windows PowerShell inherits a module path that points at PowerShell 7's
# modules and fails to load them. Drop those entries before any module loads.
if ($PSVersionTable.PSEdition -ne 'Core') {
    $env:PSModulePath = (@($env:PSModulePath -split ';' | Where-Object { $_ -and $_ -notmatch '(^|\\)PowerShell\\' }) +
        "$PSHOME\Modules") -join ';'
}

$ServiceName = 'tabtenderd'
$ServiceKey = "HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName"

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'The uninstaller must be run elevated (as Administrator).'
}

# The service may have been pointed at another data directory; find out
# before its registration is gone.
$dataDir = Join-Path $env:ProgramData 'Tabtender'
foreach ($line in (Get-ItemProperty -Path $ServiceKey -ErrorAction SilentlyContinue).Environment) {
    $name, $value = $line -split '=', 2
    if ($name -eq 'TABTENDER_DATA_DIR' -and $value) { $dataDir = $value }
}

$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($service) {
    # Disabled first, so the SCM's recovery actions cannot restart it.
    Set-Service -Name $ServiceName -StartupType Disabled
    $processId = (Get-CimInstance Win32_Service -Filter "Name = '$ServiceName'").ProcessId
    if ($service.Status -ne 'Stopped') {
        Write-Host "Stopping $ServiceName"
        Stop-Service -Name $ServiceName -Force -WarningAction SilentlyContinue
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(120))
    }
    # Files stay locked until the process is really gone, which is shortly
    # after the service reports that it stopped.
    $deadline = (Get-Date).AddSeconds(30)
    while ($processId -and (Get-Process -Id $processId -ErrorAction SilentlyContinue)) {
        if ((Get-Date) -gt $deadline) { throw "The $ServiceName process ($processId) did not exit." }
        Start-Sleep -Milliseconds 200
    }
    $output = & sc.exe delete $ServiceName
    if ($LASTEXITCODE -ne 0) {
        throw "sc.exe delete $ServiceName failed ($LASTEXITCODE): $output"
    }
    Write-Host "Removed the $ServiceName service."
} else {
    Write-Host "The $ServiceName service is not installed."
}

# Only what the installer put there: -InstallDir may name a directory that
# holds other things.
$exe = Join-Path $InstallDir 'tabtenderd.exe'
if (Test-Path -LiteralPath $exe -PathType Leaf) {
    Remove-Item -LiteralPath $exe -Force
    Write-Host "Deleted $exe"
}
if ((Test-Path -LiteralPath $InstallDir -PathType Container) -and
    -not (Get-ChildItem -LiteralPath $InstallDir -Force | Select-Object -First 1)) {
    Remove-Item -LiteralPath $InstallDir -Force
}

if ($PurgeData) {
    if (Test-Path -LiteralPath $dataDir) {
        # rd removes links instead of following them. The service account
        # could create links in its data directory, and deleting through one
        # would delete an administrator's choice of files elsewhere.
        & cmd.exe /d /c rd /s /q $dataDir | Out-Null
        if (Test-Path -LiteralPath $dataDir) {
            throw "Could not delete all of $dataDir; close programs using it and run the uninstaller again."
        }
        Write-Host "Deleted $dataDir"
    }

    # The virtual account's profile holds the DPAPI keys that protected the
    # secrets just deleted, plus temporary browser files.
    $sid = $null
    if ("$(& sc.exe showsid $ServiceName)" -match 'S-1-5-80(-\d+)+') { $sid = $Matches[0] }
    $accountProfile = $null
    if ($sid) { $accountProfile = Get-CimInstance Win32_UserProfile -Filter "SID = '$sid'" -ErrorAction SilentlyContinue }
    if ($accountProfile) {
        try {
            Remove-CimInstance -InputObject $accountProfile
            Write-Host "Deleted the service account profile $($accountProfile.LocalPath)"
        } catch {
            Write-Warning "Could not delete the service account profile $($accountProfile.LocalPath): $($_.Exception.Message)"
        }
    }
} else {
    Write-Host "Kept the data in $dataDir (use -PurgeData to delete it)."
}
