# 报告：`Stream ended without finish_reason`（仅 loomy 渠道）根因定位

> **编者注（2026-11-10 补）**：**⚠ 本报告的问题尚未修复（2026-11-10 复核）**：`internal/wire/sse.go` 的 `inBandErrorOf` 仍只识别「`error_code`/`error_msg`」与「顶层 `error`」两种形态，**未支持 loomy 的 `code`+`desc` 形态**；「流式响应零 `data:` 帧时把整段正文交给分类器」这条治本改法也**未实现**。提交 `ccade18` 只是**用测试记录**了「封禁判不出来」这一现状，不是修复。


- **报告日期**：2026-11-09
- **现象**：客户端（`deepseek-harness`）报「本轮运行失败 Stream ended without finish_reason」
- **特征**：**只有 loomy 渠道会**，换其它上游模型就正常
- **状态**：**仅探究，未改任何代码 / 配置**

---

## 0. 结论（一句话）

这条报错**不是本仓的文案**（它来自客户端），但它由本仓的一个真实缺陷触发：

> **loomy 的会话失效错误是「HTTP 200 + 裸 JSON（非 SSE）」，而我们的流内错误识别既只扫 SSE 帧、又只认两种字段形态 ⇒ 这个错误被当成"正常但零帧的流"透传**，客户端因此拿不到 `finish_reason`。
> 同时该账号**不会被记错、不会冷却、不会被标记为 session dead**，于是每次轮到它就复现一次。

---

## 1. 报错来源：不是本仓产生的

```bash
grep -rn "Stream ended without" --include='*.go' internal/ cmd/     # 无命中
grep -rn "Stream ended without" internal/server/*.html/*.js         # 无命中
```

⇒ 由客户端自己产生（客户端在流结束时要求最后一帧带 `finish_reason`，没等到就报这句）。

---

## 2. 现场证据（可复核）

### 2.1 失败请求的形态：**同一个账号、200、零帧、0.2–0.4 秒**

`data/request-log.jsonl` 与容器日志（05:45:58–05:46:28 窗口）：

| 序号 | 时间 | 模型 | mode | status | ttfb | tokens | out | total |
|---|---|---|---|---|---|---|---|---|
| 299802 | 05:45:58 | loomy/DeepSeek-V4.1- | stream | **200** | **0** | **-1** | None | **365ms** |
| 299803 | 05:45:58 | loomy/… | stream | 200 | 0 | -1 | None | 216ms |
| 299804–299808 | 05:46:08–18 | loomy/… | stream | 200 | 0 | -1 | None | 281–394ms |
| 299810 | 05:46:27 | loomy/… | stream | 200 | 0 | -1 | None | 360ms |
| 299811 | 05:46:28 | loomy/… | stream | 200 | 0 | -1 | None | 187ms |

**同一时间窗内 workbuddy 的流全部正常**（`ttfb≈5-6s`、`out_tokens` 有值、`total≈6-27s`）。

⇒ `tokens=-1` 是本仓"没读到 usage"的哨兵值 ⇒ 该流**一帧都没有**。

### 2.2 失败账号：`260927002515765204`

9 次失败全部落在它一个号上（从 JSONL 取完整 uid 得到，控制台日志只显示前 8 位）。

它在池里的状态：

```
credits     0            free_quota  0
cooling     False        disabled    False
last_err    0001-01-01   （零值 —— 从未被记过一次错）
success_count 1128
```

### 2.3 直连上游（用它自己的 session，绕开网关）

```
POST https://loomyad.xunfei.cn/api/v1/chat/completions   （stream: true）
→ {"code":"100002","desc":"登录已失效，请重新登录","trace_id":"..."}
→ HTTP = 200        总耗时 ≈ 0.19s
```

**两个要点**：

1. 正文是**裸 JSON**，**不是 SSE**（连一个 `data:` 都没有）——尽管请求是 `stream:true`；
2. HTTP 状态是 **200**（不是 401/403），所以"看状态码判错"这条路是通的，但会被当成成功。

### 2.4 抽样：0 额度号分两种形态，其中一种我们识别不了

对 5 个 `credits<=0` 的 loomy 号直连上游，得到两类返回：

| 返回形态 | 样本 | 本仓能否识别 |
|---|---|---|
| `{"error":{"message":"积分余额不足，请获取积分后再试","code":402,…}}` | 3 / 5 | ✅ 能（顶层 `error` 形态）⇒ 会被分类为额度耗尽并冷却 |
| `{"code":"100002","desc":"登录已失效，请重新登录"}` | 2 / 5 | ❌ **不能**（第三种形态）⇒ **隐形失败** |

出事的那个号属于后者。

---

## 3. 根因链（四步）

**① 上游形态**：loomy 的**鉴权 / 会话失效**错误是「HTTP 200 + 裸 JSON 信封」，且**不随 `stream:true` 变成 SSE**。

**② 识别器只认两种字段形态**：`internal/wire/sse.go` 的 `inBandErrorOf`：

- 形态 1：`error_code` + `error_msg`（华为 / CodeArts 系）
- 形态 2：顶层 `error`（OpenAI 系）
- **没有形态 3**：`code` + `desc`（loomy 实测形态）⇒ 直接 `return nil, false`

**③ 识别器只扫 SSE 帧**：它在「解析出的 `data:` 帧」上跑。loomy 这份是**裸 JSON**，一个帧都没有 ⇒ 即使补上形态 3，**这条路径根本不会被调用**。

**④ 于是整条链静默**：

```
loomy 返回 200 裸 JSON
  → 本仓按"200 成功流"处理，但正文没有 data: 帧
  → 零帧 ⇒ 没有 usage（tokens=-1）、没有 finish_reason
  → 不调 applyErrorPolicy ⇒ 账号不记错、不冷却、**不标记 session dead**
  → 账号留在轮换候选里 ⇒ 下次命中再复现
  → 客户端收到"200 但流没有 finish_reason" ⇒ 报「Stream ended without finish_reason」
```

**关键旁证**：本仓的 loomy 分类器**本来就认识这个错误**：

```go
// internal/loomy/errorclassifier.go
markerSessionDead = "登录已失效"     // → ErrKindSessionDead（永久禁用 + 要求人工重登）
```

它只是**永远收不到这段正文** —— 因为第 ② / ③ 步先把响应判成了"正常流"。

---

## 4. 为什么"只有 loomy"

其它上游的鉴权 / 额度失败至少满足下面之一，因此都能被识别：

- 返回**非 200**（401/403/402/429）⇒ 走 `status >= 400` 分类路径；
- 返回 **SSE 内的错误帧**（`data: {...}`）⇒ 走 in-band 路径；
- 用**顶层 `error`** 形态（实测其它渠道的 402 就是这种）⇒ 形态 2 认得。

loomy 恰好是三者都不满足的唯一形态：**200 + 非 SSE + `code`/`desc`**。

---

## 5. 影响面（量化）

```
loomy 账号总数           116
credits <= 0 的           21   （18%）
其中 cooling/disabled      0   ← 全部仍在池里
```

- 抽样 5 个 0 额度号：3 个「积分不足」（可识别 → 会被冷却）、**2 个「登录已失效」（不可识别 → 隐形失败）**；
- 本次出事的 `260927002515765204` 属于后者；
- 它现在额度视图已是 0，因此会被"明确知道没额度"的过滤跳过（`creditsExhausted`）——**但这是巧合，不是修复**：
  - 它**没有被标记为 session dead**，仍显示 `disabled=false`；
  - 一旦额度视图回到 >0（刷新拿到旧值 / 上游恢复），或触发"全被筛掉时的兜底放回"（`pool.go` 里 `if len(cands)==0 && len(skipped)>0 { cands = skipped }`），它就会**再次被选中并复现**。

---

## 6. 修复方向（**仅建议，未实施**）

按性价比排序：

1. **补第三种信封形态**：在 `inBandErrorOf` 里加 `code` + `desc`（loomy 实测原文）。改动最小，但只解决"已经是 SSE 帧"的情形。
2. **流式路径识别"200 但正文不是 SSE"**（治本）：流式响应若**一个 `data:` 帧都没有**，就把整段正文交给 `classifyErr(provider, 200, body)` —— 这样 loomy 分类器的 `登录已失效` 命中 ⇒ `ErrKindSessionDead` ⇒ 账号进"需人工重登"，不再被选中。
3. **"200 且零帧"单独留痕**：现在它在日志里只表现为一行 `ttfb=- tok=- total=0.2s`，与"客户端断开"完全同形，极难发现。建议记一条显式日志/计数（例如 `stream=zero_frame`），便于日后一眼定位。
4. 可选：把 `code`（100002）保留进错误码，让客户端能区分"需要重登"与"额度不足"。

> 附：这四条都属于「出口层语义」范畴，与 `pool` 的选号策略无关 —— 也就是说**不需要动选号逻辑**。

---

## 7. 复现步骤（任何人可执行）

```bash
# 1) 取一个 credits<=0 的 loomy 号（控制台账号池可见），从 auths/loomy/<uid>.json 拿 session
cat > /tmp/c.curl <<'EOF'
header = "Authorization: Bearer <该号的 session>"
EOF

# 2) 直连上游，观察形态（注意 HTTP 是 200，正文却不是 SSE）
curl -s -N -m 30 -K /tmp/c.curl -w '\n[HTTP=%{http_code} 总=%{time_total}s]\n' \
  -X POST https://loomyad.xunfei.cn/api/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"DeepSeek-V4.1-Flash","stream":true,"max_tokens":5,
       "messages":[{"role":"user","content":"hi"}]}'

# 期望看到（会话已失效的号）：
#   {"code":"100002","desc":"登录已失效，请重新登录",...}
#   [HTTP=200 总=0.19s]
```

**经网关复现**：让网关选到该号即可（并发多打几次；或不带 `tried` 的单次请求），
随后在请求日志里会看到 `status=200 / ttfb_ms=0 / tokens=-1 / total_ms≈200-400`。

---

## 8. 本报告未做的事

- ❌ 未修改任何 Go 代码
- ❌ 未修改配置
- ❌ 未重启服务
- ✅ 仅读取日志、数据库、账号池状态，并对上游做只读探测（每次 1 个 5-token 请求）
