<#
.SYNOPSIS
    AMKR 安装脚本（Windows）。

.DESCRIPTION
    从 GitHub Releases 下载 amkr.exe，用发布页上的 checksums.txt 校验 sha256，
    安装到 %LOCALAPPDATA%\Programs\AutoModelKeyRouter\amkr.exe（不需要管理员权限）。
    校验不通过会立刻中止——绝不安装校验不过的文件。

    兼容 PowerShell 5.1+（Windows 10/11 自带的 Windows PowerShell 即可）。

.EXAMPLE
    irm https://raw.githubusercontent.com/Sparrived/auto-model-key-router/master/scripts/install.ps1 | iex

.EXAMPLE
    .\install.ps1 -Version 5.0.0

.EXAMPLE
    .\install.ps1 -InstallDir "$env:USERPROFILE\bin"
#>
[CmdletBinding()]
param(
    # 要安装的版本号，例如 5.0.0（默认取 GitHub 上最新的 release）。
    [string]$Version = '',

    # 安装目录（默认 %LOCALAPPDATA%\Programs\AutoModelKeyRouter）。
    [string]$InstallDir = ''
)

$ErrorActionPreference = 'Stop'

$Repo = 'Sparrived/auto-model-key-router'
$Program = 'amkr'
$ApiLatest = "https://api.github.com/repos/$Repo/releases/latest"
$ReleaseBase = "https://github.com/$Repo/releases/download"

function Write-Step([string]$Message) { Write-Host $Message }

function Fail([string]$Message) { throw "amkr 安装失败: $Message" }

# Windows PowerShell 5.1 的默认安全协议可能还是 TLS 1.0，而 GitHub 只收 TLS 1.2+。
$currentProtocol = [Net.ServicePointManager]::SecurityProtocol
[Net.ServicePointManager]::SecurityProtocol = $currentProtocol -bor [Net.SecurityProtocolType]::Tls12

function Get-AmrkArch {
    $arch = $env:PROCESSOR_ARCHITECTURE
    # 32 位 PowerShell 跑在 64 位系统上时 PROCESSOR_ARCHITECTURE 是 x86，
    # 真实架构在 PROCESSOR_ARCHITEW6432 里。
    if ($env:PROCESSOR_ARCHITEW6432) { $arch = $env:PROCESSOR_ARCHITEW6432 }
    switch ($arch) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default { Fail "不支持的 CPU 架构: $arch（发布物只有 amd64 与 arm64）" }
    }
}

function Get-RemoteFile([string]$Url, [string]$Destination) {
    Invoke-WebRequest -Uri $Url -OutFile $Destination -UseBasicParsing
}

# Get-ExpectedHash 从 checksums.txt 里取出某个文件的 sha256。
# 行格式与 `sha256sum` 一致：<hash>  <name>（二进制模式是 <hash> *<name>）。
function Get-ExpectedHash([string]$ChecksumsPath, [string]$AssetName) {
    foreach ($line in Get-Content -LiteralPath $ChecksumsPath) {
        $parts = $line -split '\s+', 2
        if ($parts.Count -lt 2) { continue }
        $name = $parts[1].Trim().TrimStart('*')
        if ($name -eq $AssetName) {
            return $parts[0].Trim().ToLowerInvariant()
        }
    }
    return $null
}

if (-not $Version) {
    Write-Step '未指定版本，查询 GitHub 上的最新 release...'
    $release = Invoke-RestMethod -Uri $ApiLatest -Headers @{ 'User-Agent' = 'amkr-installer' } -UseBasicParsing
    $Version = $release.tag_name
    if (-not $Version) { Fail '无法解析最新版本，请显式传 -Version X.Y.Z' }
}
# 允许 -Version v5.0.0 这种带 v 的写法。
$Version = $Version.TrimStart('v')

$arch = Get-AmrkArch
$asset = "${Program}_${Version}_windows_${arch}.exe"
$assetUrl = "$ReleaseBase/v$Version/$asset"
$checksumsUrl = "$ReleaseBase/v$Version/checksums.txt"

$workdir = Join-Path $env:TEMP ('amkr-install-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $workdir | Out-Null

try {
    $assetPath = Join-Path $workdir $asset
    $checksumsPath = Join-Path $workdir 'checksums.txt'

    Write-Step "下载 $asset（v$Version，windows/$arch）..."
    try {
        Get-RemoteFile -Url $assetUrl -Destination $assetPath
    } catch {
        Fail "下载失败: $assetUrl（该版本或平台可能没有发布物）：$($_.Exception.Message)"
    }
    try {
        Get-RemoteFile -Url $checksumsUrl -Destination $checksumsPath
    } catch {
        Fail "下载校验和失败: $checksumsUrl：$($_.Exception.Message)"
    }

    $expected = Get-ExpectedHash -ChecksumsPath $checksumsPath -AssetName $asset
    if (-not $expected) { Fail "checksums.txt 里没有 $asset 的记录，拒绝安装" }
    $actual = (Get-FileHash -LiteralPath $assetPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        Fail "sha256 校验失败：$asset 期望 $expected，实际 $actual（下载可能被篡改或损坏）"
    }
    Write-Step "sha256 校验通过: $actual"

    if (-not $InstallDir) {
        if (-not $env:LOCALAPPDATA) { Fail 'LOCALAPPDATA 为空，请用 -InstallDir 指定安装目录' }
        $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\AutoModelKeyRouter'
    }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $installed = Join-Path $InstallDir "$Program.exe"
    Copy-Item -LiteralPath $assetPath -Destination $installed -Force

    Write-Step "已安装: $installed（v$Version）"
    $versionOutput = & $installed --version 2>$null
    if ($LASTEXITCODE -eq 0 -and $versionOutput) {
        Write-Step "自检: $versionOutput"
    } else {
        Write-Step "提示: 无法执行 $installed --version，请手动确认该二进制能在本机运行。"
    }

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $alreadyOnPath = $false
    if ($userPath) {
        foreach ($entry in $userPath.Split(';')) {
            if ($entry.TrimEnd('\') -eq $InstallDir.TrimEnd('\')) { $alreadyOnPath = $true }
        }
    }
    if (-not $alreadyOnPath) {
        Write-Step "提示: $InstallDir 不在用户 PATH 里。只对当前会话生效："
        Write-Step "  `$env:Path = `"$InstallDir;`$env:Path`""
        Write-Step '永久加入用户 PATH（可选，不需要管理员）：'
        Write-Step "  [Environment]::SetEnvironmentVariable('Path', `"$InstallDir;`" + [Environment]::GetEnvironmentVariable('Path','User'), 'User')"
    }
} finally {
    Remove-Item -LiteralPath $workdir -Recurse -Force -ErrorAction SilentlyContinue
}
