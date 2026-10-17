# R2 探针编排：同一条命令内 起 7864 → 跑 Playwright → 立刻停。自包含，不留孤儿。
$ErrorActionPreference = 'Continue'
$root = 'D:\project_GIT\workbuddy2api'
Set-Location $root
$env:NODE_PATH = 'D:\project_GIT\test\.pwtools\node_modules'

# 前置：确认 7864 没被别人占（占了就退出，绝不动它）
$pre = Get-NetTCPConnection -LocalPort 7864 -State Listen -ErrorAction SilentlyContinue
if ($pre) { "⚠ 7864 已被占用 pid=$($pre.OwningProcess) —— 本次不启动，直接退出"; exit 9 }

$p = Start-Process -FilePath "$root\bin\wb2api-server.exe" -ArgumentList @('-config','config-7864.json') -WorkingDirectory $root -RedirectStandardOutput "$root\.r2srv.out.txt" -RedirectStandardError "$root\.r2srv.err.txt" -PassThru -WindowStyle Hidden
"started pid=$($p.Id)"

$up = $false
for ($i = 0; $i -lt 40; $i++) {
  Start-Sleep -Milliseconds 500
  try { Invoke-WebRequest -Uri 'http://127.0.0.1:7864/ui' -UseBasicParsing -TimeoutSec 3 | Out-Null; $up = $true; break } catch { }
}
"up=$up after $($i*0.5)s"
if (-not $up) { Get-Content "$root\.r2srv.err.txt" -Tail 10; Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue; exit 8 }

try {
  node "$root\.r2probe.js" 2>&1 | Select-Object -Last 320
} finally {
  # 收尾：停掉本次起的进程，确认端口空出
  Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 800
  $left = Get-NetTCPConnection -LocalPort 7864 -State Listen -ErrorAction SilentlyContinue
  if ($left) { "⚠ 7864 仍在监听 pid=$($left.OwningProcess)，二次清理"; Stop-Process -Id $left.OwningProcess -Force -ErrorAction SilentlyContinue; Start-Sleep -Milliseconds 500 }
  $after = Get-NetTCPConnection -LocalPort 7864 -State Listen -ErrorAction SilentlyContinue
  "7864 cleaned = $($null -eq $after)"
  "7863 未触碰 = $((Get-NetTCPConnection -LocalPort 7863 -State Listen -ErrorAction SilentlyContinue) -ne $null)"
}
