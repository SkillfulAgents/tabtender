<#
.SYNOPSIS
    End-to-end test of tabtenderd as a native Windows Service.

.DESCRIPTION
    Installs the service with install.ps1, drives real headless Chrome
    sessions through it with Playwright and Puppeteer, and checks SCM
    lifecycle, key provisioning, YAML and environment configuration, and
    cleanup. Must run elevated. Uninstalls the service when it finishes.

.PARAMETER BinaryPath
    The tabtenderd.exe under test.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$BinaryPath
)

$ErrorActionPreference = 'Stop'
$Root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$DataDir = Join-Path $env:ProgramData 'Tabtender'
$Install = Join-Path $PSScriptRoot 'install.ps1'
$Uninstall = Join-Path $PSScriptRoot 'uninstall.ps1'

function Step($message) { Write-Host "`n==> $message" }
function Assert($condition, $message) { if (-not $condition) { throw "FAIL: $message" } }

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

function Get-SessionBrowsers {
    Get-CimInstance Win32_Process -Filter "Name = 'chrome.exe'" |
        Where-Object { $_.CommandLine -like '*Tabtender\profiles*' }
}

try {
    & $Uninstall -PurgeData | Out-Null

    Step 'Install the service (generated API key)'
    & $Install -BinaryPath $BinaryPath
    $service = Get-Service tabtenderd
    Assert ($service.Status -eq 'Running') "service status is $($service.Status)"
    Assert ($service.StartType -eq 'Automatic') "service start type is $($service.StartType)"
    $account = (Get-CimInstance Win32_Service -Filter "Name = 'tabtenderd'").StartName
    Assert ($account -eq 'LocalSystem') "service runs as $account"
    $url = 'http://127.0.0.1:3000'
    Wait-Until 'the API to listen' 60 { (Invoke-Api -Url "$url/v1/health").Status -eq 401 }

    Step 'One-time retrieval of the generated key'
    $keyFile = Join-Path $DataDir 'secrets\initial-api-key.txt'
    Assert (Test-Path $keyFile) 'the initial key file was not created'
    $key = (Get-Content $keyFile -Raw).Trim()
    $acl = (Get-Acl (Join-Path $DataDir 'secrets')).Access | ForEach-Object { $_.IdentityReference.Value }
    Assert (-not ($acl -match 'Users|Everyone')) "the secrets directory is accessible to: $($acl -join ', ')"
    $health = Invoke-Api -Url "$url/v1/health" -Key $key
    Assert ($health.Status -eq 200) "health with the generated key returned $($health.Status)"
    Assert ($health.Json.browser.available) "no browser available to the service: $($health.Json.browser.error)"
    Assert (-not (Test-Path $keyFile)) 'the initial key file must be deleted after first use'
    Assert (Test-Path (Join-Path $DataDir 'secrets\api-key.dpapi')) 'the key is not in DPAPI storage'

    Step 'Sessions are native headless Chrome processes'
    $created = Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $key -Body '{}'
    Assert ($created.Status -eq 201) "create session returned $($created.Status): $($created.Json | ConvertTo-Json -Compress)"
    Assert ($created.Json.headless -eq $true) 'a Windows Service session must be headless'
    Assert (@(Get-SessionBrowsers).Count -ge 1) 'no chrome.exe process found for the session'
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

    Step 'SCM stop terminates browsers'
    Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $key -Body '{}' | Out-Null
    Stop-Service tabtenderd
    (Get-Service tabtenderd).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(90))
    Wait-Until 'browsers to exit after stop' 30 { @(Get-SessionBrowsers).Count -eq 0 }

    Step 'Reinstall with an administrator-supplied key, YAML and environment configuration'
    & $Uninstall -PurgeData
    New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
    Set-Content -Path (Join-Path $DataDir 'config.yaml') -Value @"
server:
  port: 3050
sessions:
  defaultTTL: 777
  maxConcurrent: 5
"@
    $adminKey = 'administrator-supplied-key-0123456789abcdef'
    & $Install -BinaryPath $BinaryPath -ApiKey $adminKey -Environment @{ TABTENDER_PORT = '3100'; TABTENDER_MAX_SESSIONS = '1' }
    $url = 'http://127.0.0.1:3100' # TABTENDER_PORT overrides the file's 3050
    Wait-Until 'the API on the environment-configured port' 60 { (Invoke-Api -Url "$url/v1/health" -Key $adminKey).Status -eq 200 }
    Assert (-not (Test-Path (Join-Path $DataDir 'secrets\api-key.provision'))) 'the provisioning file was not consumed'
    Assert (-not (Test-Path (Join-Path $DataDir 'secrets\initial-api-key.txt'))) 'no initial key file is expected for a supplied key'
    $health = Invoke-Api -Url "$url/v1/health" -Key $adminKey
    Assert ($health.Json.sessions.max -eq 1) "max sessions is $($health.Json.sessions.max); the environment must override the file"
    $created = Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $adminKey -Body '{}'
    Assert ($created.Status -eq 201) "create session returned $($created.Status)"
    $ttl = ([datetime]$created.Json.expiresAt - [datetime]$created.Json.createdAt).TotalSeconds
    # The TTL starts once the browser is ready, so launch time comes on top.
    Assert ($ttl -ge 776 -and $ttl -lt 870) "session lifetime is $ttl, want the file-configured 777 plus launch time"
    $second = Invoke-Api -Method POST -Url "$url/v1/sessions" -Key $adminKey -Body '{}'
    Assert ($second.Status -eq 429) "a second session returned $($second.Status), want 429"

    Write-Host "`nPASS: Windows service lifecycle"
} catch {
    Write-Host "--- service log tail ---"
    $log = Join-Path $DataDir 'logs\tabtenderd.log'
    if (Test-Path $log) { Get-Content $log -Tail 60 }
    throw
} finally {
    & $Uninstall -PurgeData
}
