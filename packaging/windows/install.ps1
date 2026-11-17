<#
.SYNOPSIS
    Installs or upgrades tabtenderd as a native Windows Service.

.DESCRIPTION
    Copies tabtenderd.exe to the install directory, prepares the data
    directory (%ProgramData%\Tabtender), registers the service with the
    Service Control Manager (automatic start at boot, restart on failure),
    starts it, and waits until the API answers.

    The service runs as its own least-privilege virtual account,
    NT SERVICE\tabtenderd. The data directory is accessible only to that
    account, SYSTEM and Administrators.

    Running the installer again upgrades in place: the service is stopped,
    the executable replaced, and the service reconfigured and restarted. The
    API key, configuration, and browser profiles are kept.

    Run elevated. Downloaded scripts are blocked by the default execution
    policy; install.cmd next to this script runs it with the policy bypassed.

.PARAMETER BinaryPath
    The tabtenderd.exe to install. Defaults to tabtenderd.exe next to this script.

.PARAMETER InstallDir
    Where the executable is installed. Defaults to %ProgramFiles%\Tabtender.
    The directory must be used for nothing else: the installer takes it over
    and makes it read-only for everyone but SYSTEM and Administrators. A
    custom location must also sit below directories that only administrators
    can modify; the installer does not check its parents.

.PARAMETER ApiKey
    Optional administrator-supplied API key (at least 32 characters). It is
    handed to the service through a provisioning file that the service imports
    into DPAPI-protected storage and deletes on start. When omitted on a fresh
    install, the service generates a key; read it once from
    %ProgramData%\Tabtender\secrets\initial-api-key.txt (the file is removed
    after the key is first used).

.PARAMETER Environment
    Optional TABTENDER_* environment variables for the service, either as a
    hashtable, @{ TABTENDER_PORT = '8080' }, or as text that also works from
    install.cmd, "TABTENDER_PORT=8080;TABTENDER_MAX_SESSIONS=4". When omitted
    on an upgrade the existing variables are kept. Do not pass secrets here:
    service environment blocks are readable by local users.

.PARAMETER NoStart
    Register the service without starting it.

.EXAMPLE
    .\install.cmd

.EXAMPLE
    .\install.ps1 -ApiKey (Get-Content .\key.txt) -Environment @{ TABTENDER_PORT = '8080' }
#>
[CmdletBinding()]
param(
    [string]$BinaryPath = (Join-Path $PSScriptRoot 'tabtenderd.exe'),
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'Tabtender'),
    [string]$ApiKey,
    [object[]]$Environment,
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest's progress bar is slow on Windows PowerShell

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
$ServiceAccount = "NT SERVICE\$ServiceName"
$ServiceKey = "HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName"
$StartTimeoutSeconds = 90

$SidType = [Security.Principal.SecurityIdentifier]
$SystemSid = 'S-1-5-18'
$AdministratorsSid = 'S-1-5-32-544'
$UsersSid = 'S-1-5-32-545'
$ReadAndExecute = 0x1200a9

# --- Helpers -----------------------------------------------------------------

function Invoke-Native {
    param([string]$File, [string[]]$Arguments)
    # Windows PowerShell turns redirected stderr output into terminating
    # errors under 'Stop'; the exit code is what decides here.
    $ErrorActionPreference = 'Continue'
    $output = & $File @Arguments 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0) {
        throw "$File $($Arguments -join ' ') failed ($LASTEXITCODE): $($output.Trim())"
    }
    $output
}

function Resolve-SidName {
    param([string]$Sid)
    try { (New-Object $SidType $Sid).Translate([Security.Principal.NTAccount]).Value } catch { $Sid }
}

function ConvertTo-EnvironmentTable {
    param([object[]]$Values)
    $table = @{}
    foreach ($value in $Values) {
        if ($value -is [Collections.IDictionary]) {
            foreach ($name in $value.Keys) { $table[[string]$name] = [string]$value[$name] }
            continue
        }
        foreach ($pair in ([string]$value -split ';')) {
            if (-not $pair.Trim()) { continue }
            $name, $text = $pair -split '=', 2
            if ($null -eq $text) { throw "Expected NAME=value in -Environment, got '$pair'" }
            $table[$name.Trim()] = $text
        }
    }
    $table
}

# Reads owner and permissions through .NET. Get-Acl would do, but it lives in
# a module, and this script should depend on as few of those as it can.
function Get-Security {
    param([string]$Path)
    $sections = [Security.AccessControl.AccessControlSections]'Owner, Access'
    if (Test-Path -LiteralPath $Path -PathType Container) {
        New-Object Security.AccessControl.DirectorySecurity $Path, $sections
    } else {
        New-Object Security.AccessControl.FileSecurity $Path, $sections
    }
}

# Reports whether a directory is cut off from its parent's permissions, is
# owned by a trusted account, and grants nothing beyond read access to
# anybody else.
function Test-Restricted {
    param([string]$Path, [string[]]$ReadOnlySids)
    $acl = Get-Security $Path
    if (-not $acl.AreAccessRulesProtected) { return $false }
    if ($acl.GetOwner($SidType).Value -notin $TrustedSids) { return $false }
    foreach ($rule in $acl.GetAccessRules($true, $true, $SidType)) {
        if ($rule.AccessControlType -ne 'Allow') { return $false }
        $sid = $rule.IdentityReference.Value
        if ($sid -in $TrustedSids) { continue }
        $readOnly = ([int]$rule.FileSystemRights -band (-bnot $ReadAndExecute)) -eq 0
        if ($sid -in $ReadOnlySids -and $readOnly) { continue }
        return $false
    }
    $true
}

# Lists everything below a directory that is a link or is owned by an
# untrusted account. The owner of a file can always rewrite it and its
# permissions, so such entries stay under that account's control even inside
# a restricted directory.
function Get-UntrustedEntry {
    param([string]$Path)
    $pending = New-Object Collections.Stack
    $pending.Push($Path)
    while ($pending.Count -gt 0) {
        foreach ($entry in Get-ChildItem -LiteralPath $pending.Pop() -Force) {
            if ($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) {
                "$($entry.FullName) (a link)"
                continue
            }
            $owner = (Get-Security $entry.FullName).GetOwner($SidType).Value
            if ($owner -notin $TrustedSids) {
                "$($entry.FullName) (owned by $(Resolve-SidName $owner))"
            } elseif ($entry.PSIsContainer) {
                $pending.Push($entry.FullName)
            }
        }
    }
}

# Creates the directory with the given security descriptor, or takes over an
# existing one.
#
# Standard users can create directories under %ProgramData% and elsewhere. A
# user who creates ours first stays its owner and could plant a configuration
# file, a helper executable, or an API key for the service to act on. An
# existing directory is therefore accepted only if a trusted account owns it
# and, unless an earlier install already restricted it, everything inside it.
function Initialize-RestrictedDirectory {
    param([string]$Path, [string]$Sddl, [string[]]$ReadOnlySids = @())

    $core = $PSVersionTable.PSEdition -eq 'Core' # .NET moved these methods
    if (-not (Test-Path -LiteralPath $Path)) {
        # Created together with its permissions, so there is no moment at
        # which another account could get into it.
        $security = New-Object Security.AccessControl.DirectorySecurity
        $security.SetSecurityDescriptorSddlForm($Sddl)
        if ($core) {
            [IO.FileSystemAclExtensions]::CreateDirectory($security, $Path) | Out-Null
        } else {
            [IO.Directory]::CreateDirectory($Path, $security) | Out-Null
        }
    } else {
        $item = Get-Item -LiteralPath $Path -Force
        if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "$Path exists and is not a plain directory."
        }
        $owner = (Get-Security $Path).GetOwner($SidType).Value
        if ($owner -notin $TrustedSids) {
            throw ("$Path already exists and is owned by $(Resolve-SidName $owner), not by an administrator. " +
                'Its contents cannot be trusted. Inspect and delete it, then run the installer again.')
        }
        $wasRestricted = Test-Restricted $Path $ReadOnlySids

        # Restrict first, then inspect: once the permissions are in place only
        # owners of existing entries can still change the contents, and those
        # are exactly what the inspection finds.
        $dacl = New-Object Security.AccessControl.DirectorySecurity
        $dacl.SetSecurityDescriptorSddlForm($Sddl, [Security.AccessControl.AccessControlSections]::Access)
        $dacl.SetAccessRuleProtection($true, $false)
        # An upgrade normally finds the permissions as it left them. Writing
        # them again would needlessly re-propagate them through a tree the
        # service account has been writing to.
        $wanted = $dacl.GetAccessRules($true, $false, $SidType) | ForEach-Object { $_.IdentityReference.Value } | Sort-Object -Unique
        $present = (Get-Security $Path).GetAccessRules($true, $true, $SidType) | ForEach-Object { $_.IdentityReference.Value } | Sort-Object -Unique
        if (-not $wasRestricted -or "$wanted" -ne "$present") {
            if ($core) {
                [IO.FileSystemAclExtensions]::SetAccessControl([IO.DirectoryInfo]$Path, $dacl)
            } else {
                [IO.Directory]::SetAccessControl($Path, $dacl)
            }
        }
        Invoke-Native icacls @($Path, '/setowner', "*$AdministratorsSid", '/L', '/Q') | Out-Null

        if (-not $wasRestricted) {
            $untrusted = @(Get-UntrustedEntry $Path)
            if ($untrusted.Count -gt 0) {
                # Leave it recognizably unrestricted so the next run inspects it again.
                Invoke-Native icacls @($Path, '/inheritance:e', '/Q') | Out-Null
                throw ("$Path contains entries that were not created by an administrator and may have been planted:`n  " +
                    (($untrusted | Select-Object -First 10) -join "`n  ") +
                    "`nInspect and delete them, then run the installer again.")
            }
            # Everything inside is trusted and free of links: drop leftover
            # explicit permissions and hand ownership to Administrators.
            # (Never recurse into a directory that was already restricted:
            # the service account may have created links there.)
            if (Get-ChildItem -LiteralPath $Path -Force | Select-Object -First 1) {
                Invoke-Native icacls @((Join-Path $Path '*'), '/reset', '/T', '/L', '/Q') | Out-Null
            }
            Invoke-Native icacls @($Path, '/setowner', "*$AdministratorsSid", '/T', '/L', '/Q') | Out-Null
        }
    }

    if (-not (Test-Restricted $Path $ReadOnlySids)) {
        throw "Could not restrict access to $Path. Current permissions:`n$(Invoke-Native icacls @($Path))"
    }
}

function Stop-ServiceForUpgrade {
    # Disabled for the duration, so the SCM's recovery actions cannot restart
    # the service halfway through. Registration sets it back to automatic.
    Set-Service -Name $ServiceName -StartupType Disabled
    $processId = (Get-CimInstance Win32_Service -Filter "Name = '$ServiceName'").ProcessId
    $service = Get-Service -Name $ServiceName
    if ($service.Status -ne 'Stopped') {
        Write-Host "Stopping the running $ServiceName service"
        Stop-Service -Name $ServiceName -Force -WarningAction SilentlyContinue
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(120))
    }
    # The executable stays locked until the process is really gone, which is
    # shortly after the service reports that it stopped.
    $deadline = (Get-Date).AddSeconds(30)
    while ($processId -and (Get-Process -Id $processId -ErrorAction SilentlyContinue)) {
        if ((Get-Date) -gt $deadline) { throw "The $ServiceName process ($processId) did not exit." }
        Start-Sleep -Milliseconds 200
    }
}

function Register-Service {
    param([string]$Executable, [bool]$Exists)
    $command = "`"$Executable`" run"
    if ($Exists) {
        $current = Get-CimInstance Win32_Service -Filter "Name = '$ServiceName'"
        $result = Invoke-CimMethod -InputObject $current -MethodName Change `
            -Arguments @{ PathName = $command; DisplayName = 'Tabtender'; StartMode = 'Automatic' }
        if ($result.ReturnValue -ne 0) {
            throw "Could not reconfigure the $ServiceName service (Win32_Service.Change returned $($result.ReturnValue))."
        }
    } else {
        New-Service -Name $ServiceName -BinaryPathName $command -DisplayName 'Tabtender' -StartupType Automatic | Out-Null
    }
    Set-Service -Name $ServiceName `
        -Description 'Tabtender browser session server: native browser sessions for AI agents over CDP.'

    # Browsers render arbitrary web content on behalf of remote agents, so
    # the service gets its own unprivileged virtual account, not LocalSystem.
    Invoke-Native sc.exe @('config', $ServiceName, 'obj=', $ServiceAccount) | Out-Null

    # Restart after failures: after 1s, then 5s, then every 30s. The failure
    # flag extends this from crashes to exits that report an error, such as a
    # port that is temporarily in use.
    Invoke-Native sc.exe @('failure', $ServiceName, 'reset=', '86400', 'actions=', 'restart/1000/restart/5000/restart/30000') | Out-Null
    Invoke-Native sc.exe @('failureflag', $ServiceName, '1') | Out-Null
}

function Get-ServiceEnvironment {
    $table = @{}
    $block = (Get-ItemProperty -Path $ServiceKey -ErrorAction SilentlyContinue).Environment
    foreach ($line in $block) {
        $name, $text = $line -split '=', 2
        if ($name) { $table[$name] = $text }
    }
    $table
}

function Set-ServiceEnvironment {
    param([hashtable]$Variables)
    if ($Variables.Count -gt 0) {
        $block = $Variables.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }
        Set-ItemProperty -Path $ServiceKey -Name Environment -Type MultiString -Value ([string[]]$block)
    } else {
        Remove-ItemProperty -Path $ServiceKey -Name Environment -ErrorAction SilentlyContinue
    }
}

function Get-HttpStatus {
    param([string]$Url)
    try {
        [int](Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 5).StatusCode
    } catch {
        # Error statuses surface as exceptions that carry the response.
        $response = $_.Exception.Response
        if ($response) { [int]$response.StatusCode } else { 0 }
    }
}

# Starts the service and waits until its API answers. The SCM reports the
# service as running before the daemon has loaded its configuration and bound
# its port, so "running" alone says little. Returns the API's base URL.
function Start-ServiceAndWait {
    Start-Service -Name $ServiceName
    $firstProcessId = (Get-CimInstance Win32_Service -Filter "Name = '$ServiceName'").ProcessId
    $deadline = (Get-Date).AddSeconds($StartTimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $service = Get-CimInstance Win32_Service -Filter "Name = '$ServiceName'"
        if ($service.State -eq 'Stopped' -or $service.ProcessId -ne $firstProcessId) {
            throw "The $ServiceName service exited during startup."
        }
        # The port is whatever the configuration file and environment made
        # it; ask the system what the service process listens on.
        $listeners = Get-NetTCPConnection -State Listen -OwningProcess $service.ProcessId -ErrorAction SilentlyContinue
        foreach ($listener in $listeners) {
            $address = $listener.LocalAddress
            if ($address -eq '0.0.0.0') { $address = '127.0.0.1' }
            elseif ($address -eq '::') { $address = '[::1]' }
            elseif ($address -like '*:*') { $address = "[$address]" }
            $url = "http://${address}:$($listener.LocalPort)"
            # Unauthenticated, so a healthy daemon answers 401.
            if ((Get-HttpStatus "$url/v1/health") -in 200, 401) { return $url }
        }
        Start-Sleep -Milliseconds 500
    }
    throw "The $ServiceName service did not answer on its API within $StartTimeoutSeconds seconds."
}

function Write-StartDiagnostics {
    param([string]$LogFile)
    $ErrorActionPreference = 'Continue'
    Write-Host '--- service status ---'
    & sc.exe queryex $ServiceName | ForEach-Object { Write-Host $_ }
    Write-Host '--- recent service events ---'
    # Best effort: a problem reading the event log must not replace the
    # error being diagnosed.
    try {
        Get-WinEvent -LogName System -MaxEvents 300 -ErrorAction SilentlyContinue |
            Where-Object { $_.ProviderName -eq 'Service Control Manager' -and $_.Message -match "Tabtender|$ServiceName" } |
            Select-Object -First 5 | ForEach-Object { Write-Host "$($_.TimeCreated.ToString('s')) $($_.Message)" }
        Get-WinEvent -LogName Application -MaxEvents 300 -ErrorAction SilentlyContinue |
            Where-Object { $_.ProviderName -eq $ServiceName } |
            Select-Object -First 3 | ForEach-Object { Write-Host "$($_.TimeCreated.ToString('s')) $($_.Message)" }
    } catch {
        Write-Host "(could not read the event log: $($_.Exception.Message))"
    }
    Write-Host "--- $LogFile (tail) ---"
    if (Test-Path -LiteralPath $LogFile) {
        Get-Content -LiteralPath $LogFile -Tail 40 | ForEach-Object { Write-Host $_ }
    } else {
        Write-Host '(no log file was written)'
    }
}

# --- Validation --------------------------------------------------------------

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'The installer must be run elevated (as Administrator).'
}
if (-not (Test-Path -LiteralPath $BinaryPath -PathType Leaf)) {
    throw "tabtenderd.exe not found at $BinaryPath"
}
$BinaryPath = (Resolve-Path -LiteralPath $BinaryPath).ProviderPath
if (-not [IO.Path]::IsPathRooted($InstallDir)) {
    throw "-InstallDir must be an absolute path, got $InstallDir"
}
if ($ApiKey -and $ApiKey.Length -lt 32) {
    throw 'The API key must be at least 32 characters long.'
}

$upgrade = [bool](Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)
if ($PSBoundParameters.ContainsKey('Environment')) {
    $variables = ConvertTo-EnvironmentTable $Environment
} elseif ($upgrade) {
    $variables = Get-ServiceEnvironment
} else {
    $variables = @{}
}
foreach ($name in $variables.Keys) {
    if ($name -notlike 'TABTENDER_*') {
        throw "Only TABTENDER_* variables may be set for the service, got $name"
    }
    if ($name -in 'TABTENDER_API_KEY', 'TABTENDER_CLOUDFLARE_TUNNEL_TOKEN') {
        throw "$name is a secret; provision it with -ApiKey or a .provision file instead of the service environment."
    }
}

$DataDir = Join-Path $env:ProgramData 'Tabtender'
if ($variables['TABTENDER_DATA_DIR']) { $DataDir = $variables['TABTENDER_DATA_DIR'] }
$secretsDir = Join-Path $DataDir 'secrets'
$logFile = Join-Path $DataDir 'logs\tabtenderd.log'
$exe = Join-Path $InstallDir 'tabtenderd.exe'

# A virtual account's SID is derived from the service name, so it is known
# before the service exists.
if ((Invoke-Native sc.exe @('showsid', $ServiceName)) -notmatch 'S-1-5-80(-\d+)+') {
    throw "Could not determine the service SID of $ServiceName."
}
$ServiceSid = $Matches[0]

# Accounts whose files can be trusted: whoever owns a file controls it.
$TrustedSids = @($SystemSid, $AdministratorsSid, $ServiceSid, $identity.User.Value)

if (Test-Path -LiteralPath $InstallDir) {
    $foreign = @(Get-ChildItem -LiteralPath $InstallDir -Force | Where-Object { $_.Name -ne 'tabtenderd.exe' })
    if ($foreign.Count -gt 0) {
        throw "$InstallDir contains other files ($($foreign[0].Name), ...). Choose a directory that is used only by Tabtender."
    }
}

# --- Installation ------------------------------------------------------------

if ($upgrade) {
    Write-Host "Upgrading the existing $ServiceName service; its data in $DataDir is kept"
    Stop-ServiceForUpgrade
}
try {
    # Full control for SYSTEM and Administrators in both directories. The
    # service account can change its data but only read and run its program.
    $full = "(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
    $read = '(A;OICI;0x{0:x};;;{1})' -f $ReadAndExecute, 'BU'
    $readService = '(A;OICI;0x{0:x};;;{1})' -f $ReadAndExecute, $ServiceSid

    Write-Host "Preparing $DataDir"
    Initialize-RestrictedDirectory $DataDir "O:BAG:BAD:PAI$full(A;OICI;FA;;;$ServiceSid)"

    Write-Host "Installing tabtenderd to $InstallDir"
    Initialize-RestrictedDirectory $InstallDir "O:BAG:BAD:PAI$full$read$readService" @($UsersSid, $ServiceSid)
    if ($BinaryPath -ne $exe) {
        Copy-Item -LiteralPath $BinaryPath -Destination $exe -Force
    }

    if ($ApiKey) {
        # Imported into DPAPI storage and deleted by the service at startup.
        New-Item -ItemType Directory -Force -Path $secretsDir | Out-Null
        [IO.File]::WriteAllText((Join-Path $secretsDir 'api-key.provision'), $ApiKey)
    }
    # Without a stored or supplied key the service generates one and leaves
    # it in a one-time file.
    $expectInitialKey = -not (Test-Path -Path (Join-Path $secretsDir 'api-key.*'))

    Write-Host "Registering the $ServiceName service to run as $ServiceAccount"
    Register-Service $exe $upgrade
    Set-ServiceEnvironment $variables
} catch {
    if ($upgrade) {
        # Do not leave a previously working service disabled.
        Set-Service -Name $ServiceName -StartupType Automatic -ErrorAction SilentlyContinue
    }
    throw
}

$initialKeyFile = Join-Path $secretsDir 'initial-api-key.txt'
if ($NoStart) {
    Write-Host "The $ServiceName service is registered but was not started."
} else {
    Write-Host "Starting the $ServiceName service"
    try {
        $url = Start-ServiceAndWait
        if ($expectInitialKey -and -not (Test-Path -LiteralPath $initialKeyFile)) {
            throw "The service started but did not write $initialKeyFile."
        }
    } catch {
        try { Write-StartDiagnostics $logFile } catch { Write-Host "(diagnostics failed: $($_.Exception.Message))" }
        throw
    }
    Write-Host "Tabtender is running at $url"
}

Write-Host ''
Write-Host "Configuration:  $(Join-Path $DataDir 'config.yaml') (restart the service after editing)"
Write-Host "Log file:       $logFile"
if (Test-Path -LiteralPath $initialKeyFile) {
    Write-Host "API key:        $initialKeyFile (one-time; removed after first use)"
}
