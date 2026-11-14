# v2.0.6 — 2026-11-14

> 格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

补丁版，**相对 v2.0.5 的增量**（1 个提交 / 20 文件 / +742 −30）。
主题：**把流式上游连接的生命周期收干净** —— 客户端断开后不再泄漏连接，
上游不发换行时缓冲区不再无限增长。

## 修复

### 上游连接的生命周期（本轮实质修复）

**`internal/server/channels_forward.go`**

- 重试循环入口、`p.Chat` **之前与之后**都补 `ctx.Err()` 检查。客户端断开后不再继续换渠道空转。
- `p.Chat` 返回但 ctx 已死时，**关闭刚拿到的 body** —— 此前这条路径会泄漏一个上游连接。
- 错误路径同样补上 `cs.Body.Close()`。

**`internal/server/channels_stream.go`**

- `context.AfterFunc(r.Context(), closeBody)`：客户端断开时**主动关掉上游流**，这是流式泄漏的主路径。
  `sync.Once` 保证不重复关。
- `maxChannelPendingBytes = 1 MiB`：上游迟迟不吐 `\n` 时，pending 缓冲**不再无限增长**；
  分片读取按剩余额度裁剪，超限即停。
- 首块判断补 SSE 识别（`:` / `event:` / `id:` / `retry:` 前缀，或含 `data:`），
  避免把非 SSE 的 200 当流式透传。

**`internal/logbuf/`** —— 写入失败的可见化与收尾（`sqlsink.go` +39 / `logbuf.go`）。

### `.gitignore` 拦住浏览器 profile（★ 400 MB 的账号 cookie）

monkeycode 扫码登录会用一个临时 Edge profile 起浏览器。该目录里是
**Cookies / Login Data / 会话存储 / Local Storage**，加上 Edge 组件缓存与 ONNX 模型 ——
实测 **4052 个文件、400 MB+**。

    git add -A --dry-run | grep -c browser-profiles
    → 4040          ← 一次 `git add -A` 就会把这 4040 个文件写进 git 历史

账号 cookie 进了 git 就永久留在历史里（本仓公开在 GitHub），所以显式忽略
`**/.browser-profiles/`，并放在 `.gitignore` **最后**（后匹配者胜，前面那些 `!` 白名单盖不住它）。

> ⚠ 判断这件事的正确方法**不是** `git check-ignore`：它对那个目录回了"已忽略"
> （指向的却是一个空行号），而 `git add -A --dry-run` 才是实情。
> **要问"到底会加什么"，就问 `add --dry-run`。**

## 新增

- `internal/server/channels_bounds_test.go` —— 上面那些边界（pending 上限、取消响应）的判据。
- `internal/logbuf/lifecycle_test.go` —— logbuf 生命周期判据。
- `tests/frontend/verify_first_batch_behavior.js` —— 前端首批行为校验脚本。

## 变更

四个 `*_live_test.go`（codearts ×1、monkeycode ×3）与两个 hermetic 契约测试的补强；
都带 `testing.Short()` / `WB2API_LIVE_TEST` 守卫，CI 不会因缺凭据而红。
`ui/app.js` 与 `webui.html` 的前端小改。

## 开发者工具（v2.0.5 补记）

`tools/gofmt_lf_check.ps1` —— 在 Windows 上复现 CI 的 gofmt 门禁。

**为什么需要它**：gofmt 对 **CRLF** 文件判「干净」，而仓库在 Windows 上签出时
`core.autocrlf=true` 会把工作树变成 CRLF。于是同一个文件：

    工作树（CRLF）→ gofmt -l 无输出   ⇒ 本地以为干净
    blob（LF）    → gofmt -l 有输出   ⇒ CI 判它脏

结果是本地 `gofmt -w` 变成**空操作**，发布连挂三轮才发现。
脚本把文件归一到 LF 的副本再跑 gofmt，本地判定即与 CI 一致：

    pwsh -File tools/gofmt_lf_check.ps1    # exit 0 = 与 CI 一致

（这条本来该写在 v2.0.5 的说明里，但那个脚本是在 v2.0.5 说明定稿之后才加的，故补记于此。）

## 验证

    pwsh -File tools/gofmt_lf_check.ps1   → 1199 个 .go 文件全部干净（与 CI 判定一致）
    go build ./...                        → exit 0
    go vet ./...                          → exit 0
    go test ./... -count=1                → ok x 52 包，0 FAIL
    node tools/toast_sync.js --check      → exit 0
    node tests/frontend/verify_theme_compliance.js → 7/7 PASS