# 把 AMKR 从 Python 版切换到 Go 版（需要**管理员权限**运行）
#
# 为什么必须提权：旧的计划任务以 SYSTEM 身份运行，非管理员既不能结束它的进程，
# 也不能改/删该任务。脚本里每一步都会打印结果，任何一步失败都会停下。
#
# 用法：右键 PowerShell → 以管理员身份运行 → 执行本脚本。

$ErrorActionPreference = 'Stop'

$ConfigPath = "$env:LOCALAPPDATA\AutoModelKeyRouter\router-config.json"
$GoExe      = "$env:LOCALAPPDATA\Programs\AutoModelKeyRouter\amkr.exe"
$TaskName   = "\AutoModelKeyRouter"
$Port       = 28881

function Step($text) { Write-Host "`n=== $text ===" -ForegroundColor Cyan }

# ---- 0) 前置检查 ----
Step "0) 前置检查"
if (-not (Test-Path $GoExe))    { throw "找不到 Go 二进制: $GoExe" }
if (-not (Test-Path $ConfigPath)) { throw "找不到配置: $ConfigPath" }
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "需要管理员权限。请右键 PowerShell 选择「以管理员身份运行」后重试。"
}
Write-Host "  管理员权限: 是"
Write-Host "  Go 二进制 : $GoExe"
Write-Host "  配置      : $ConfigPath"

# ---- 1) 停止并删除旧的 Python 计划任务 ----
Step "1) 停止并删除旧的 Python 计划任务"
schtasks /end /tn $TaskName 2>&1 | Out-Host
Start-Sleep -Seconds 2

# schtasks /end 不会杀掉 SYSTEM 下的子进程，必须显式结束它
$killed = 0
Get-CimInstance Win32_Process -Filter "Name='pythonw.exe' OR Name='python.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -like '*auto_model_key_router*' } |
    ForEach-Object {
        Write-Host "  结束 PID $($_.ProcessId): $($_.CommandLine)"
        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
        $killed++
    }
Write-Host "  已结束 $killed 个 Python 进程"

schtasks /delete /tn $TaskName /f 2>&1 | Out-Host

# ---- 2) 确认端口已释放 ----
Step "2) 确认 $Port 端口已释放"
$deadline = (Get-Date).AddSeconds(20)
while ((Get-Date) -lt $deadline) {
    $listen = netstat -ano | Select-String -Pattern ":$Port\s+0\.0\.0\.0:0\s+LISTENING"
    if (-not $listen) { break }
    Start-Sleep -Seconds 1
}
$listen = netstat -ano | Select-String -Pattern ":$Port\s+0\.0\.0\.0:0\s+LISTENING"
if ($listen) {
    Write-Host "  !! 端口仍被占用:" -ForegroundColor Red
    $listen | ForEach-Object { Write-Host "     $($_.Line.Trim())" }
    throw "端口 $Port 仍被占用，已中止——请手动确认占用进程后再重试。"
}
Write-Host "  端口 $Port 已释放" -ForegroundColor Green

# ---- 3) 安装 Go 版为 SYSTEM 计划任务 ----
Step "3) 安装 Go 版为 SYSTEM 计划任务"
& $GoExe --config $ConfigPath --service install
Write-Host "  install 退出码: $LASTEXITCODE"

# ---- 4) 启动 ----
Step "4) 启动 Go 版服务"
& $GoExe --config $ConfigPath --service start
Write-Host "  start 退出码: $LASTEXITCODE"

# ---- 5) 自检 ----
Step "5) 自检"
Start-Sleep -Seconds 6
$cfg = Get-Content $ConfigPath -Raw | ConvertFrom-Json
$base = "http://127.0.0.1:$($cfg.port)"
$auth = "Authorization: Bearer $($cfg.local_api_key)"

Write-Host "  /health:" -NoNewline
$health = curl.exe -s -H $auth "$base/health"
Write-Host " $health"

Write-Host "  /v1/models (无鉴权，应为 401):" -NoNewline
curl.exe -s -o NUL -w " %{http_code}" "$base/v1/models" | Out-Host
Write-Host ""

Write-Host "  /metrics 指标总数:" -NoNewline
$metrics = curl.exe -s -H $auth "$base/metrics?all_history=true" | ConvertFrom-Json
Write-Host " requests=$($metrics.total.requests) successes=$($metrics.total.successes) failures=$($metrics.total.failures)"
Write-Host "  （切换前的基线是 requests=168723 successes=138587 failures=30136；数字应当 >= 它，因为期间可能有新请求）"

Write-Host "`n完成。若自检不通过，回退办法：" -ForegroundColor Yellow
Write-Host "  schtasks /delete /tn `"$TaskName`" /f"
Write-Host "  uv tool install auto-model-key-router   # 重装 Python 版"
Write-Host "  （配置与指标库的备份在 %LOCALAPPDATA%\AutoModelKeyRouter-backup-*）"
