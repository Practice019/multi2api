<#
.SYNOPSIS
    在 Windows 上复现 CI（Linux）的 gofmt 门禁。

.DESCRIPTION
    # 为什么需要这个脚本

    CI 的 `Check formatting` 跑在 Linux（LF）上：

        unformatted=$(gofmt -l . | grep -v '^$' || true)

    而本仓在 Windows 上签出时 `core.autocrlf=true` 会把工作树变成 **CRLF**。
    关键在于：**gofmt 对 CRLF 文件会判「干净」**（它容忍 CRLF 而不报告），
    于是同一个文件：

        工作树（CRLF）→ gofmt -l 无输出   ⇒ 本地以为干净
        blob（LF）    → gofmt -l 有输出   ⇒ CI 判它脏

    这就是 2026-11-14 那次发布连挂三轮的真实原因：本地怎么跑都是绿的。
    `.github/workflows/ci.yml` 里那句注释（「Windows 上 CRLF 会触发 gofmt 误报」）
    方向也说反了 —— 实际是 **CRLF 掩盖了真问题**，不是误报。

    # 本脚本做的事

    把 `git ls-files '*.go'` 的每个文件**归一到 LF** 后拷进临时目录，再对那份副本
    跑一次 `gofmt -l`。副本里没有 CRLF，于是本地判定与 CI 一致。

.EXAMPLE
    pwsh -File tools/gofmt_lf_check.ps1
    # 退出码 0 = 与 CI 一致（干净）；1 = 有文件需要 gofmt -w

.NOTES
    只读操作：不改工作树里的任何文件，扫完即删临时目录。
#>
[CmdletBinding()]
param(
    # 仓库根目录，默认为本脚本所在目录的上一级。
    [string]$RepoRoot = (Split-Path -Parent $PSScriptRoot)
)

$ErrorActionPreference = 'Stop'
$enc = New-Object System.Text.UTF8Encoding $false
$scan = Join-Path ([System.IO.Path]::GetTempPath()) ('gofmt-lfscan-' + [guid]::NewGuid().ToString('N'))

if (-not (Test-Path (Join-Path $RepoRoot '.git'))) {
    Write-Error "不是 git 仓库：$RepoRoot"
    exit 2
}

try {
    New-Item -ItemType Directory -Path $scan -Force | Out-Null

    Push-Location $RepoRoot
    $files = @(git ls-files '*.go' | Where-Object { $_ -notmatch '^vendor/' })
    Pop-Location

    if ($files.Count -eq 0) {
        Write-Error "没找到任何已跟踪的 .go 文件（cwd = $RepoRoot）"
        exit 2
    }

    foreach ($f in $files) {
        $src = Join-Path $RepoRoot ($f -replace '/', '\')
        if (-not (Test-Path $src)) { continue }
        $dst = Join-Path $scan ($f -replace '/', '\')
        $d = Split-Path $dst -Parent
        if (-not (Test-Path $d)) { New-Item -ItemType Directory -Path $d -Force | Out-Null }
        # 归一到 LF —— 这一步就是本脚本存在的全部理由
        $raw = [System.IO.File]::ReadAllText($src).Replace("`r`n", "`n").Replace("`r", "`n")
        [System.IO.File]::WriteAllText($dst, $raw, $enc)
    }

    $out = gofmt -l $scan 2>&1 | Where-Object { $_ -and $_.Trim() -ne '' }
    $bad = @($out | ForEach-Object { $_ -replace ([regex]::Escape($scan + '\')), '' })

    if ($bad.Count -gt 0) {
        Write-Host "以下 $($bad.Count) 个文件与 CI 判定不一致（需要 gofmt -w）：" -ForegroundColor Red
        $bad | ForEach-Object { Write-Host "  $_" -ForegroundColor Red }
        Write-Host ''
        Write-Host '修法（注意：必须先转 LF，否则本地 gofmt 会判「干净」而不改）：'
        Write-Host '  1) 读文件 → .Replace("`r`n","`n") → 写回'
        Write-Host '  2) gofmt -w <文件>'
        exit 1
    }

    Write-Host "gofmt 与 CI 一致：$($files.Count) 个 .go 文件全部干净。" -ForegroundColor Green
    exit 0
}
finally {
    if (Test-Path $scan) { Remove-Item $scan -Recurse -Force -ErrorAction SilentlyContinue }
}
