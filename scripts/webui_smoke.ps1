# 无头浏览器冒烟测试：真正把 WebUI 跑起来，捕捉只在浏览器里才会暴露的错误。
#
# 为什么必须有这一层：静态检查与 node 模块加载都发现不了
#   - 严格模式下给 SVGElement.className 赋值抛 TypeError（整页白屏）
#   - CSS 选择器写错导致布局塌陷
#   - 图表因为容器宽度为 0 而画不出来
# 这些只有真浏览器渲染一次才知道。脚本启动预览服务器、抓取控制台错误与页面关键
# 结构、并对每页截图。
#
# 用法：
#   pwsh scripts/webui_smoke.ps1

param(
  [int]$Port = 8801,
  [string]$OutDir = ".dbg/smoke",
  [string]$Chrome = "C:\Program Files\Google\Chrome\Application\chrome.exe"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot

if (-not (Test-Path $Chrome)) {
  $candidates = @(
    "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe",
    "C:\Program Files\Microsoft\Edge\Application\msedge.exe"
  ) | Where-Object { Test-Path $_ }
  if (-not $candidates) { throw "找不到 Chrome / Edge 可执行文件" }
  $Chrome = $candidates[0]
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$python = Join-Path $root ".venv\Scripts\python.exe"
if (-not (Test-Path $python)) { $python = "python" }

Write-Host "启动预览服务器 :$Port ..."
$server = Start-Process -FilePath $python -ArgumentList @(
  (Join-Path $root "scripts\webui_preview.py"), "--port", $Port
) -PassThru -WindowStyle Hidden

try {
  # 等端口可用，最多 15 秒。
  $ready = $false
  for ($i = 0; $i -lt 30; $i++) {
    try {
      Invoke-WebRequest -Uri "http://127.0.0.1:$Port/health" -UseBasicParsing -TimeoutSec 2 | Out-Null
      $ready = $true; break
    } catch { Start-Sleep -Milliseconds 500 }
  }
  if (-not $ready) { throw "预览服务器未能启动" }

  $pages = @("overview", "activity", "providers", "routing", "unified", "integrations", "settings")
  $failures = @()

  foreach ($page in $pages) {
    $url = "http://127.0.0.1:$Port/ui/#/$page"
    $profile = Join-Path $env:TEMP ("amkr-smoke-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
    $shot = Join-Path (Resolve-Path $OutDir).Path "$page.png"

    # --dump-dom 会等虚拟时间推进完再输出，此时模块已加载、fetch 已返回。
    $dom = & $Chrome --headless=new --disable-gpu --no-first-run --no-default-browser-check `
      --user-data-dir=$profile --window-size=1600,1400 --virtual-time-budget=6000 `
      --dump-dom $url 2>&1 | Out-String
    Remove-Item -Recurse -Force $profile -ErrorAction SilentlyContinue

    $domLen = $dom.Length
    # 空壳判定：只有 #root > .app 而没有实际内容，说明模块在求值时抛错了。
    $rendered = ($dom -match 'class="shell"') -or ($dom -match 'login-shell')
    $hasChart = $dom -match '<svg'
    $hasNav = $dom -match 'class="nav"'

    # Chrome 把未捕获异常写进 DOM 顶部的 <pre> 或 stderr，两种都检查。
    $consoleError = ($dom -match 'Uncaught') -or ($dom -match 'TypeError') -or ($dom -match 'SyntaxError')

    $ok = $rendered -and $hasNav -and -not $consoleError
    $flag = if ($ok) { "OK  " } else { "FAIL" }
    $extra = if ($hasChart) { "chart" } else { "no-chart" }
    Write-Host ("{0} {1,-14} dom={2,7} {3}" -f $flag, $page, $domLen, $extra)

    if (-not $ok) {
      $failures += $page
      $snippet = $dom.Substring(0, [Math]::Min(600, $domLen))
      Write-Host "     !! $snippet"
    }

    & $Chrome --headless=new --disable-gpu --no-first-run --no-default-browser-check `
      --hide-scrollbars --force-device-scale-factor=1 --user-data-dir=$profile `
      --window-size=1600,1400 --virtual-time-budget=6000 `
      --screenshot=$shot $url 2>&1 | Out-Null
  }

  if ($failures.Count) {
    Write-Host "`n失败的页面: $($failures -join ', ')" -ForegroundColor Red
    exit 1
  }
  Write-Host "`n全部 $($pages.Count) 个页面渲染正常，截图在 $OutDir"
} finally {
  if ($server -and -not $server.HasExited) { Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue }
}
