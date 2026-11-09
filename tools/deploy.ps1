<#
.SYNOPSIS
  安全部署 wb2api-server：先构建成功、再动服务；任何一步失败都回滚。

.DESCRIPTION
  # 为什么要有这个脚本（本会话真实事故）
  某次 `go build` 失败、新 exe 根本没生成，但部署脚本仍无条件
  `Stop-Process` + `Move-Item` ⇒ **服务停了、exe 还没了**，只能手工救。
  ⇒ 本脚本把顺序钉死成"**构建成功** → 记录现状 → 备份 → 停 → 换 → 起 → 等监听"，
     并且每一步失败都走同一段回滚逻辑。

  硬门禁（任何一条不满足就 exit 1，且**绝不碰服务**）：
    1. 构建 exit≠0 或产物不存在            ⇒ 立刻退出（服务原样运行）
    2. -Bogus（故障注入）                  ⇒ 用于验证上面这条：故意让构建失败

  回滚触发点：停服务之后任何一步失败（替换失败 / 启动失败 / 20s 内没等到监听）。

.PARAMETER Exe
  正式 exe 路径（默认 bin\wb2api-server.exe，相对仓库根）。

.PARAMETER DryRun
  只走到"构建成功 + 记录现状"，不停服务、不替换、不重启。
  与 -Bogus 一起用可验证"构建失败时服务不受影响"。

.PARAMETER Bogus
  故障注入：故意构建一个不存在的包 ⇒ 构建必失败。仅用于验证门禁。

.PARAMETER Port
  健康检查端口（默认 7863）。

.EXAMPLE
  pwsh -NoProfile -File tools/deploy.ps1 -DryRun -Bogus    # 验证门禁：必失败，且服务不受影响
  pwsh -NoProfile -File tools/deploy.ps1                   # 真正部署
#>
param(
    [string]$Exe = 'bin\wb2api-server.exe',
    [switch]$DryRun,
    [switch]$Bogus,
    [int]$Port = 7863,
    # 重启时的工作目录。默认仓库根 —— 实测本机应用从仓库根启动（auths/ 等相对路径依赖它）。
    # 为什么不能只看原进程的 ExecutablePath：那只给出 exe 所在目录（bin/），不是真实 CWD。
    [string]$WorkDir = '',
    # -SkipTests 跳过部署前的**离线测试门禁**（默认跑）。
    #
    # # 为什么默认跑测试
    #
    # 2026-11-09 实测：那天 20+ 次部署里，至少有一次是**带红测试**部署上去的
    # （monkeycode 的 Jobs 测试在改了 Jobs() 之后变红，而当时的门禁只看构建 ⇒ 红灯被忽略）。
    # 后果不严重（那次只改测试），但它证明「构建通过」≠「能部署」。
    #
    # # 为什么必须留 -SkipTests 这个出口
    #
    # 门禁实测 **43.4 秒**（`go test ./... -short`，2026-11-09）。虽然比预想的快，
    # 但部署是高频操作 ⇒ 保留显式降级开关。**不给默认值**：必须手打，不许它变成习惯。
    #
    # # 为什么用 -short（而不是 -run 过滤）
    #
    # 本仓有 5 条**真打上游**的 Live 测试（TestLiveCheckinStatus / TestLiveCheckin /
    # TestLiveBothRegions / TestLiveChat / TestLiveCaptcha），它们**都有 `testing.Short()`
    # 守卫**（2026-11-09 实测确认）⇒ `-short` 正好全部跳过 ✓
    # 它们会因上游抖动而红 —— 那是环境问题不是代码回归 ⇒ 拿它们当门禁会导致
    # "每次都被拦下" ✗ ⇒ 门禁只跑**离线测试**（实测在 -short 下 ok=50 / FAIL=0）。
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path -Parent $PSScriptRoot          # tools/ 的上一级 = 仓库根
$exePath = if ([System.IO.Path]::IsPathRooted($Exe)) { $Exe } else { Join-Path $root $Exe }
$stamp = Get-Date -Format 'MMdd-HHmm'
$tmpExe = Join-Path (Split-Path -Parent $exePath) ("wb2api-server.build-$stamp.exe")
$backupExe = Join-Path (Split-Path -Parent $exePath) ("wb2api-server.replaced-$stamp.exe")

# 回滚状态（catch 里用）
$origPid = $null; $origCmd = $null; $origDir = $null; $origHash = $null
$newHash = $null
$serviceStopped = $false
$sw = [System.Diagnostics.Stopwatch]::StartNew()

function Get-OriginalProcess {
    $c = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $c) { return $null }
    $p = Get-CimInstance Win32_Process -Filter ("ProcessId=" + $c.OwningProcess) -ErrorAction SilentlyContinue
    return $p
}

function Hash-Of([string]$path) {
    if (-not (Test-Path $path)) { return '(不存在)' }
    return (Get-FileHash $path -Algorithm SHA256).Hash.Substring(0, 12)
}

# Start-GatewayLogged 启动网关并把 stdout/stderr **追加**到 logs\server.log。
#
# # 为什么不能直接用 Start-Process -RedirectStandardOutput（本轮的 P0）
#
# 实测：2026-11-09 部署 15+ 次，而 logs\server.log 停在 **09:22:35**、5 小时 37 分没写过 ✗
# 三个坑，每个都会导致日志丢失或损坏：
#   ① `-RedirectStandardOutput` 与 `-RedirectStandardError` **不能用同一个文件**
#      （PowerShell 直接报错：Cannot use the same file for both）
#   ② 它们是**覆盖**语义（不是追加）⇒ 每次部署都会清空历史日志 ✗
#   ③ 重定向后父进程（本脚本）退出时句柄归属会变，行为不如交给 cmd 稳定
#
# # 为什么用 cmd /c 包一层
#
# `cmd /c "... >> file 2>&1"` 与 start.bat 的行为**逐字一致**（追加 + 合并 stderr），
# 那是本仓既有的、被验证过的启动方式 ✓
# ⇒ 部署启动与手工启动用**同一个**重定向语义，避免"手工起有日志、部署起没日志"。
#
# ⚠ 本函数必须定义在**使用点之前**（L81 的 Rollback 里就要用）——
#   PowerShell 按脚本顺序定义函数，定义在后面的函数在其被调用前是不存在的。
function Start-GatewayLogged {
    param(
        [Parameter(Mandatory = $true)][string]$ExePath,
        [Parameter(Mandatory = $true)][string]$WorkingDir,
        [string]$Args = ''
    )
    $logDir = Join-Path $WorkingDir 'logs'
    if (-not (Test-Path $logDir)) { New-Item -ItemType Directory -Path $logDir -Force | Out-Null }
    $logFile = Join-Path $logDir 'server.log'

    # cmd 的 /c 只接受**一个**参数串 ⇒ 用字符串拼，但每个路径都自己加引号。
    # 形状：cmd.exe /c ""<exe>" <args> >> "<log>" 2>&1"
    $inner = '""' + $ExePath + '"'
    if ($Args) { $inner += ' ' + $Args }
    $inner += ' >> "' + $logFile + '" 2>&1"'

    Start-Process -FilePath 'cmd.exe' -ArgumentList '/c', $inner -WorkingDirectory $WorkingDir -WindowStyle Hidden | Out-Null
}

function Rollback {
    param([string]$reason)
    Write-Host "[rollback] 触发原因：$reason" -ForegroundColor Yellow
    try {
        if ($serviceStopped) {
            if (-not (Test-Path $backupExe)) { Write-Host "[rollback] 备份不存在，无法回滚 exe" -ForegroundColor Red; return }
            Copy-Item $backupExe $exePath -Force
            Write-Host "[rollback] 已把备份拷回：$backupExe → $exePath" -ForegroundColor Yellow
            if ($origCmd) {
                Start-GatewayLogged -ExePath $exePath -WorkingDir $origDir -Args (Rebuild-Args $origCmd)
                Write-Host "[rollback] 已用原命令行重启" -ForegroundColor Yellow
            }
        } else {
            Write-Host "[rollback] 服务未被停过 ⇒ 无需回滚" -ForegroundColor Green
        }
    } catch {
        Write-Host ("[rollback] 回滚本身失败：" + $_.Exception.Message) -ForegroundColor Red
    }
}

# 从原命令行里剥掉 exe 自己那一段，得到参数串
function Rebuild-Args([string]$cmdline) {
    if (-not $cmdline) { return '' }
    $t = $cmdline.Trim()
    if ($t.StartsWith('"')) {
        $end = $t.IndexOf('"', 1)
        if ($end -gt 0) { return $t.Substring($end + 1).Trim() }
    }
    $sp = $t.IndexOf(' ')
    if ($sp -lt 0) { return '' }
    return $t.Substring($sp + 1).Trim()
}

try {
    Write-Host "=== 部署 wb2api-server ===" -ForegroundColor Cyan
    Write-Host ("  仓库根 : {0}" -f $root)
    Write-Host ("  正式 exe: {0}" -f $exePath)

    # ── 记录现状（构建之前就记，便于任何分支都能打印"服务没被碰过"）──
    $proc = Get-OriginalProcess
    if ($proc) {
        $origPid = $proc.ProcessId
        $origCmd = $proc.CommandLine
        # ⚠ 工作目录不能只看 ExecutablePath（那给出的是 exe 所在目录，通常是 bin/），
        # 真实的启动 CWD 才是应用解析相对路径（如 auths/）的依据。
        # 若原命令行里写的是**相对** exe（实测本机就是 "bin\wb2api-server.exe"），
        # 那它只可能从仓库根启动 ⇒ CWD = 仓库根。
        $cmdExe = ''
        $t = $proc.CommandLine.Trim()
        if ($t.StartsWith('"')) { $e2 = $t.IndexOf('"', 1); if ($e2 -gt 0) { $cmdExe = $t.Substring(1, $e2 - 1) } }
        else { $sp2 = $t.IndexOf(' '); $cmdExe = if ($sp2 -lt 0) { $t } else { $t.Substring(0, $sp2) } }
        # 两支都落到仓库根：相对命令行只可能从仓库根启动；绝对命令行时 CWD 无从得知，
        # 但 exe 位于仓库内 ⇒ 仓库根是唯一有依据的选择（-WorkDir 可覆盖）。
        $origDir = if ($WorkDir) { $WorkDir } else { $root }
        $origHash = Hash-Of $exePath
        Write-Host ("  当前进程: PID {0}  启动于 {1}" -f $origPid, $proc.CreationDate)
        Write-Host ("  当前 exe : {0}" -f $origHash)
    } else {
        Write-Host ("  当前没有监听 {0} 的进程（冷启动场景）" -f $Port) -ForegroundColor Yellow
    }

    # ── 门禁 1：构建（失败 ⇒ 抛错 ⇒ 绝不碰服务）──
    $pkg = if ($Bogus) { './cmd/__nonexistent_for_gate_test__' } else { './cmd/server' }
    Write-Host ("`n[1/7] 构建 {0} ⇒ {1}" -f $pkg, $tmpExe) -ForegroundColor Cyan
    $env:GOMAXPROCS = '2'
    Push-Location $root
    try {
        & go build -o $tmpExe $pkg 2>&1 | ForEach-Object { Write-Host ("      " + $_) -ForegroundColor DarkGray }
        $buildExit = $LASTEXITCODE
    } finally { Pop-Location }
    if ($buildExit -ne 0 -or -not (Test-Path $tmpExe)) {
        throw ("构建失败（exit=$buildExit，产物存在=$([bool](Test-Path $tmpExe))）—— 门禁拦下，**没有**碰服务")
    }
    $newHash = Hash-Of $tmpExe
    Write-Host ("      构建成功：{0}" -f $newHash) -ForegroundColor Green

    if ($DryRun) {
        Write-Host "`n[DryRun] 到此为止：不备份、不停服务、不替换、不重启。" -ForegroundColor Yellow
        if ($origPid) { Write-Host ("[DryRun] 服务仍在运行：PID {0}（未被触碰）" -f $origPid) -ForegroundColor Green }
        Remove-Item $tmpExe -Force -ErrorAction SilentlyContinue
        Write-Host ("[DryRun] 已清理临时产物；耗时 {0:N0}ms" -f $sw.Elapsed.TotalMilliseconds)
        exit 0
    }

    # ── 门禁②：离线测试 ──
    #
    # ⚠ 为什么并进第 1 步、不另起编号：独立一步要把 [1/7]…[7/7] 全改成 [x/8]
    #   （7 处编号 + 阅读习惯），而它的语义与构建同属"门禁 —— 没过就不许碰服务"，
    #   失败形态也一样（打印 + exit 1）。并进去 = 零编号风险。
    if ($SkipTests) {
        Write-Host "      测试：已跳过（-SkipTests）" -ForegroundColor Yellow
        Write-Host "      ⚠ 你选择了跳过测试门禁。构建通过 ≠ 能部署。" -ForegroundColor Yellow
    } else {
        Write-Host "      离线测试中（go test ./... -short，实测约 45 秒）…" -ForegroundColor Cyan
        $testSw = [System.Diagnostics.Stopwatch]::StartNew()
        $testOut = & go test ./... -short -count=1 2>&1
        $testCode = $LASTEXITCODE
        $testSw.Stop()
        if ($testCode -ne 0) {
            Write-Host ("      测试失败（{0:N1}s）—— 门禁拦下，**没有碰服务**" -f ($testSw.ElapsedMilliseconds / 1000)) -ForegroundColor Red
            $testOut | Select-Object -Last 40 | ForEach-Object { Write-Host "      $_" }
            Write-Host ""
            Write-Host "      这不是构建错误，是**测试失败**。" -ForegroundColor Yellow
            Write-Host "      修好测试再部署；确实要跳过就重跑：" -ForegroundColor Yellow
            Write-Host "          pwsh -NoProfile -File tools\deploy.ps1 -SkipTests" -ForegroundColor Yellow
            Write-Host "      ⚠ 跳过测试 = 你接受「带红部署」—— 别让它变成习惯。" -ForegroundColor Yellow
            exit 1
        }
        Write-Host ("      测试通过（离线测试全绿，{0:N1}s）" -f ($testSw.ElapsedMilliseconds / 1000)) -ForegroundColor Green
    }

    # ── 2) 备份现有 exe ──
    Write-Host "`n[2/7] 备份现有 exe" -ForegroundColor Cyan
    if (Test-Path $exePath) {
        Copy-Item $exePath $backupExe -Force
        Write-Host ("      备份到 {0}" -f $backupExe)
    } else {
        Write-Host "      现有 exe 不存在（首次部署）" -ForegroundColor Yellow
    }

    # ── 3) 停服务（只按 PID，不按名字杀）──
    Write-Host "`n[3/7] 停服务" -ForegroundColor Cyan
    if ($origPid) {
        Stop-Process -Id $origPid -Force
        $serviceStopped = $true
        Start-Sleep -Milliseconds 300
        Write-Host ("      已停 PID {0}" -f $origPid)
    }

    # ── 4) 替换 ──
    Write-Host "`n[4/7] 替换 exe" -ForegroundColor Cyan
    Move-Item $tmpExe $exePath -Force
    Write-Host ("      {0} ← {1}" -f $exePath, $newHash)

    # ── 5) 用原命令行重启 ──
    Write-Host "`n[5/7] 重启" -ForegroundColor Cyan
    $startArgs = Rebuild-Args $origCmd
    if ($origDir) { Start-GatewayLogged -ExePath $exePath -WorkingDir $origDir -Args $startArgs }
    else { Start-GatewayLogged -ExePath $exePath -WorkingDir (Split-Path -Parent $exePath) -Args $startArgs }
    Write-Host ("      已启动（参数：{0}）" -f $(if ($startArgs) { $startArgs } else { '(无)' }))

    # ── 6) 等监听（最多 20s）──
    Write-Host "`n[6/7] 等端口 $Port 监听" -ForegroundColor Cyan
    $deadline = (Get-Date).AddSeconds(20)
    $listening = $false
    while ((Get-Date) -lt $deadline) {
        $c = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($c) { $listening = $true; $newPid = $c.OwningProcess; break }
        Start-Sleep -Milliseconds 400
    }
    if (-not $listening) { throw ("20 秒内没等到 {0} 监听 ⇒ 启动失败" -f $Port) }

    # ── 7) 打印 ──
    Write-Host "`n[7/7] 完成" -ForegroundColor Green
    # 让人一眼看到"这次部署有没有过测试门禁"（跳过时黄字提醒 —— 别让它变成习惯）。
    Write-Host ("  测试 : {0}" -f $(if ($SkipTests) { '已跳过（-SkipTests）' } else { '✓ 通过（离线测试）' })) -ForegroundColor $(if ($SkipTests) { 'Yellow' } else { 'Green' })
    Write-Host ("  PID  : {0} → {1}" -f $(if ($origPid) { $origPid } else { '(冷启动)' }), $newPid)
    Write-Host ("  hash : {0} → {1}" -f $origHash, (Hash-Of $exePath))
    Write-Host ("  监听 : {0}:{1} ✓" -f $Port, $listening)
    Write-Host ("  耗时 : {0:N0}ms" -f $sw.Elapsed.TotalMilliseconds)
    exit 0
} catch {
    Write-Host ("`n[失败] " + $_.Exception.Message) -ForegroundColor Red
    Rollback $_.Exception.Message
    Write-Host ("  耗时 : {0:N0}ms" -f $sw.Elapsed.TotalMilliseconds) -ForegroundColor Red
    exit 1
} finally {
    if (Test-Path $tmpExe) { Remove-Item $tmpExe -Force -ErrorAction SilentlyContinue }
}
