# 建议报告：请求日志缺少「上游错误原文」—— 排障能力缺口

> **编者注（2026-11-10 补）**：**本报告的建议已实施**：`logbuf.Entry` 新增 `upstream_err`（提交 `e74653e`）与 `upstream_req`（提交 `cb0e018`），失败请求体完整落盘见 `bcfd790`（`data/failed-req/`）。⚠ 仍有一处漏点：工具循环路径未记 `upstream_req`（见 `loomy-502-misreported-as-503-2026-11-10.md` §4.3）。


- **报告日期**：2026-11-10
- **触发**：用户问「我们这个网关，它那个请求不会保存记录的，对吗？正常来说，这种报错，你直接去查那个日志，都是可以查到的。是我们的日志系统没有做好的，你看看是不是？」
- **结论**：**用户的判断是对的** —— 不是"完全没记"，而是**记了判决摘要、丢了上游原文**。
- **状态**：仅调研 + 建议，**未改代码**

---

## 0. 一句话结论

请求日志（`data/request-log.jsonl`）**记了"我判了什么"（`decision`），但没记"上游到底说了什么"**。
所以像 codar 那次 `last=codar server 403` 的失败，事后**无法知道 403 的响应体内容**
（是风控？是额度？是账号受限？）—— 而这恰恰是排障时最需要的一段证据。

**更值得注意的是**：代码注释里明确承诺了「日志里保持原文（logbuf 记的是排障用的证据）」，
但 `logbuf` **根本没有这个字段**。属于**设计意图与实现脱节**，不是取舍。

---

## 1. 现场举证：同一次失败，日志里有什么、缺什么

以用户报的 codar 失败为例（`data/request-log.jsonl`，seq 324354 / 324355）：

```json
{
  "seq": 324354, "status": 502, "model": "codar/deepseek-v4.1-flash",
  "provider": "codar", "uid": "user_15083583411_7985",
  "ttfb_ms": 0, "tokens": -1, "total_ms": 678, "pre_ms": 594,
  "decision": "switched_upstream:codar attempts=5 spentMs=675 last=codar server 403",
  "client_ua": "deepseek-harness/0.2.1-rc.3"
}
```

**有的**：状态码、判决摘要（`decision`）、尝试次数、耗时、账号、UA —— 结构清晰，这部分做得**很好**。

**缺的**（本次排障真正需要的三样）：

| 缺什么 | 后果（本次真实发生） |
|---|---|
| 上游响应体原文 | 只知 `403`，不知正文是 `API access is restricted` 还是 `insufficient quota` 还是边缘 IP 封控 |
| 上游响应头（关键几条） | 分不清是 CDN/WAF 拦的（`server: nginx`、`cf-ray`）还是应用层拒的 |
| 每次尝试的错误序列 | `attempts=5` 但不知道 5 次分别是什么错（全 403？还是 403/429/超时混合？）|

排障结果是：我**只能靠直连上游复现**才推断出方向，而直连又复现不出来（风控窗口已过），最终
**无法给出确定结论**。用户的抱怨正是这个体验。

---

## 2. 代码层核实：字段定义里确实没有"错误原文"

`internal/logbuf/logbuf.go` 的行结构（字段清单）：

```go
Seq / At / Model / Mode / Status / UID / Provider
TTFBMS / Tokens / TotalMS / Credit
ThinkTokens / CacheHitTokens / CacheMissTokens
Decision     string  // 判决留痕，形如 "reason:source"
PreMs / ClientIP / ClientUA
Channel / UpstreamModel
```

→ **没有任何字段承载上游错误原文**。而 `Decision` 的注释自己写的是"**判决留痕**"，
语义上就是"我做了什么决定"，不是"上游说了什么"。

### 2.1 更要紧的：注释承诺了，实现没做

`internal/server/handler.go`（出口拼错误消息那段）原文：

```go
// ⚠ 日志里保持原文（logbuf 记的是排障用的证据），只有出站这份要脱敏。
msg += ": " + gateway.MaskError(lastErr)
```

这段注释的**意思是**：
- **出站**（给客户端）→ 脱敏（`MaskError`），防止把 host/IP/base_url 泄露给调用方 ✅ 已实现
- **日志侧** → 应当保留原文作为证据 ❌ **未实现**（logbuf 里没有这个字段）

也就是说：**脱敏那半做了，保留原文那半没做**。这解释了一个反直觉的现象 ——
**客户端看到的错误比运维能查到的更详细**（客户端拿到了脱敏后的上游原文，
而日志里连脱敏后的都没有）。

### 2.2 渠道路径（Key 型渠道）比账号池路径更薄

`channels_forward.go` 的失败分支会 `log.Printf` 一行（容器日志），但不写 `request-log.jsonl`
的结构化记录 —— 于是"渠道"类失败在**管理台的请求日志页面里查不到**，只能翻 `docker logs`。
（反例：`logChatRowProvider` 在成功路径上是记了 `client_ip` / `client_ua` 的，
说明这条链路本身没问题，只是**失败路径没接**。）

---

## 3. 影响面：这不是一次性的，是系统性的

同一类"排障时缺证据"在三处已经实际发生过（都由本报告作者在真实报障中遇到）：

| # | 事件 | 当时缺什么 | 后果 |
|---|---|---|---|
| 1 | loomy 上游 502 雪崩 | 每次尝试的错误序列 | 只能靠"账号 0 冷却但请求失败"反推 |
| 2 | loomy `200 + 空流`（`Stream ended without finish_reason`） | 上游响应体（非 SSE 的 JSON 信封） | 靠直连上游才确认是 `登录已失效` |
| 3 | **codar 403（本次）** | 403 响应体原文 | **至今无法定性**：风控？IP？账号？ |

共同点：**都不是"没有日志"，而是"日志记了现象、没记原因"**。

---

## 4. 建议（按性价比排序，均未实施）

### 建议 1（强烈建议）：给日志加「上游错误原文」字段

在 `internal/logbuf` 的行结构里增加**一个**字段，例如：

```go
// UpstreamErr 上游错误原文（**脱敏后**），仅失败请求出现。
//
// 与 Decision 的分工：
//   Decision    —— 我做了什么（换号/熔断/降级）
//   UpstreamErr —— 上游说了什么（原文，含错误码与关键词）
//
// 为什么必须脱敏后再存：管理台是网页，这份会渲染到前端；
// 而 host/IP/base_url 属于"后端是谁"的信息（此前已因泄露被修过一次）。
// 用既有的 gateway.MaskError 即可，与出站同一口径。
UpstreamErr string `json:"upstream_err,omitempty"`
```

**写入点**（只需 2~3 处，与 `decision` 同一位置）：
- `handler.go` 出口（账号池路径）：`lastErr` 已经在那里，直接 `MaskError(lastErr)`
- `channels_forward.go` 失败分支：`tryChannelOnce` 返回的 `ferr`
- 流内错误路径（`inband` 分支）：已有 `inband.Body` 原文

**成本**：约 20 行 + 4 个用例；logbuf 是 append-only 的 JSONL，**加字段不破坏既有解析**（`omitempty`，
老记录读出来是空串，前端按 `—` 显示即可）。**无破坏性变更**。

### 建议 2（强烈建议）：每次尝试的错误序列

`decision` 里已有 `attempts=5`，但只留了 `last=`。建议改成**保留全部尝试**的摘要：

```
attempts=5 spentMs=675 last=codar server 403
  →  try1=403(node1) try2=403(node2) try3=403 ... try5=403
```

至少记录**每次的上游状态码 + 账号尾号**（不含敏感信息）。这样"全 403"与"403/429 混合"
一眼可分 —— 前者是上游整体拒绝，后者是单号问题。

**成本**：把 `decision` 从字符串改成持久化的小数组字段（或用分隔符串），**约 15 行**。
注意 logbuf 的既有字段纪律：**只追加、不改语义**，所以新字段叫 `attempts_detail` 更安全。

### 建议 3（中等）：把渠道失败也写进结构化请求日志

现在 `channels_forward.go` 的失败只 `log.Printf` 到容器 stdout，管理台请求日志页面看不到。
建议：失败也调一次 `logChatRowProvider`（成功路径已经在调，同一个函数）。

**价值**：渠道类问题（Key 型上游）从此不必 `docker logs` 才能查。
**成本**：约 10 行。风险：要确认失败时 `fw.Stream` 可能为 nil（需判空）。

### 建议 4（可选）：管理台请求日志加「错误」列 + 详情弹窗

前端：请求日志表格加一列（`—` / 错误码 / 简短文案），点开看完整 `upstream_err`。
**成本**：前端改动较大（表格 + 弹窗 + 审计），建议先做后端（建议 1/2），字段先落库，
前端随后跟上。

### 建议 5（可选）：脱敏要留"可判别指纹"

`MaskErrorText` 会把 host 抹成 `***`，这在对外是对的；但排障时"被抹掉的是哪个 host"
有时很关键。建议**只在日志侧**额外留一个短指纹（如 `host_hash=ab12cd`，不可逆），
既能对比"是不是同一个后端"，又不泄露具体地址。**成本低，争议也低**（hash 可逆性可忽略）。

---

## 5. 反面意见：为什么此前可能是"故意不记"

诚实列出可能的反对理由，以及为什么它们不足以支撑"不记"：

| 反对理由 | 反驳 |
|---|---|
| "错误原文可能含敏感信息（key/host/IP）" | 既有 `MaskError` 就是干这个的，**出站已用它**；日志侧复用同一函数即可 |
| "日志体积会变大" | 只在**失败**请求上写（`omitempty`），实测失败率远低于 1%；且 request-log 已有 `keep_days` 轮转与 `max_bytes` 上限 |
| "上游原文可能很长（HTML 页面）" | 加**长度上限**（如 2 KiB）+ 截断标记即可；HTML 403 页面截断后仍能看出 `nginx` / `Access denied` |
| "客户端已经能拿到错误了" | 见 §2.1：**客户端拿到的比日志更详细**，而且客户端不会把日志交给运维。反过来才是正确方向 |

---

## 6. 如果要动手，最小可行改动（给实现者）

**只做建议 1** 就能覆盖 80% 的排障需求，且改动面最小：

```text
1. internal/logbuf/logbuf.go     加 UpstreamErr 字段（json:"upstream_err,omitempty"）
2. internal/server/handler.go    出口处：st.upstreamErr = gateway.MaskError(lastErr)
3. internal/logbuf              Row/Record 组装处带上它
4. 前端（可延后）             请求日志详情里显示
5. 测试                        1 条：失败请求的落盘行必须含 upstream_err；
                               1 条：成功后该字段为空（omitempty 生效）
```

**验收标准**（可脚本复检）：

```bash
# 造一次必然失败（例如不存在的模型名 → 上游 404/400）后：
python3 - <<'EOF'
import json
last=[json.loads(l) for l in open('data/request-log.jsonl')][-1]
assert last['status'] != 200
assert last.get('upstream_err'), "失败请求必须带上游原文"
print('OK:', last['upstream_err'][:120])
EOF
```

---

## 7. 与既有设计的关系（避免重复造轮子）

本仓**已经**有两条正确的先例，建议 1 就是把它补齐，而不是新发明：

1. `Decision`（"判决留痕"）—— 抄自 new-api 的 `PolicyDecision`，**已经证明有用**
   （本次就是靠 `attempts=5 last=codar server 403` 才定位到方向）
2. `ClientIP` / `ClientUA` —— 2026-10-20 补的，注释写着"两条路的观测能力不该不同"

`UpstreamErr` 与它们**同一性质**：都是"排障要的那一列"。缺了它，
`Decision` 只能告诉你"我放弃了"，不能告诉你"为什么"。

---

## 8. 本报告未做的事

- ❌ 未改任何代码
- ❌ 未改任何配置
- ✅ 只读取了 `data/request-log.jsonl` 与源码
