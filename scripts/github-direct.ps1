# github-direct.ps1 —— 代理挂了时，用**直连**跑 gh / git API 的封装。
#
# # 为什么需要它（2026-10-21 实测）
# 本机把 HTTP_PROXY/HTTPS_PROXY 指向 127.0.0.1:10808。代理进程一停，
# gh 就报：
#     proxyconnect tcp: dial tcp 127.0.0.1:10808: connectex: No connection could be made
# 而**同一时刻直连 api.github.com 是通的**（实测 200）。也就是说：
#   · 代理不可用 ≠ GitHub 不可达
#   · git 走 SSH（22 端口）本来就不吃 HTTP 代理 ⇒ push 一直可用
# 所以排障顺序应该是：① 先试直连（本脚本）→ ② 再看代理进程 → ③ 最后才考虑隧道。
#
# 用法：
#   .\scripts\github-direct.ps1 release list --limit 3
#   .\scripts\github-direct.ps1 release create v1.9.18 --title "..." --notes-file notes.md
#   .\scripts\github-direct.ps1 api rate_limit
param([Parameter(ValueFromRemainingArguments = $true)] $Rest)

$saved = @{}
foreach ($n in 'HTTP_PROXY','HTTPS_PROXY','ALL_PROXY','http_proxy','https_proxy','all_proxy') {
  $saved[$n] = [Environment]::GetEnvironmentVariable($n, 'Process')
  Set-Item -Path "Env:$n" -Value '' -ErrorAction SilentlyContinue
}
try {
  & gh @Rest
  exit $LASTEXITCODE
} finally {
  # 只影响本次进程；退出时恢复，避免污染调用方 shell
  foreach ($n in $saved.Keys) { if ($saved[$n]) { Set-Item -Path "Env:$n" -Value $saved[$n] -ErrorAction SilentlyContinue } }
}
