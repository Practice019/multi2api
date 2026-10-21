# github-ssh-tunnel.ps1 —— 直连也不通时的兜底：用 SSH 22 端口给 HTTPS 建一条隧道。
#
# # 原理
# git 的 SSH 通道（git@github.com:22）与 HTTP(S) 是两条独立链路。代理挂了、但 SSH 还能连的
# 时候（本机 2026-10-21 实测就是这种状态），可以：
#     ssh -N -L 8443:api.github.com:443 git@github.com
# 于是本机 8443 上的每个连接都会被 SSH 转发到 api.github.com:443；
# 再把 gh/curl 的 HTTPS_PROXY 指向 http://127.0.0.1:8443（它们用 CONNECT 方式走代理）即可。
#
# 用法（两个窗口）：
#   窗口 A： .\scripts\github-ssh-tunnel.ps1            # 前台挂着，Ctrl+C 结束
#   窗口 B： $env:HTTPS_PROXY='http://127.0.0.1:8443'; gh release list
#
# ⚠ 隧道只覆盖 api.github.com。要传资产（uploads.github.com）就要再开一条 -L。
param([int]$Port = 8443)

Write-Host "建立 SSH 隧道 127.0.0.1:$Port -> api.github.com:443 ...（Ctrl+C 结束）"
$ssh = (Get-Command ssh -ErrorAction SilentlyContinue)
if (-not $ssh) { Write-Error '本机没有 ssh 客户端'; exit 1 }

# -N 不执行远程命令；-o ExitOnForwardFailure=yes 让"端口被占用"立刻报错而不是静默挂着
& $ssh.Source -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -L "${Port}:api.github.com:443" git@github.com
