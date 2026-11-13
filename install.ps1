#Requires -Version 5.1
<#
Installer for agent-kanban (github.com/Rishang/agent-kanban) on Windows.

  irm https://raw.githubusercontent.com/Rishang/agent-kanban/main/install.ps1 | iex

Env overrides (set before running):
  $env:VERSION     = "v1.0.0"   install a specific tag (default: latest)
  $env:INSTALL_DIR = "C:\tools" install into this directory (default: %LOCALAPPDATA%\Programs\agent-kanban)

Linux and macOS: use install.sh instead.
#>

function Install-AgentKanban {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'   # Windows PowerShell 5.1 downloads crawl with the progress bar on

    $Repo = 'Rishang/agent-kanban'
    $Binary = 'agent-kanban'

    function Info($msg) { Write-Host "==> $msg" -ForegroundColor Blue }
    function Warn($msg) { Write-Host "warn: $msg" -ForegroundColor Yellow }

    # Windows PowerShell 5.1 defaults to TLS 1.0/1.1, which GitHub rejects.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    # Releases publish windows/amd64 only. Windows on ARM runs it under emulation.
    $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($arch) {
        'AMD64' { }
        'ARM64' { Info 'Windows on ARM: installing the amd64 build, which runs under emulation' }
        default { throw "unsupported architecture: $arch (only 64-bit Windows is supported)" }
    }

    $version = $env:VERSION
    if (-not $version -or $version -eq 'latest') {
        Info 'Resolving latest release...'
        try {
            $version = (Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ 'User-Agent' = 'agent-kanban-installer' }).tag_name
        } catch {
            throw "could not determine the latest release tag (does $Repo have a release yet?): $($_.Exception.Message)"
        }
    }
    if ($version -notmatch '^v\d') { throw "unexpected release tag: $version" }

    # Archive names carry the bare version, tags carry the leading v.
    $archive = "${Binary}_$($version.TrimStart('v'))_windows_amd64.zip"
    $baseUrl = "https://github.com/$Repo/releases/download/$version"

    $installDir = $env:INSTALL_DIR
    if (-not $installDir) { $installDir = Join-Path $env:LOCALAPPDATA "Programs\$Binary" }

    Info "Installing $Binary $version (windows/amd64)"

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("$Binary-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        $zip = Join-Path $tmp $archive
        Info "Downloading $archive"
        try {
            Invoke-WebRequest -Uri "$baseUrl/$archive" -OutFile $zip -UseBasicParsing
        } catch {
            throw "download failed: $baseUrl/$archive"
        }

        # A checksum mismatch aborts. A missing checksums.txt only warns.
        $sumsFile = Join-Path $tmp 'checksums.txt'
        $haveSums = $true
        try { Invoke-WebRequest -Uri "$baseUrl/checksums.txt" -OutFile $sumsFile -UseBasicParsing } catch { $haveSums = $false }
        if ($haveSums) {
            $line = Get-Content $sumsFile | Where-Object { $_ -match "\s$([regex]::Escape($archive))$" } | Select-Object -First 1
            if (-not $line) { throw "$archive is not listed in checksums.txt" }
            $expected = ($line -split '\s+')[0].ToLower()
            $actual = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash.ToLower()
            if ($expected -ne $actual) { throw "checksum mismatch for $archive" }
            Info 'Checksum verified'
        } else {
            Warn 'no checksums.txt in the release, skipping checksum verification'
        }

        $extract = Join-Path $tmp 'out'
        Expand-Archive -Path $zip -DestinationPath $extract -Force
        $exe = Join-Path $extract "$Binary.exe"
        if (-not (Test-Path $exe)) { throw "$Binary.exe not found inside $archive" }

        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
        $target = Join-Path $installDir "$Binary.exe"
        try {
            Copy-Item -Path $exe -Destination $target -Force
        } catch {
            throw "could not write $target. If agent-kanban is running, stop it and run the installer again."
        }
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }

    Write-Host ''
    Info "Installed $Binary $version to $target"

    # Add to the user PATH (persisted) and to this session, so the command works right away.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = @()
    if ($userPath) { $entries = $userPath -split ';' | Where-Object { $_ } }
    if ($entries -notcontains $installDir) {
        [Environment]::SetEnvironmentVariable('Path', (($entries + $installDir) -join ';'), 'User')
        Info "Added $installDir to your user PATH (open a new terminal for other windows to see it)"
    }
    if (($env:Path -split ';') -notcontains $installDir) { $env:Path = "$env:Path;$installDir" }

    Info "Next: cd your-project; $Binary   (then: $Binary skill sync)"
}

Install-AgentKanban
