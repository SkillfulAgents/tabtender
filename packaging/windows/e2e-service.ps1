<#
.SYNOPSIS
    End-to-end test of tabtenderd as a native Windows Service.

.DESCRIPTION
    Installs the service the way a user does (install.cmd, which runs
    install.ps1 under Windows PowerShell), drives real headless Chrome
    sessions through it with Playwright and Puppeteer, and checks the service
    account, directory permissions, SCM lifecycle and recovery, in-place
    upgrade, key provisioning, YAML and environment configuration, and
    cleanup. Must run elevated under PowerShell 7. Uninstalls the service
    when it finishes.

.PARAMETER BinaryPath
    The tabtenderd.exe under test.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$BinaryPath
)

$ErrorActionPreference = 'Stop'
$Root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$BinaryPath = (Resolve-Path -LiteralPath $BinaryPath).ProviderPath
$DataDir = Join-Path $env:ProgramData 'Tabtender'
$InstallDir = Join-Path $env:ProgramFiles 'Tabtender'
$SecretsDir = Join-Path $DataDir 'secrets'
$KeyFile = Join-Path $SecretsDir 'initial-api-key.txt'
$LogFile = Join-Path $DataDir 'logs\tabtenderd.log'
$InstallCmd = Join-Path $PSScriptRoot 'install.cmd'
$UninstallCmd = Join-Path $PSScriptRoot 'uninstall.cmd'
$InstallScript = Join-Path $PSScriptRoot 'install.ps1'
$ServiceAccount = 'NT SERVICE\tabtenderd'
$UsersSid = 'S-1-5-32-545'

function Step($message) { Write-Host "`n==> $message" }
function Assert($condition, $message) { if (-not $condition) { throw "FAIL: $message" } }

# Runs one of the .cmd wrappers, echoing its output. Returns the output; the
# exit code is left in $LASTEXITCODE.
function Invoke-Wrapper {
    param([string]$Wrapper, [string[]]$Arguments = @(), [switch]$AllowFailure)
    $ErrorActionPreference = 'Continue'
    $output = & $Wrapper @Arguments 2>&1 | ForEach-Object { "$_" } | Out-String
    Write-Host $output.TrimEnd()
    if (-not $AllowFailure -and $LASTEXITCODE -ne 0) {
        throw "FAIL: $(Split-Path $Wrapper -Leaf) $($Arguments -join ' ') exited with $LASTEXITCODE"
    }
    $output
}

function Invoke-Native {
    param([string]$File, [string[]]$Arguments)
    $ErrorActionPreference = 'Continue' # stderr output alone is not a failure
    $output = & $File @Arguments 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0) { throw "FAIL: $File $($Arguments -join ' ') failed ($LASTEXITCODE): $output" }
    $output
}

function Invoke-Api {
    param([string]$Method = 'GET', [string]$Url, [string]$Key, [string]$Body)
    $headers = @{}
    if ($Key) { $headers['Authorization'] = "Bearer $Key" }
    $params = @{ Method = $Method; Uri = $Url; Headers = $headers; SkipHttpErrorCheck = $true; TimeoutSec = 120 }
    if ($Body) { $params['Body'] = $Body; $params['ContentType'] = 'application/json' }
    $response = Invoke-WebRequest @params
    $json = $null
    if ($response.Content) { $json = $response.Content | ConvertFrom-Json }
    [pscustomobject]@{ Status = [int]$response.StatusCode; Json = $json }
}

function Wait-Until {
    param([string]$What, [int]$TimeoutSeconds, [scriptblock]$Condition)
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try { if (& $Condition) { return } } catch { }
        Start-Sleep -Milliseconds 300
    }
    throw "FAIL: timed out waiting for $What"
}

function Get-ServiceInfo { Get-CimInstance Win32_Service -Filter "Name = 'tabtenderd'" }

function Get-SessionBrowsers {
    Get-CimInstance Win32_Process -Filter "Name = 'chrome.exe'" |
        Where-Object { $_.CommandLine -like '*Tabtender\profiles*' }
}

function Get-ProcessAccount($process) {
    $owner = Invoke-CimMethod -InputObject $process -MethodName GetOwner
    "$($owner.Domain)\$($owner.User)"
}

# The data directory must be cut off from %ProgramData%'s permissions (which
# let every user create files) and grant access to nobody but SYSTEM,
# Administrators and the service account.
function Assert-DataDirRestricted {
    $acl = Get-Acl -LiteralPath $DataDir
    Assert $acl.AreAccessRulesProtected "$DataDir still inherits permissions from $env:ProgramData"
    $allowed = 'NT AUTHORITY\SYSTEM', 'BUILTIN\Administrators', $ServiceAccount
    $others = @($acl.Access | ForEach-Object { $_.IdentityReference.Value } | Where-Object { $_ -notin $allowed })
    Assert ($others.Count -eq 0) "$DataDir is accessible to: $($others -join ', ')"
    Assert ($acl.Owner -in $allowed) "$DataDir is owned by $($acl.Owner)"
}

function Remove-DataDir {
    # Retried: a service that just stopped holds its files a moment longer.
    Wait-Until "$DataDir to be deleted" 20 {
        if (Test-Path -LiteralPath $DataDir) { & cmd.exe /d /c rd /s /q $DataDir 2>&1 | Out-Null }
        -not (Test-Path -LiteralPath $DataDir)
    }
}

# Prints one block of diagnostics. Whatever goes wrong while collecting it is
# noted and otherwise ignored: diagnostics must never mask the failure.
function Write-Section {
    param([string]$Title, [scriptblock]$Body)
    Write-Host "--- $Title ---"
    try {
        $ErrorActionPreference = 'Continue'
        & $Body 2>&1 | Out-String -Stream | ForEach-Object { Write-Host $_ }
    } catch {
        Write-Host "(could not collect: $($_.Exception.Message))"
    }
}

function Write-Diagnostics {
    Write-Section 'service configuration' {
        & sc.exe qc tabtenderd
        & sc.exe queryex tabtenderd
        & sc.exe qfailure tabtenderd
        & sc.exe qfailureflag tabtenderd
        Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\tabtenderd' -ErrorAction SilentlyContinue |
            Select-Object ImagePath, ObjectName, Start, Environment | Format-List
    }
    foreach ($path in $DataDir, $SecretsDir, $InstallDir, (Join-Path $InstallDir 'tabtenderd.exe')) {
        Write-Section "permissions of $path" {
            if (Test-Path -LiteralPath $path) {
                "owner: $((Get-Acl -LiteralPath $path).Owner)"
                & icacls.exe $path
            } else {
                'does not exist'
            }
        }
    }
    Write-Section 'data directory' {
        if (Test-Path -LiteralPath $DataDir) {
            Get-ChildItem -LiteralPath $DataDir -Force -Recurse -Depth 1 -ErrorAction SilentlyContinue | ForEach-Object { $_.FullName }
        }
    }
    Write-Section 'service account profile' {
        Get-CimInstance Win32_UserProfile | Where-Object { $_.LocalPath -like '*tabtenderd*' } |
            Select-Object LocalPath, SID, Loaded | Format-List
    }
    Write-Section 'tabtenderd and chrome processes' {
        Get-CimInstance Win32_Process -Filter "Name = 'tabtenderd.exe' OR Name = 'chrome.exe'" |
            ForEach-Object { "$($_.ProcessId) $(Get-ProcessAccount $_) $($_.Name)" }
    }
    Write-Section 'Service Control Manager events' {
        Get-WinEvent -LogName System -MaxEvents 500 -ErrorAction SilentlyContinue |
            Where-Object { $_.ProviderName -eq 'Service Control Manager' -and $_.Message -match 'Tabtender|tabtenderd' } |
            Select-Object -First 15 | ForEach-Object { "$($_.TimeCreated.ToString('s')) [$($_.Id)] $($_.Message)" }
    }
    Write-Section 'tabtenderd events in the Application log' {
        Get-WinEvent -LogName Application -MaxEvents 500 -ErrorAction SilentlyContinue |
            Where-Object { $_.ProviderName -eq 'tabtenderd' } |
            Select-Object -First 5 | ForEach-Object { "$($_.TimeCreated.ToString('s')) $($_.Message)" }
    }
    Write-Section 'service log tail' {
        if (Test-Path -LiteralPath $LogFile) { Get-Content -LiteralPath $LogFile -Tail 80 } else { '(no log file)' }
    }
}

# The outcome is decided here and nowhere else: $failure stays set unless
# every step ran to the end, and the script's last statement turns it into
# the exit code. Diagnostics and cleanup run in between and cannot change it,
# whatever they print, throw, or leave in $LASTEXITCODE.
$failure = 'the test did not run to completion'
try {
    Invoke-Wrapper $UninstallCmd @('-PurgeData') | Out-Null
    Remove-DataDir

    Step 'A data directory created by a non-administrator is refused'
    New-Item -ItemType Directory -Path $DataDir | Out-Null
    Set-Content -Path (Join-Path $DataDir 'config.yaml') -Value "browser:`n  executablePath: C:\planted.exe"
    Invoke-Native icacls.exe @($DataDir, '/setowner', "*$UsersSid", '/Q') | Out-Null
    $output = Invoke-Wrapper $InstallCmd @('-BinaryPath', $BinaryPath) -AllowFailure
    Assert ($LASTEXITCODE -ne 0) 'the installer accepted a data directory owned by BUILTIN\Users'
    Assert ($output -match 'cannot be trusted') "the installer failed for another reason: $output"
    Assert (-not (Get-Service tabtenderd -ErrorAction SilentlyContinue)) 'the service was registered despite the refused data directory'
    Remove-DataDir

    Step 'Files planted in an administrator-created data directory are refused'
    New-Item -ItemType Directory -Path (Join-Path $DataDir 'bin') | Out-Null
    $planted = Join-Path $DataDir 'bin\cloudflared.exe'
    Set-Content -Path $planted -Value 'planted'
    Invoke-Native icacls.exe @($planted, '/setowner', "*$UsersSid", '/Q') | Out-Null
    # In-process, which also covers install.ps1 under PowerShell 7.
    $refusal = & $InstallScript -BinaryPath $BinaryPath *>&1 | Out-String
    Write-Host $refusal.TrimEnd()
    Assert ($LASTEXITCODE -ne 0) 'the installer accepted a planted file'
    Assert ($refusal -match 'may have been planted') "the installer failed for another reason: $refusal"
    Assert ($refusal -match 'cloudflared\.exe') "the refusal does not name the planted file: $refusal"
    Assert (-not (Get-Service tabtenderd -ErrorAction SilentlyContinue)) 'the service was registered despite the planted file'
    Remove-DataDir

    Step 'Install the service (generated API key)'
    Invoke-Wrapper $InstallCmd @('-BinaryPath', $BinaryPath) | Out-Null
    $service = Get-ServiceInfo
    Assert ($service.State -eq 'Running') "service state is $($service.State)"
    Assert ($service.StartMode -eq 'Auto') "service start mode is $($service.StartMode)"
    Assert ($service.StartName -eq $ServiceAccount) "service runs as $($service.StartName), want $ServiceAccount"
    $daemon = Get-CimInstance Win32_Process -Filter "ProcessId = $($service.ProcessId)"
    Assert ((Get-ProcessAccount $daemon) -eq $ServiceAccount) "the daemon process runs as $(Get-ProcessAccount $daemon)"
    Assert ((& sc.exe qfailureflag tabtenderd | Out-String) -match 'TRUE') 'failure actions are not enabled for error exits'
    Assert-DataDirRestricted
    $url = 'http://127.0.0.1:3000'
    # No waiting: the installer must not return before the API answers.
    Assert ((Invoke-Api -Url "$url/v1/health").Status -eq 401) 'the API was not listening when the installer returned'

    Step 'One-time retrieval of the generated key'
    Assert (Test-Path $KeyFile) 'the initial key file was not created'
    $key = (Get-Content $KeyFile -Raw).Trim()
    $acl = (Get-Acl $SecretsDir).Access | ForEach-Object { $_.IdentityReference.Value }
    Assert (-not ($acl -match 'Users|Everyone')) "the secrets directory is accessible to: $($acl -join ', ')"
    $health = Invoke-Api -Url "$url/v1/health" -Key $key
    Assert ($health.Status -eq 200) "health with the generated key returned $($health.Status)"
    Assert ($health.Json.browser.available) "no browser available to the service: $($health.Json.browser.error)"
    Assert (-not (Test-Path $KeyFile)) 'the initial key file must be deleted after first use'
    Assert (Test-Path (Join-Path $SecretsDir 'api-key.dpapi')) 'the key is not in DPAPI storage'

    Step 'Sessions are native headless Chrome processes of the service account'
    $created = Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $key -Body '{}'
    Assert ($created.Status -eq 201) "create session returned $($created.Status): $($created.Json | ConvertTo-Json -Compress)"
    Assert ($created.Json.headless -eq $true) 'a Windows Service session must be headless'
    $browsers = @(Get-SessionBrowsers)
    Assert ($browsers.Count -ge 1) 'no chrome.exe process found for the session'
    # Chrome starts helper processes that live for a moment only; one that
    # exited after it was listed has no owner to report.
    $accounts = @($browsers | ForEach-Object { try { Get-ProcessAccount $_ } catch { } } | Sort-Object -Unique)
    Assert ("$accounts" -eq $ServiceAccount) "browsers run as: $($accounts -join ', ')"
    $headed = Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $key -Body '{"headless": false}'
    Assert ($headed.Status -eq 400) "a headed session request returned $($headed.Status), want 400"
    $deleted = Invoke-Api -Method DELETE -Url "$url/v1/sessions/$($created.Json.id)" -Key $key
    Assert ($deleted.Status -eq 200) "delete returned $($deleted.Status)"
    Wait-Until 'the browser to exit' 30 { @(Get-SessionBrowsers).Count -eq 0 }

    Step 'Playwright and Puppeteer through the service'
    $env:TABTENDER_URL = $url
    $env:TABTENDER_API_KEY = $key
    Push-Location (Join-Path $Root 'test\clients')
    try {
        npm test
        Assert ($LASTEXITCODE -eq 0) 'the client conformance suite failed'
    } finally { Pop-Location }

    Step 'SCM restart: key persists, sessions are reconciled'
    $leftover = Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $key -Body '{"ttlSeconds": 600}'
    Assert ($leftover.Status -eq 201) 'could not create a session before restart'
    Restart-Service tabtenderd
    Wait-Until 'the API after restart' 60 { (Invoke-Api -Url "$url/v1/health" -Key $key).Status -eq 200 }
    $list = Invoke-Api -Url "$url/v1/sessions" -Key $key
    Assert (@($list.Json.sessions).Count -eq 0) 'sessions survived a service restart'
    Assert (@(Get-SessionBrowsers).Count -eq 0) 'browsers survived a service restart'

    Step 'Installing again upgrades in place and keeps the data'
    Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $key -Body '{}' | Out-Null
    $before = (Get-ServiceInfo).ProcessId
    $marker = Join-Path $DataDir 'upgrade-marker.txt'
    Set-Content -Path $marker -Value 'kept'
    Invoke-Wrapper $InstallCmd @('-BinaryPath', $BinaryPath) | Out-Null
    $service = Get-ServiceInfo
    Assert ($service.State -eq 'Running') "after the upgrade the service state is $($service.State)"
    Assert ($service.ProcessId -ne $before) 'the upgrade did not restart the service'
    Assert ($service.StartMode -eq 'Auto') "after the upgrade the start mode is $($service.StartMode)"
    Assert ($service.StartName -eq $ServiceAccount) "after the upgrade the service runs as $($service.StartName)"
    Assert ((Invoke-Api -Url "$url/v1/health" -Key $key).Status -eq 200) 'the API key did not survive the upgrade'
    Assert (-not (Test-Path $KeyFile)) 'the upgrade generated a new API key'
    Assert (Test-Path $marker) 'the upgrade removed files from the data directory'
    Assert (@(Get-SessionBrowsers).Count -eq 0) 'browsers survived the upgrade'
    Assert-DataDirRestricted
    Remove-Item -LiteralPath $marker

    Step 'An error exit is restarted by the SCM'
    Stop-Service tabtenderd
    (Get-Service tabtenderd).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(90))
    # Occupy the port so the daemon fails to start and exits with an error.
    $squatter = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 3000)
    $squatter.Start()
    try {
        try { Start-Service tabtenderd } catch { Write-Host "Start-Service: $_" }
        Wait-Until 'the daemon to fail on the occupied port' 30 {
            Select-String -LiteralPath $LogFile -SimpleMatch 'listen on 127.0.0.1:3000' -Quiet
        }
    } finally { $squatter.Stop() }
    # Nothing starts the service from here on except the SCM's recovery actions.
    Wait-Until 'the SCM to restart the service after an error exit' 90 {
        (Invoke-Api -Url "$url/v1/health" -Key $key).Status -eq 200
    }

    Step 'SCM stop terminates browsers'
    Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $key -Body '{}' | Out-Null
    Stop-Service tabtenderd
    (Get-Service tabtenderd).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(90))
    Wait-Until 'browsers to exit after stop' 30 { @(Get-SessionBrowsers).Count -eq 0 }
    Start-Sleep -Seconds 3
    Assert ((Get-Service tabtenderd).Status -eq 'Stopped') 'the SCM restarted a service that was stopped on request'

    Step 'Without the installer, the service creates a restricted data directory'
    Remove-DataDir
    Start-Service tabtenderd
    Wait-Until 'the API with a service-created data directory' 60 { (Invoke-Api -Url "$url/v1/health").Status -eq 401 }
    Assert-DataDirRestricted
    Assert (Test-Path $KeyFile) 'no initial key file in the service-created data directory'

    Step 'Uninstalling keeps the data unless asked to purge it'
    $stray = Join-Path $InstallDir 'not-ours.txt'
    Set-Content -Path $stray -Value 'not installed by Tabtender'
    Invoke-Wrapper $UninstallCmd | Out-Null
    Assert (-not (Get-Service tabtenderd -ErrorAction SilentlyContinue)) 'the service is still registered'
    Assert (-not (Test-Path (Join-Path $InstallDir 'tabtenderd.exe'))) 'tabtenderd.exe was not removed'
    Assert (Test-Path $stray) 'the uninstaller deleted a file it did not install'
    Assert (Test-Path (Join-Path $SecretsDir 'api-key.dpapi')) 'the uninstaller deleted data without -PurgeData'
    Remove-Item -LiteralPath $stray
    Invoke-Wrapper $UninstallCmd @('-PurgeData') | Out-Null
    Assert (-not (Test-Path $DataDir)) '-PurgeData left the data directory behind'
    Assert (-not (Test-Path $InstallDir)) 'the empty install directory was left behind'

    Step 'Reinstall with an administrator-supplied key, YAML and environment configuration'
    New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
    Set-Content -Path (Join-Path $DataDir 'config.yaml') -Value @"
server:
  port: 3050
sessions:
  defaultTTL: 777
  maxConcurrent: 5
"@
    $adminKey = 'administrator-supplied-key-0123456789abcdef'
    Invoke-Wrapper $InstallCmd @('-BinaryPath', $BinaryPath, '-ApiKey', $adminKey,
        '-Environment', 'TABTENDER_PORT=3100;TABTENDER_MAX_SESSIONS=1') | Out-Null
    $url = 'http://127.0.0.1:3100' # TABTENDER_PORT overrides the file's 3050
    $health = Invoke-Api -Url "$url/v1/health" -Key $adminKey
    Assert ($health.Status -eq 200) "health on the environment-configured port returned $($health.Status)"
    Assert-DataDirRestricted
    Assert (-not (Test-Path (Join-Path $SecretsDir 'api-key.provision'))) 'the provisioning file was not consumed'
    Assert (-not (Test-Path $KeyFile)) 'no initial key file is expected for a supplied key'
    Assert ($health.Json.sessions.max -eq 1) "max sessions is $($health.Json.sessions.max); the environment must override the file"
    $created = Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $adminKey -Body '{}'
    Assert ($created.Status -eq 201) "create session returned $($created.Status): $($created.Json | ConvertTo-Json -Compress)"
    $ttl = ([datetime]$created.Json.expiresAt - [datetime]$created.Json.createdAt).TotalSeconds
    # The TTL starts once the browser is ready, so launch time comes on top.
    Assert ($ttl -ge 776 -and $ttl -lt 870) "session lifetime is $ttl, want the file-configured 777 plus launch time"
    $second = Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $adminKey -Body '{}'
    Assert ($second.Status -eq 429) "a second session returned $($second.Status), want 429"

    Step 'An upgrade keeps the service environment; a failed start is reported'
    # Port 3100 is taken while the upgraded service starts, so the installer
    # must notice that the daemon never came up instead of reporting success.
    Stop-Service tabtenderd
    (Get-Service tabtenderd).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(90))
    $squatter = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 3100)
    $squatter.Start()
    try {
        $output = Invoke-Wrapper $InstallCmd @('-BinaryPath', $BinaryPath) -AllowFailure
        Assert ($LASTEXITCODE -ne 0) 'the installer reported success although the daemon could not bind its port'
        Assert ($output -match 'listen on 127\.0\.0\.1:3100') 'the installer did not show the log tail explaining the failure'
    } finally { $squatter.Stop() }

    Step 'Final uninstall leaves nothing behind'
    Invoke-Wrapper $UninstallCmd @('-PurgeData') | Out-Null
    Assert (-not (Get-Service tabtenderd -ErrorAction SilentlyContinue)) 'the service is still registered'
    Assert (-not (Test-Path $DataDir)) 'the data directory was left behind'
    Assert (-not (Test-Path $InstallDir)) 'the install directory was left behind'
    Assert (@(Get-SessionBrowsers).Count -eq 0) 'browsers were left running'

    $failure = $null
} catch {
    $failure = "$_`n$($_.ScriptStackTrace)"
}

if ($failure) {
    Write-Host "`n$failure`n"
    try { Write-Diagnostics } catch { Write-Host "(diagnostics failed: $($_.Exception.Message))" }
}

Write-Host "`n==> Cleanup"
try {
    $ErrorActionPreference = 'Continue'
    & $UninstallCmd -PurgeData 2>&1 | ForEach-Object { Write-Host "$_" }
} catch {
    Write-Host "(cleanup failed: $($_.Exception.Message))"
}

if ($failure) {
    Write-Host "`nE2E FAILED: $failure"
    exit 1
}
Write-Host "`nALL STEPS PASSED: Windows service lifecycle"
exit 0
