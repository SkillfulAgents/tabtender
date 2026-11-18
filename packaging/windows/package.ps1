<#
.SYNOPSIS
  Builds the Windows release archives.

.DESCRIPTION
  Runs in two steps so that the executables can be signed in between:

    package.ps1 -Version 0.1.0 -Step Build     # dist/tabtender-windows-<arch>/
    (sign dist/tabtender-windows-*/tabtenderd.exe)
    package.ps1 -Version 0.1.0 -Step Archive   # dist/tabtender-windows-<arch>.zip

  Without -Step both run, which gives unsigned archives.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $Version,
    [ValidateSet('Build', 'Archive', 'All')] [string] $Step = 'All'
)

$ErrorActionPreference = 'Stop'
$root = Resolve-Path (Join-Path $PSScriptRoot '..\..')
$dist = Join-Path $root 'dist'
$architectures = 'amd64', 'arm64'

function Get-StageDirectory([string] $Arch) {
    Join-Path $dist "tabtender-windows-$Arch"
}

if ($Step -in 'Build', 'All') {
    $ldflags = "-s -w -X github.com/SkillfulAgents/tabtender/internal/version.Version=$Version"
    foreach ($arch in $architectures) {
        $stage = Get-StageDirectory $arch
        if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
        New-Item -ItemType Directory -Force -Path $stage | Out-Null

        $env:GOOS = 'windows'
        $env:GOARCH = $arch
        $env:CGO_ENABLED = '0'
        Push-Location $root
        try {
            go build -trimpath -ldflags $ldflags -o (Join-Path $stage 'tabtenderd.exe') ./cmd/tabtenderd
            if ($LASTEXITCODE -ne 0) { throw "go build failed for windows/$arch" }
        } finally {
            Pop-Location
            Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
        }

        foreach ($file in 'install.ps1', 'install.cmd', 'uninstall.ps1', 'uninstall.cmd') {
            Copy-Item (Join-Path $PSScriptRoot $file) -Destination $stage
        }
        Copy-Item (Join-Path $root 'LICENSE') -Destination $stage
        bash (Join-Path $root 'packaging/third-party-notices.sh') (Join-Path $stage 'THIRD_PARTY_NOTICES.txt')
        if ($LASTEXITCODE -ne 0) { throw 'collecting license notices failed' }
    }
}

if ($Step -in 'Archive', 'All') {
    foreach ($arch in $architectures) {
        $stage = Get-StageDirectory $arch
        if (-not (Test-Path (Join-Path $stage 'tabtenderd.exe'))) {
            throw "$stage has not been built; run -Step Build first"
        }
        $archive = Join-Path $dist "tabtender-windows-$arch.zip"
        if (Test-Path $archive) { Remove-Item -Force $archive }
        Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $archive
        Write-Host "==> $archive"
    }
}
