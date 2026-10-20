# Plan：渠道转发请求日志的指标采集修复

> 目标 `goal-4ac239fe`。规格来源 = 该目标 objective（已查证的三处缺陷 + 三级兜底口径）。

## 概述

渠道转发路径（`channel:*`）现在写进请求日志的行，**模式列恒为 `sync`、TTFB/tokens/tok-s 全为空**。
根因两条：`mode` 是硬编码字面量；转发是纯字节 tee、没有任何 SSE 解析。

要做的是给转发路径加一层**只读旁路解析**，照 new-api 的三级 fallback 取 usage，
并且**转发字节一个都不许改**——这是本计划唯一的高危面，所有验证都围绕它设计。

## 架构决策

**D1 · 旁路而非改写**
在 `writeChannelStream` 里对已读出的 chunk 做「复制给客户端」+「喂给采集器」两路，
采集器只读、不阻塞、不缓存整轮。理由：改写或缓冲会改变 flush 时机，
直接毁掉流式的体感——这比日志不准严重得多。

**D2 · 只保留最后两帧（照 new-api）**
采集器不累积全部帧，只记 `lastFrame` / `secondLastFrame` 两个字符串。
理由：new-api 源码注释写明「部分兼容网关把完整的累计 usage 附在倒数第二个事件上，
随后发送一个空的最后事件」（`relay/channel/openai/relay-openai.go`）。只留两帧内存恒定。

**D3 · 三级 fallback，绝不记 -1**
① 末帧 `usage` → ② 倒数第二帧 `usage`（须 `PromptTokens>0` 且 `CompletionTokens>0 || TotalTokens>0`）→
③ 按已接收的输出文本**估算**。理由：`-1` 是我们的"未知"哨兵，它一进聚合就要被排除，
等于这条请求的用量永久丢失；估算值虽不精确，但它是**可用的下界**，且能算出 tok/s。

**D4 · 估算不引入新依赖**
new-api 用 `ResponseText2Usage`；我们用「字符数 / 经验系数 + 工具调用补偿」在本仓内实现，
不引第三方 tokenizer。理由：估算本就非精确，引入 tokenizer 只会让二进制和依赖变重。

**D5 · `mode` 语义向 new-api 对齐**
`mode` 记**客户端意图**（请求体 `stream` 字段），不是网关实际转发方式。
理由：用户看日志是要知道"这个请求本来该是流式吗"，而不是"网关内部怎么转的"。

## 任务清单

- [x] **T1** 修 mode：渠道分支按 `peek.Stream` 记 `stream`/`sync` · 规模 S · 依赖 — ｜ 验收：`stream:true` 的请求日志 mode=`stream`，`stream:false` 为 `sync`（两条都实测到） ｜ 验证：`go test ./internal/server/ -run TestChannelLogMode -v`
- [x] **T2** 新建 SSE 用量采集器（独立文件 `internal/server/stream_usage.go`）+ 单测 · 规模 M · 依赖 — ｜ 验收：四种合成 SSE（末帧含 usage / 无 usage / usage 在倒数第二帧 / 空流）分别得到 usage、fallback、估算、空，且均不返回 -1 ｜ 验证：`go test ./internal/server/ -run TestStreamUsage -v`

> **Checkpoint A（T1+T2 后）**：跑 `go build ./...`、`go vet ./...`、`go test ./internal/... ./cmd/... -count=1`、
> 门禁① `node tests/frontend/verify_inline_script_syntax.js` 六段全过。此时主路径**一行未改**，风险为零。

- [x] **T3** 采集器接入 `writeChannelStream`（旁路 tee）+ 字节一致性测试 · 规模 M · 依赖 T2 ｜ 验收：给定任意输入字节流，写出的字节与输入**逐字节相同**（含分片边界、`\n\n` 分割、UTF-8 多字节跨 chunk）；同时采集器拿到 usage 与 ttfb>0 ｜ 验证：`go test ./internal/server/ -run TestChannelStreamByteIdentity -v`

> **Checkpoint B（T3 后）**：这是唯一动主路径的一步。必须确认三件事 ——
> ① 字节一致性测试通过（4 种分片形态）；② 临时实例发一条真实流式请求，客户端收到的内容与直连上游**完全一致**；
> ③ 首帧 flush 时机未变（用逐帧到达时刻证明，不是看总耗时）。

- [x] **T4** 日志行补齐 ttfb_ms / tokens / tok-s（`logChatRowProvider` 的调用点改造） · 规模 S · 依赖 T3 ｜ 验收：日志 JSON 该行的 `ttfb_ms>0`、`tokens>0`、前端 tok/s 列有值 ｜ 验证：临时实例 + 假上游，`Select-String` 读该行字段
- [x] **T5** 端到端验证（假上游回放四种 SSE 形态） · 规模 M · 依赖 T4 ｜ 验收：四种形态下请求日志行的 mode/ttfb/tok/tok-s 四列都不为空，且转发内容与假上游发出的逐字节一致 ｜ 验证：临时实例 + 假上游 + 读日志行
- [ ] **T6** 前端真实浏览器验证（CDP 打开请求日志页） · 规模 S · 依赖 T5 ｜ 验收：`channel:*` 行的「模式」显示 stream、「TTFB」「tok」「tok/s」不再是 "—" ｜ 验证：CDP `Runtime.evaluate` 读该行单元格文本
- [ ] **T7** 发版（打 tag 触发 release，只写本版改动，不写对标字样） · 规模 S · 依赖 T6 ｜ 验收：release 创建成功且 5 平台产物齐全 ｜ 验证：`gh release view`

## 范围边界（不做）

- 不改账号池路径（`workbuddy:*`）的任何采集逻辑 —— 它本来就正常。
- 不把 `-1` 哨兵从历史日志里回填 —— 历史已落盘，回填等于造假数据。
- 不给渠道请求补 `uid` —— 渠道是渠道凭据，没有账号池的 uid 概念；该列保持空是对的。
- 不为估算精度引入 tokenizer。
- 不动 7863 线上实例的数据与配置。

## 风险与对策

| 风险 | 对策 |
|---|---|
| **旁路解析改变转发字节/缓冲行为**（最高危） | T3 前主路径零改动；T3 用"逐字节相同"测试 + 逐帧到达时刻双重证明；出问题可整体 revert 到 Checkpoint A |
| SSE 分片跨 chunk 导致 JSON 解析失败 | 采集器按 `\n\n` 事件边界切分，未闭合的尾部留在缓冲里等下一 chunk（不按 chunk 边界盲目解析） |
| 上游根本不发 usage（部分兼容网关） | 第三级估算兜底，保证不产生 `-1` |
| `stream:true` 但上游实际返回非 SSE（错误体） | 采集器识别非 `data:` 前缀内容 → 判定为错误体，不解析、不产 usage，日志按失败行记 |
| 估算值被误当精确值 | 日志行加标记（估算来源），前端 hover 显示「估算」二字 |

## 验收命令（本仓库原文，不是"跑一下测试"）

```powershell
$env:PATH = 'D:\go-path\go\bin;' + $env:PATH
cd D:\project_GIT\workbuddy2api
go build ./...
go vet ./...
go test ./internal/... ./cmd/... -count=1
node tests/frontend/verify_inline_script_syntax.js      # 必须 === 全部通过（6 段） ===
```
