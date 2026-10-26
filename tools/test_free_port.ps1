<#
.SYNOPSIS
    在**空闲端口 + 隔离工作目录**里起一个 wb2api-server，跑冒烟，验完自动清理。

.DESCRIPTION
    目标：给"改完代码想真跑一下"提供一条 **不会踩到生产实例（7863）** 的通道。

    # 为什么"换工作目录"就够了（不需要改一行代码）

    服务的数据面全部是**相对 cwd 的路径**：

        cmd/server/main.go:102        data/sticky-bindings.json
        cmd/server/main.go:1411       data/accounts.json
        cmd/server/main.go:1415       data/sessions.json
        cmd/server/config.go:1157-58  ./auths（凭证）、./data/state.json（账号池）
        cmd/server/config.go:1194     ./data/checkin-log.json

    唯一的命令行 flag 是 --config（cmd/server/main.go:55），端口在 config 的
    listen 键（cmd/server/config.go:36）。

    ⇒ 复制一份 config、改 listen、把 cwd 指到临时目录，就得到一个**完全独立**的
       实例：自己的 auths/（空）、自己的 data/、自己的账号池快照。
       真实实例的 data/ 与 auths/ 一个字节都不会被读写。

    # 为什么必须在脚本里**显式**守住 7863

    7863 是本机正在跑的生产实例。这个脚本会 Stop-Process，而"按端口找进程再杀"
    或 taskkill /IM 都能在一瞬间杀掉它（按镜像名通杀尤其危险：bin/ 下躺着一排
    同名历史版本）。所以这里有三道独立的闸：

      · 目标端口 == 7863        ⇒ 立刻退出（不是跳过，是拒绝）
      · 待杀 pid == 生产 pid    ⇒ 拒绝停止
      · 待杀进程的镜像路径 ≠ 本次 build 出来的那个 / 不在临时目录内 ⇒ 拒绝停止

    # ⚠ 两条与直觉不同、但必须按**代码事实**写的验收口径

      ① /healthz 在**空 auths/** 的实例上返回 **503**，不是 200。
         原因：handler.go:621-624 用 Pool.ServableNow() 判活，新目录里 0 个账号
         ⇒ 没有可用上游 ⇒ 503。这是**正确**的语义（探活就该这么严）。
         ⇒ 就绪判据是「TCP+HTTP 能应答」，不是「status == 200」；
            本脚本把 200 与 503 都算起来了，并把实际码打出来。

      ② /ui 未登录返回 **200 + 登录页**，不是 302/401。
         原因：webui.go:174-180 写着「给人用的入口要给一张能填表」，真正的
         安全边界在数据面（/admin/* 无凭据一律 401）。
         ⇒ 断言 200 + 页面标题「登录 · 控制台」，且**不含**控制台外壳标题
            「网关控制台」—— 后者出现才意味着未登录拿到了控制台。

    # 幂等

    目标端口被占用 ⇒ 自动退到下一个空闲候选。所以连续跑两次都绿：
    第二次会自己换端口。跑完核对端口确实释放。

.PARAMETER Port
    首选端口，默认 7868。7863 永远拒绝。

.PARAMETER SkipBuild
    复用 -Binary 指定的可执行文件，不跑 go build（快，但可能不是当前源码）。

.PARAMETER KeepTemp
    跑完保留临时目录。默认成功即删、失败保留。

.PARAMETER AllowSharedRedis
    允许 config 里 upstash.url 非空（默认拒绝：那会让隔离实例与生产共享状态）。

.EXAMPLE
    powershell -NoProfile -ExecutionPolicy Bypass -File tools\test_free_port.ps1

.EXAMPLE
    powershell -NoProfile -ExecutionPolicy Bypass -File tools\test_free_port.ps1 -Port 7870 -KeepTemp
#>
[CmdletBinding()]
param(
    [int]   $Port            = 7868,
    [int[]] $FallbackPorts   = @(7868, 7869, 7870, 7871, 7872),
    [string]$RepoRoot        = '',
    [string]$Binary          = '',
    [switch]$SkipBuild,
    [switch]$KeepTemp,
    [switch]$AllowSharedRedis,
    [int]   $ReadyTimeoutSec = 30
)

$ErrorActionPreference = 'Stop'

# ─────────────────────── 常量：安全红线（不是可调参数） ───────────────────────
$PROTECTED_PORT = 7863
$PROTECTED_PID  = 38716
$PROTECTED_PROC = 'wb2api-server'

$SmokePaths = @('/healthz', '/status', '/ui')

# ─────────────────────────── 输出小工具 ───────────────────────────
$script:Errors = New-Object System.Collections.Generic.List[string]

function Write-Step([string]$m) { Write-Host "  -> $m" }
function Write-OK  ([string]$m) { Write-Host "  [OK] $m" -ForegroundColor Green }
function Write-Warn([string]$m) { Write-Host "  [!!] $m" -ForegroundColor Yellow }
function Write-Bad ([string]$m) {
    Write-Host "  [XX] $m" -ForegroundColor Red
    $script:Errors.Add($m)
}
function Fail-Fast([string]$m) {
    Write-Host ''
    Write-Host "失败：$m" -ForegroundColor Red
    exit 1
}

# ─────────────────────────── 端口 / 进程探测 ───────────────────────────

# Get-Listeners 返回某端口上处于 Listen 状态的连接（空数组 = 没人监听）。
#
# ⚠ 用 Get-NetTCPConnection 而不是"试着连一下"：连得上不代表是我们的实例
#   （可能被反代/防火墙接走），而 Listen 是唯一直接回答"这个端口归谁"的事实。
# ⚠ -ErrorAction SilentlyContinue：没有监听时它**会**报错（找不到对象），那是常态。
function Get-Listeners([int]$p) {
    @(Get-NetTCPConnection -State Listen -LocalPort $p -ErrorAction SilentlyContinue)
}

function Test-PortFree([int]$p) { return ((Get-Listeners $p).Count -eq 0) }

# Get-ProcessInfo 取 pid 的镜像名与路径（pid 不存在返回 $null）。
function Get-ProcessInfo([int]$procId) {
    $p = Get-Process -Id $procId -ErrorAction SilentlyContinue
    if (-not $p) { return $null }
    $path = ''
    try { $path = $p.Path } catch { $path = '' }   # 权限不足时 Path 会抛
    return [pscustomobject]@{ Id = $p.Id; Name = $p.ProcessName; Path = $path }
}

# Get-ProtectedState 采集生产实例此刻状态，用于前后对比。
function Get-ProtectedState {
    $ls = Get-Listeners $PROTECTED_PORT
    $own = 0
    if ($ls.Count -gt 0) { $own = [int]$ls[0].OwningProcess }
    $info = Get-ProcessInfo $PROTECTED_PID
    return [pscustomobject]@{
        PortListening = ($ls.Count -gt 0)
        PortOwnerPid  = $own
        PidAlive      = ($null -ne $info)
        ProcName      = $(if ($info) { $info.Name } else { '' })
        ProcPath      = $(if ($info) { $info.Path } else { '' })
    }
}

# Resolve-TargetPort 从候选里挑第一个空闲端口，并保证它不是 7863。
#
# # 为什么遍历候选而不是"占了就失败"
#
# 上一轮实例可能还没完全释放端口（几秒延迟），那种失败是**假红**：
# 原因与本次改动无关。自动换端口让"连续跑两次"成为常态可用。
function Resolve-TargetPort {
    $candidates = @($Port) + $FallbackPorts | Select-Object -Unique
    foreach ($cand in $candidates) {
        # ⚠ 第一道闸：7863 一律**拒绝**，不是跳过。
        #   跳过会让人误以为"我指定了 7863，脚本验过了它" —— 静默改需求比报错危险。
        if ($cand -eq $PROTECTED_PORT) {
            Fail-Fast ("目标端口 $cand 是**生产实例**的端口（pid $PROTECTED_PID），" +
                       "本脚本绝不触碰。请换端口（默认 7868）。")
        }
        if ($cand -lt 1 -or $cand -gt 65535) {
            Fail-Fast "端口 $cand 不在合法范围 1..65535"
        }
        if (Test-PortFree $cand) { return $cand }
        $ls = Get-Listeners $cand
        $own = 0
        if ($ls.Count -gt 0) { $own = [int]$ls[0].OwningProcess }
        Write-Warn "端口 $cand 已被占用（pid $own），试下一个"
    }
    Fail-Fast ("候选端口全被占用：$($candidates -join ', ')。" +
               "用 -Port 指定一个空闲端口，或先关掉占着它们的实例。")
}

# ─────────────────────────── HTTP 探测 ───────────────────────────

# Invoke-Probe 发一次 GET，返回 { Status; Body; Error }（Status=0 表示连接层没通）。
#
# # 为什么用 HttpWebRequest 而不是 Invoke-WebRequest
#
# PS 5.1 的 Invoke-WebRequest 在 4xx/5xx 时**抛异常**，而本脚本恰恰要断言
# "这里是 401 / 那里是 503"。从异常里抠 status 的代码更绕，且 PS7 行为不同。
# HttpWebRequest 在任何状态码下都返回同一个响应对象 —— 判据只有一处。
#
# ⚠ AllowAutoRedirect=$false：/ui?token= 那条路径会 302，要看见 302 本身。
# ⚠ Proxy=$null：直连 127.0.0.1。本机可能配了系统代理（config 里就有）。
function Invoke-Probe([string]$url, [int]$timeoutSec = 5) {
    $req = [System.Net.HttpWebRequest]::Create($url)
    $req.Method            = 'GET'
    $req.Timeout           = $timeoutSec * 1000
    $req.ReadWriteTimeout  = $timeoutSec * 1000
    $req.AllowAutoRedirect = $false
    $req.Proxy             = $null
    $req.UserAgent         = 'wb2api-freeport-smoke/1.0'
    try {
        $resp = $req.GetResponse()
        $code = [int]$resp.StatusCode
        $body = ''
        try {
            $sr = New-Object System.IO.StreamReader($resp.GetResponseStream())
            $body = $sr.ReadToEnd()
            $sr.Dispose()
        } catch { }
        $resp.Close()
        return [pscustomobject]@{ Status = $code; Body = $body; Error = $null }
    } catch [System.Net.WebException] {
        $we = $_.Exception
        if ($we.Response) {
            $code = [int]$we.Response.StatusCode
            $body = ''
            try {
                $sr = New-Object System.IO.StreamReader($we.Response.GetResponseStream())
                $body = $sr.ReadToEnd()
                $sr.Dispose()
            } catch { }
            $we.Response.Close()
            return [pscustomobject]@{ Status = $code; Body = $body; Error = $null }
        }
        return [pscustomobject]@{ Status = 0; Body = ''; Error = $we.Message }
    }
}

# Wait-Ready 轮询到服务能应答，返回就绪耗时。
#
# # 就绪判据是「能应答」，不是「200」
#
# 见文件头 ①：空 auths/ 的实例 /healthz 返回 503，那恰恰证明服务已经起来了
# （它在正常表达"我没有可用上游"）。把 200 当判据会让本脚本**永远**等到超时，
# 而失败信息会说"服务没起来" —— 指向完全错误的方向。
function Wait-Ready([int]$p, [int]$timeoutSec) {
    $sw  = [System.Diagnostics.Stopwatch]::StartNew()
    $url = "http://127.0.0.1:$p/healthz"
    $lastErr = ''
    while ($sw.Elapsed.TotalSeconds -lt $timeoutSec) {
        $r = Invoke-Probe $url 2
        if ($r.Status -ge 200 -and $r.Status -lt 600) {
            $sw.Stop()
            return [pscustomobject]@{ Ok = $true; ElapsedMs = [int]$sw.Elapsed.TotalMilliseconds; Status = $r.Status; LastError = '' }
        }
        if ($r.Error) { $lastErr = $r.Error }
        Start-Sleep -Milliseconds 250
    }
    $sw.Stop()
    return [pscustomobject]@{ Ok = $false; ElapsedMs = [int]$sw.Elapsed.TotalMilliseconds; Status = 0; LastError = $lastErr }
}

# ─────────────────────────── go 环境 ───────────────────────────
#
# # 为什么脚本里必须自己修这三样（不是"多此一举"）
#
# 本机（DSH 沙箱里的 Windows）默认 env 是**被裁剪过**的，实测：
#
#     PATHEXT  = '.CPL'            ← 只剩这一项！
#     GOPATH   = ''（空）
#     GOCACHE  = 'off'
#
# 三样都会让 go build 直接失败，而报错信息**不指向真因**：
#
#   · PATHEXT 没有 .EXE ⇒ Get-Command go / & go 都报 "not recognized"，
#     看起来像"没装 Go"（实测 go.exe 就在 C:\Program Files\Go\bin）。
#   · GOCACHE=off ⇒ "go: build cache is required"，看起来像权限问题。
#   · GOPATH 空 ⇒ 模块缓存找不到，"module cache not found"。
#
# 人手敲命令时会在 cmd 里显式 set 一套；脚本必须把这件事**自己**做完，
# 否则"脚本在我这能跑、在你那不能跑"——那是基建工具最不该有的性质。
#
# ⚠ 只改**本进程**的环境（$env:xxx），不碰系统/用户级设置。
# ⚠ 只填**缺失或明显坏掉**的项：调用方显式设好的值一律尊重。
# Get-UserHome 解析"用户主目录"。
#
# # 为什么不能直接用 $env:USERPROFILE
#
# 本机（DSH 沙箱）实测 $env:USERPROFILE 是**空串**。而 Join-Path 收到空/null
# 会抛 ParameterBindingValidationException：
#
#     Cannot bind argument to parameter 'Path' because it is null.
#
# 这条报错**完全不指向真因**（看起来像脚本参数写错了），而且发生在 go build
# 之前 —— 表现为"脚本跑到一半静默停住"。基建工具里这类错误最贵：
# 用的人会去查 go、查权限，唯独不会去查 USERPROFILE。
#
# 所以按可靠性顺序回落，最后兜底到 TEMP（GOPATH 只是放模块缓存，
# 放哪都能工作，只是不复用别处的下载）。
function Get-UserHome {
    if ($env:USERPROFILE) { return $env:USERPROFILE }
    if ($env:HOMEDRIVE -and $env:HOMEPATH) { return ($env:HOMEDRIVE + $env:HOMEPATH) }
    try {
        $f = [System.Environment]::GetFolderPath('UserProfile')
        if ($f) { return $f }
    } catch { }
    if ($env:TEMP) { return $env:TEMP }
    return [System.IO.Path]::GetTempPath()
}

function Initialize-GoEnv {
    # ① PATHEXT：缺 .EXE 就补回来（追加，不覆盖已有项）
    if ($env:PATHEXT -notmatch '\.EXE') {
        $env:PATHEXT = @('.COM', '.EXE', '.BAT', '.CMD') + @($env:PATHEXT -split ';' | Where-Object { $_ }) -join ';'
    }

    # ② PATH：找到 go.exe（先看 PATH，再看默认安装位）
    $goCmd = Get-Command go -ErrorAction SilentlyContinue
    if (-not $goCmd -and (Test-Path 'C:\Program Files\Go\bin\go.exe')) {
        $env:PATH = 'C:\Program Files\Go\bin;' + $env:PATH
        $goCmd = Get-Command go -ErrorAction SilentlyContinue
    }
    if (-not $goCmd) {
        Fail-Fast ('找不到 go（PATH 里没有，C:\Program Files\Go\bin 也没有）。' +
                   '装 Go 或把它的 bin 加进 PATH。')
    }

    # ③ GOPATH / GOCACHE / GOTOOLCHAIN / GOPROXY
    if (-not $env:GOPATH) { $env:GOPATH = Join-Path (Get-UserHome) 'go' }

    # ⚠ GOCACHE 有两个"坏值"：空 与 'off'（本机实测就是 off）。
    #   两种都必须当成"没设"。判 -not 只能抓住空串，抓不住 off。
    if ((-not $env:GOCACHE) -or ($env:GOCACHE -eq 'off')) {
        # 优先复用已有的本机缓存目录（能省掉一次全量编译，通常 1~2 分钟）；
        # 没有才落到临时目录。 ⚠ 不硬编码 D:\temp\gocache：别人机器上不一定有。
        $tempRoot = if ($env:TEMP) { $env:TEMP } else { [System.IO.Path]::GetTempPath() }
        $candidates = @('D:\temp\gocache', (Join-Path $tempRoot 'gocache-wb2api'))
        foreach ($c in $candidates) {
            if (Test-Path $c) { $env:GOCACHE = $c; break }
        }
        if (-not $env:GOCACHE) { $env:GOCACHE = Join-Path $tempRoot 'gocache-wb2api' }
    }

    if (-not $env:GOTOOLCHAIN) { $env:GOTOOLCHAIN = 'local' }
    if (-not $env:GOPROXY)     { $env:GOPROXY     = 'https://goproxy.cn,direct' }

    Write-Step ("go env: GOPATH=$($env:GOPATH) GOCACHE=$($env:GOCACHE) " +
                "GOTOOLCHAIN=$($env:GOTOOLCHAIN)")
}

# ─────────────────────────── 配置准备 ───────────────────────────

# New-IsolatedConfig 把 config.json 复制到临时目录，只改 listen。
#
# # 为什么用**正则替换一行**，而不是 ConvertFrom-Json → 改 → ConvertTo-Json
#
# 序列化会重排键、改缩进、统一数字格式 —— 于是"复制出来的配置"与人工维护的
# 配置不再是同一份文件，出问题时无法用 diff 判断"是不是脚本改坏了它"。
# 只换 listen 那一行，其余**逐字节保留**。
#
# # 为什么必须校验几个键仍是相对路径
#
# 隔离的全部依据是"相对 cwd"。若哪天 state_file 被写成绝对路径，本脚本就会
# **读写生产账号池**（而它自己毫不知情，报告里还写着"隔离"）。所以把它变成
# 一条**启动前**的红色断言 —— 这是脚本里最该存在的自检。
function New-IsolatedConfig {
    param([string]$SrcPath, [string]$DstPath, [int]$p)

    $raw = Get-Content -Path $SrcPath -Raw -Encoding UTF8

    # ① listen 必须恰好命中一次（配置结构变了就该被人看见，而不是静默改别处）
    $listenRe = [regex]'"listen"\s*:\s*"[^"]*"'
    $hits = $listenRe.Matches($raw)
    if ($hits.Count -ne 1) {
        Fail-Fast "config.json 里 listen 键命中 $($hits.Count) 次（应为 1）：配置结构可能变了，先人工确认"
    }
    $raw = $listenRe.Replace($raw, '"listen": "127.0.0.1:' + $p + '"', 1)

    # ② 路径类键必须仍是相对路径
    foreach ($key in @('state_file', 'auth_dir', 'checkin_log_path')) {
        $re = [regex]('"' + $key + '"\s*:\s*"([^"]*)"')
        foreach ($h in $re.Matches($raw)) {
            $val = $h.Groups[1].Value
            if ($val -eq '') { continue }                       # 留空 = 用默认（仍相对）
            if ($val -match '^[A-Za-z]:' -or $val.StartsWith('/') -or $val.StartsWith('\\')) {
                Fail-Fast ("配置里 $key 是**绝对路径** $val —— 隔离会失效" +
                           "（该实例会去读写真实数据）。请改回 ./ 开头的相对路径。")
            }
        }
    }

    # ③ upstash.url 非空 ⇒ 隔离实例会与生产**共享 Redis 状态**（粘性绑定/池快照）
    $um = [regex]::Match($raw, '"upstash"\s*:\s*\{[^}]*"url"\s*:\s*"([^"]*)"')
    if ($um.Success -and $um.Groups[1].Value.Trim() -ne '' -and -not $AllowSharedRedis) {
        Fail-Fast ("config.json 的 upstash.url 非空 —— 隔离实例会与生产共享 Redis 状态。" +
                   "清空它，或显式加 -AllowSharedRedis。")
    }

    $utf8 = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($DstPath, $raw, $utf8)
}

# ─────────────────────────── 主流程 ───────────────────────────

Write-Host ''
Write-Host '=== 空闲端口隔离测试（wb2api-server） ===' -ForegroundColor Cyan

# ① 仓库根：脚本在 <repo>/tools/ 下 ⇒ 上跳一级。
#    用脚本自身位置而不是 cwd：cwd 是隔离的手段，不该同时承担"找哪个仓库"的职责。
if (-not $RepoRoot) {
    $RepoRoot = Split-Path -Parent (Split-Path -Parent $PSCommandPath)
}
if (-not (Test-Path (Join-Path $RepoRoot 'go.mod'))) {
    Fail-Fast "仓库根 $RepoRoot 下没有 go.mod —— 路径推断错了？用 -RepoRoot 显式指定"
}
$RepoRoot = (Resolve-Path $RepoRoot).Path
Write-OK "仓库根 $RepoRoot"

# ② 起跑前记录生产实例状态（结束时对比，这是"没被动过"的唯一证据）
$protectedBefore = Get-ProtectedState
Write-OK ("生产实例 7863：监听=$($protectedBefore.PortListening) owner=$($protectedBefore.PortOwnerPid) " +
          "pid $PROTECTED_PID 存活=$($protectedBefore.PidAlive)")
if (-not $protectedBefore.PidAlive) {
    Write-Warn "pid $PROTECTED_PID 当前不在 —— 本机没有那个生产实例；7863 仍被列为禁止端口"
}

# ③ 生产实例运行期间的可访问性基线
if ($protectedBefore.PortListening) {
    $pb = Invoke-Probe "http://127.0.0.1:$PROTECTED_PORT/healthz" 3
    if ($pb.Status -ge 200 -and $pb.Status -lt 600) {
        Write-OK "生产实例 7863 可访问（/healthz -> $($pb.Status)）"
    } else {
        Write-Warn "生产实例 7863 在监听但 /healthz 没应答：$($pb.Error)"
    }
} else {
    Write-Warn '7863 当前没有监听 —— 不会产出「生产实例仍可访问」这条证据'
}

# ④ 选端口
$targetPort = Resolve-TargetPort
if ($targetPort -ne $Port) { Write-Warn "首选端口 $Port 不可用，改用 $targetPort" }
# 双保险：解析完再断言一次（将来改 Resolve 的逻辑，这条仍会拦住）
if ($targetPort -eq $PROTECTED_PORT) {
    Fail-Fast "内部错误：解析出的端口等于生产端口 $PROTECTED_PORT"
}
Write-OK "目标端口 $targetPort（已确认空闲且 != $PROTECTED_PORT）"

# ⑤ 临时工作目录
$stamp   = Get-Date -Format 'yyyyMMdd-HHmmss'
$tempDir = Join-Path $RepoRoot ("artifacts\freeport-{0}-{1}" -f $targetPort, $stamp)
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
Write-OK "临时工作目录 $tempDir"

$proc      = $null
$buildLog  = Join-Path $tempDir 'build.log'
$stdoutLog = Join-Path $tempDir 'stdout.log'
$stderrLog = Join-Path $tempDir 'stderr.log'
$exePath   = Join-Path $tempDir 'wb2api-server.exe'
$succeeded = $false

try {
    # ⑥ 隔离的配置与目录
    $srcCfg = Join-Path $RepoRoot 'config.json'
    if (-not (Test-Path $srcCfg)) {
        Fail-Fast "找不到 $srcCfg —— 请先从 config.example.json 复制并编辑"
    }
    New-IsolatedConfig -SrcPath $srcCfg -DstPath (Join-Path $tempDir 'config.json') -p $targetPort
    Write-OK '已复制配置并只改 listen（其余逐字节保留）'

    # ⚠ 空目录而不是复制真实 auths/：本脚本验的是"服务能不能起来 + 路由是否按契约
    #   响应"，不是"账号能不能用"。带真实凭证会让它变成会花上游额度的测试。
    New-Item -ItemType Directory -Path (Join-Path $tempDir 'data')  -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $tempDir 'auths') -Force | Out-Null
    Write-OK '已建空 data/ 与 auths/（0 个账号 ⇒ /healthz 预期 503，见文件头 ①）'

    # ⑦ 二进制
    if ($SkipBuild) {
        if (-not $Binary) { Fail-Fast '-SkipBuild 必须同时给 -Binary <exe 路径>' }
        $exePath = (Resolve-Path $Binary).Path
        Write-OK "复用已有二进制 $exePath（不保证与当前源码一致）"
    } else {
        Initialize-GoEnv
        Write-Step "go build（首次可能要 1~2 分钟；输出写 $buildLog）"
        $swBuild = [System.Diagnostics.Stopwatch]::StartNew()
        & go build -o $exePath ./cmd/server 2>&1 | Out-File -FilePath $buildLog -Encoding UTF8
        $buildRc = $LASTEXITCODE
        $swBuild.Stop()
        if ($buildRc -ne 0) {
            if (Test-Path $buildLog) { Get-Content $buildLog -Tail 30 | Write-Host }
            Fail-Fast "go build 失败（exit $buildRc），详见 $buildLog"
        }
        $sz = [math]::Round((Get-Item $exePath).Length / 1MB, 1)
        Write-OK ("build 成功，耗时 {0:N1}s，大小 {1}MB" -f $swBuild.Elapsed.TotalSeconds, $sz)
    }

    # ⑧ 启动（cwd = 临时目录 = 隔离的根据）
    #
    # ⚠ 不读 stdout 管道：管道读不走会让子进程写满缓冲后**阻塞**，
    #   表现为"服务起来了但再也不响应"。重定向到文件没有这个问题。
    # ⚠ stdout/stderr 必须分两个文件：Start-Process 对同一文件两次重定向会直接报错。
    Write-Step "启动实例（cwd=$tempDir）"
    $proc = Start-Process -FilePath $exePath `
                          -ArgumentList '--config', 'config.json' `
                          -WorkingDirectory $tempDir `
                          -RedirectStandardOutput $stdoutLog `
                          -RedirectStandardError  $stderrLog `
                          -PassThru -WindowStyle Hidden
    Write-OK "已启动 pid $($proc.Id)"

    # ⑨ 等就绪
    $ready = Wait-Ready $targetPort $ReadyTimeoutSec
    if (-not $ready.Ok) {
        Write-Bad ("实例在 {0}s 内没就绪（最后错误：{1}）" -f $ReadyTimeoutSec, $ready.LastError)
        Write-Host '--- stdout 尾部 ---' -ForegroundColor DarkGray
        if (Test-Path $stdoutLog) { Get-Content $stdoutLog -Tail 30 | Write-Host }
        Write-Host '--- stderr 尾部 ---' -ForegroundColor DarkGray
        if (Test-Path $stderrLog) { Get-Content $stderrLog -Tail 30 | Write-Host }
    } else {
        Write-OK ("就绪，耗时 {0}ms（/healthz -> {1}）" -f $ready.ElapsedMs, $ready.Status)

        # ⑩ 冒烟
        Write-Host ''
        Write-Host '  冒烟结果：' -ForegroundColor Cyan
        $smoke = @()
        foreach ($path in $SmokePaths) {
            $r = Invoke-Probe "http://127.0.0.1:$targetPort$path" 5
            $smoke += [pscustomobject]@{ Path = $path; Status = $r.Status; Body = $r.Body; Error = $r.Error }
        }
        foreach ($s in $smoke) {
            $shown = if ($s.Status -eq 0) { "连接失败：$($s.Error)" } else { "$($s.Status)" }
            Write-Host ("    {0,-10} -> {1}" -f $s.Path, $shown)
        }

        # 断言：按**代码事实**（文件头 ①②）
        $hz = $smoke | Where-Object { $_.Path -eq '/healthz' } | Select-Object -First 1
        if ($hz -and ($hz.Status -eq 200 -or $hz.Status -eq 503)) {
            Write-OK "/healthz 应答正常（$($hz.Status)；空 auths 时 503 是预期，见 handler.go:621）"
        } else {
            $got = if ($hz) { $hz.Status } else { 'no-response' }
            Write-Bad "/healthz 期望 200 或 503，实得 $got"
        }

        $st = $smoke | Where-Object { $_.Path -eq '/status' } | Select-Object -First 1
        if ($st -and $st.Status -eq 401) {
            Write-OK '/status 未认证 401（consoleAdminAuth，handler.go:593）'
        } else {
            $got = if ($st) { $st.Status } else { 'no-response' }
            Write-Bad "/status 未认证时应为 401（数据面 fail-closed），实得 $got"
        }

        $ui = $smoke | Where-Object { $_.Path -eq '/ui' } | Select-Object -First 1
        $uiBody   = if ($ui) { $ui.Body } else { '' }
        $uiStatus = if ($ui) { $ui.Status } else { 0 }
        # ⚠ 判据必须锚 <title>，不能用正文子串：
        #   登录页正文里就有一行「本地网关控制台 · 零外部依赖 · …」（webui-login.html:1250），
        #   拿它当"控制台外壳"的判据会把**正确的登录页**判成失效。
        #   <title>GW</title> 才是两个页面真正互斥的唯一标记（webui.html:6 / webui-login.html:7）。
        $isShell     = ($uiBody -match '<title>\s*网关控制台')
        $isLoginPage = ($uiStatus -eq 200) -and ($uiBody -match '<title>\s*登录')
        if ($isShell) {
            Write-Bad '/ui 未登录却拿到**控制台外壳** —— 这是访问控制失效，不是脚本问题'
        } elseif ($isLoginPage) {
            Write-OK '/ui 未登录 → 登录页（200，<title>登录 · 控制台）'
        } else {
            Write-Bad "/ui 期望未登录登录页，实得 status=$uiStatus"
        }

        if ($script:Errors.Count -eq 0) { $succeeded = $true }
    }
}
catch {
    # 把"意外异常"变成一条**有上下文**的失败记录。
    # ⚠ 没有这个 catch，脚本会以一个裸的 PowerShell 异常结束（例如上面的
    #   Join-Path null），输出里只有引擎的一句话，没有人知道它发生在哪一步、
    #   也没人知道 finally 里那些清理有没有做。异常本身仍要打出来。
    Write-Bad ("未预期的异常：" + $_.Exception.Message)
    Write-Host ("    位置：" + $_.InvocationInfo.PositionMessage) -ForegroundColor DarkGray
}
finally {
    # ⑪ 清理：**必须**在 finally，异常/中断也要收尾
    Write-Host ''
    Write-Step '清理'

    if ($proc) {
        $live = Get-ProcessInfo $proc.Id
        if ($live) {
            # ★ 三道校验全过才允许杀。任何一道不过 ⇒ 只报错不杀。
            #   宁可留一个进程，也不能误杀生产。
            $isProtected = ($live.Id -eq $PROTECTED_PID)
            $pathOfLive = ''
            if ($live.Path) {
                try { $pathOfLive = (Resolve-Path -LiteralPath $live.Path).Path } catch { $pathOfLive = $live.Path }
            }
            $pathOfExe = ''
            if (Test-Path -LiteralPath $exePath) {
                try { $pathOfExe = (Resolve-Path -LiteralPath $exePath).Path } catch { $pathOfExe = $exePath }
            }
            $sameExe = ($pathOfLive -ne '') -and ($pathOfLive -eq $pathOfExe)
            $inTemp  = ($pathOfLive -ne '') -and $pathOfLive.StartsWith($tempDir, [StringComparison]::OrdinalIgnoreCase)

            if ($isProtected) {
                Write-Bad "待杀 pid $($live.Id) == 生产 pid $PROTECTED_PID —— 拒绝停止（这不该发生）"
            } elseif ((-not $sameExe) -or (-not $inTemp)) {
                Write-Bad ("待杀 pid $($live.Id) 的镜像不在本次临时目录内（$pathOfLive）—— " +
                           '拒绝停止，避免误杀别的实例')
            } else {
                # 按 pid 精确停止。**绝不** taskkill /IM 或按镜像名通杀：
                # 本机同名进程不止一个（bin/ 下躺着一排历史版本）。
                Stop-Process -Id $live.Id -Force -ErrorAction SilentlyContinue
                # ⚠ Stop-Process 是**异步**的；不等就可能读到一个"还在监听"的端口，
                #   于是下一步的释放检查出现假红。
                $waited = 0
                while ((Get-Process -Id $live.Id -ErrorAction SilentlyContinue) -and ($waited -lt 10000)) {
                    Start-Sleep -Milliseconds 200
                    $waited += 200
                }
                if (Get-Process -Id $live.Id -ErrorAction SilentlyContinue) {
                    Write-Bad "pid $($live.Id) 在 10s 内没有退出"
                } else {
                    Write-OK "已停止 pid $($live.Id)（按 pid 精确停止，未用镜像名通杀）"
                }
            }
        } else {
            Write-OK "进程已自行退出（pid $($proc.Id)）"
        }
    }

    # ⑫ 端口必须真的释放 —— 否则下次运行会被"假占用"挡掉
    $released = $false
    for ($i = 0; $i -lt 50; $i++) {
        if (Test-PortFree $targetPort) { $released = $true; break }
        Start-Sleep -Milliseconds 200
    }
    if ($released) {
        Write-OK "端口 $targetPort 已释放（再查 Listen 为空）"
    } else {
        $ls = Get-Listeners $targetPort
        $own = 0
        if ($ls.Count -gt 0) { $own = [int]$ls[0].OwningProcess }
        Write-Bad "端口 $targetPort 仍被占用（pid $own）"
    }

    # ⑬ 生产实例前后对比
    $protectedAfter = Get-ProtectedState
    if ($protectedBefore.PidAlive -and $protectedAfter.PidAlive) {
        Write-OK "生产实例 pid $PROTECTED_PID 仍在（$($protectedAfter.ProcName)）"
    } elseif ($protectedBefore.PidAlive -and (-not $protectedAfter.PidAlive)) {
        Write-Bad "★ 生产实例 pid $PROTECTED_PID **消失了** —— 立刻查（脚本没杀它，但结论必须报）"
    } else {
        Write-OK '生产实例本就不在，无需对比'
    }
    if ($protectedBefore.PortListening) {
        if ($protectedAfter.PortListening -and ($protectedAfter.PortOwnerPid -eq $protectedBefore.PortOwnerPid)) {
            Write-OK "7863 监听者未变（pid $($protectedAfter.PortOwnerPid)）"
        } else {
            Write-Bad "★ 7863 的监听者变了（前 $($protectedBefore.PortOwnerPid) → 后 $($protectedAfter.PortOwnerPid)）"
        }
        $pa = Invoke-Probe "http://127.0.0.1:$PROTECTED_PORT/healthz" 3
        if ($pa.Status -ge 200 -and $pa.Status -lt 600) {
            Write-OK "生产实例 7863 仍可访问（/healthz -> $($pa.Status)）"
        } else {
            Write-Bad "生产实例 7863 清理后不可访问：$($pa.Error)"
        }
    }

    # ⑭ 日志尾部（**在删目录之前**打印，否则证据就没了）
    Write-Host ''
    Write-Host '  日志尾部 20 行：' -ForegroundColor Cyan
    if (Test-Path $stdoutLog) {
        Get-Content $stdoutLog -Tail 20 | ForEach-Object { Write-Host "    $_" }
    } else {
        Write-Host '    （没有 stdout 日志）'
    }
    if ((Test-Path $stderrLog) -and ((Get-Item $stderrLog).Length -gt 0)) {
        Write-Host '  stderr 尾部 10 行：' -ForegroundColor Yellow
        Get-Content $stderrLog -Tail 10 | ForEach-Object { Write-Host "    $_" }
    }

    # ⑮ 临时目录：成功即删（失败保留证据）
    if ($KeepTemp) {
        Write-OK "保留临时目录（-KeepTemp）：$tempDir"
    } elseif ($succeeded -and ($script:Errors.Count -eq 0)) {
        try {
            Remove-Item -Path $tempDir -Recurse -Force -ErrorAction Stop
            Write-OK '已删除临时目录'
        } catch {
            Write-Warn "临时目录删不掉（$($_.Exception.Message)），留着手动清理：$tempDir"
        }
    } else {
        Write-Warn "失败，保留临时目录供排障：$tempDir"
    }
}

# ⑯ 汇总
Write-Host ''
if ($script:Errors.Count -eq 0) {
    Write-Host ("=== 通过：端口 {0} 冒烟全绿，已清理，7863 未受影响 ===" -f $targetPort) -ForegroundColor Green
    exit 0
}
Write-Host '=== 失败 ===' -ForegroundColor Red
foreach ($e in $script:Errors) { Write-Host "  · $e" -ForegroundColor Red }
exit 1
