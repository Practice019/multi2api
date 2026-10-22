# MonkeyCode 客户端逆向报告

> 逆向目标：`ohmyagent.exe`（MonkeyCode 桌面客户端内核，Go 编译，27.4 MB）
> 版本标识：UA `ohmyagent f6b21ad`
> 日期：2026-10-23
> 状态：**协议完全打通，纯 HTTP 可用，无需客户端**

---

## 0. 一句话结论

MonkeyCode 的模型接口需要一个 `X-OhMyAgent-Signature` 头，它**不是**对请求体签名，而是：

```
X-OhMyAgent-Signature = "v1=" + HMAC-SHA256(signing_secret, instructions)
```

`instructions` 就是 OpenAI Responses / Anthropic Messages 请求里那段**静态 system prompt**。
签名与 `input`、`tools`、`model`、`stream` 等一切动态内容无关。

**验证**：两个不同 body 的捕获样本均命中；纯 HTTP 复现后 `HTTP 200`，真实返回 `"收到"`。

---

## 1. 逆向方法

| 步骤 | 手段 | 结果 |
|---|---|---|
| 1 | 字符串搜索二进制 | 找到符号 `promptauth.Sign` 与文件 `promptauth/signature.go` |
| 2 | 提取内嵌文档 | 找到 `HMAC-SHA256` + `X-OhMyAgent-Signature` 的说明段落 |
| 3 | **本地中间人捕获** | 把客户端 `base_url` 指向自建服务器，拿到带签名的明文请求 |
| 4 | 假设验证 | 按文档"只签静态 system"逐个字段试，命中 |

### 关键突破点

文档原文（二进制内嵌，`docs` 段）：

> Agent 不会新增一段专用于签名的提示词。它从已有提示词中拆出不依赖会话运行态的通用指令作为第一条，
> 使用 `signing_secret` 对其计算 HMAC-SHA256，并通过 `X-OhMyAgent-Signature` 发送；
> Task/Todo 模式、工具、技能、Agent 列表、AGENTS.md、项目、环境、Git 和模型信息等动态内容放在后续
> system/developer 条目中，**不参与签名**。

### 捕获方法（可复现）

```js
// 起一个本地服务器，把收到的请求连同签名一起存下来
http.createServer((req, res) => {
  // 记录 req.headers['x-ohmyagent-signature'] 和 body
  // 回一个最小的 OpenAI Responses 响应，让客户端正常收尾
})
```

然后给客户端一份配置，`base_url` 指向本地：

```json
{
  "model": "monkeycode-basic/qwen3.8-flash",
  "name": "cap",
  "type": "openai-responses",
  "api_key": "oma_...",
  "signing_secret": "omas_...",
  "base_url": "http://127.0.0.1:8803/v1"
}
```

`ohmyagent.exe -p hi --model-config @cfg.json` 即可捕获。

⚠ **坑**：`ohmyagent` 会读 stdin，用 `Start-Process` 重定向会报
`read /dev/stdin: The handle is invalid`。必须用 `cmd /c ... < NUL`。

---

## 2. 签名算法（核心）

### 2.1 精确公式

```go
sig := "v1=" + hex(HMAC-SHA256(key=signing_secret, msg=instructions))
```

- `signing_secret`：`omas_` 开头，来自 `POST /api/v1/users/ohmyagent/api-keys`
- `instructions`：请求里的静态 system prompt 原文，**不要做任何变换**（不 trim、不加换行）
- 输出：`v1=` + 64 位小写十六进制

### 2.2 已排除的假设（全部错误，留档避免重走）

| 假设 | 结果 |
|---|---|
| HMAC(secret, rawBody) | ✗ |
| HMAC(secret, sha256(body)) | ✗ |
| HMAC(secret, base64(body)) | ✗ |
| JSON 重排后签名 | ✗ |
| Go `json.Marshal` 的 `<>&` 转义 | ✗ |
| method + path + body 拼接 | ✗ |
| 混入 `api_key` | ✗ |
| 含时间戳 / nonce | ✗ **确认无时间成分**（同 body 隔数小时签名不变） |
| 签名整个 body | ✗ |

**教训**：不要把"确定性的哈希"当成"对整体输入签名"。确定性同样可以由"对不变部分签名"解释。

### 2.3 重要性质

| 性质 | 实测 |
|---|---|
| 确定性 | ✓ 同一 `instructions` 隔数小时签名完全相同 |
| 与 body 无关 | ✓ 同 `instructions` + 不同 `input` → 签名相同 |
| `api_key` 不参与 | ✓ 换 key 不换签名 |
| 换 `signing_secret` 会变 | ✓ |
| 可离线计算 | ✓ 不需要请求任何服务 |

### 2.4 ⚠ 签名与 instructions 强绑定

签名覆盖的是**你发出去的那段 instructions**。所以：

- 可以自定义 instructions（用自家 system prompt），但必须对**自己发的那段**算签名
- 不能用 A 的签名配 B 的 instructions（会 403 `invalid ohmyagent request`）

这意味着**不需要模仿官方的 5170 字节 prompt**，自己写一段即可，只要签名对得上。

---

## 3. 接口清单

### 3.1 双协议入口

| 协议 | 端点 | body 形态 | 签名对象 |
|---|---|---|---|
| OpenAI Responses | `POST /v1/responses` | `{"model","instructions","input",...}` | `instructions` 字段 |
| Anthropic Messages | `POST /v1/messages` | `{"model","system","messages",...}` | `system` 字段 |

基址随站点：

```
国内站  https://proxy.monkeycode-ai.com/v1
国际站  https://proxy.monkeycode-ai.net/v1
```

两个协议都必须带：

```
Authorization: Bearer oma_...
X-OhMyAgent-Signature: v1=<64hex>
Content-Type: application/json
```

Anthropic 额外需要 `anthropic-version: 2023-06-01`。

**不带签名 → `403 {"error":"invalid ohmyagent request"}`**

### 3.2 管理端点（普通 session cookie，无签名）

| 功能 | 方法 | 路径 |
|---|---|---|
| 签发 API Key | POST | `/api/v1/users/ohmyagent/api-keys` → `{api_key, signing_secret}` |
| 模型列表 | GET | `/api/v1/users/models?limit=200` |
| 额度 | GET | `/api/v1/users/wallet` |
| 签到 | POST | `/api/v1/users/wallet/checkin` |
| 账号信息 | GET | `/api/v1/users/status` |
| 订阅 | GET | `/api/v1/users/subscription` |

认证：`Cookie: monkeycode_ai_session=<uuid>`

---

## 4. 实测结果

### 4.1 Chat 打通

```
=== OpenAI Responses ===
HTTP 200  status=completed  model=monkeycode-basic/qwen3.8-flash
usage: input=1635 output=126 total=1761 (reasoning=125)
输出: "收到"

=== Anthropic Messages ===
HTTP 200  content[0].type=thinking  content[1].type=text
```

### 4.2 流式（SSE）

`stream: true` → `HTTP 200`，48 行 SSE，事件序列：

```
response.created → response.in_progress
→ response.output_item.added (×2)
→ response.reasoning_summary_part.added
→ response.reasoning_summary_text.delta/done
→ response.output_text.delta (×10)
→ response.output_text.done
→ response.output_item.done (×2)
→ response.content_part.done
→ response.completed
```

### 4.3 模型档位（国内站 basic 账号）

| 模型 | 协议 | 结果 |
|---|---|---|
| `monkeycode-basic/qwen3.8-flash` | openai-responses | ✓ 200 |
| `monkeycode-basic/qwen3.5-plus` | anthropic | ✓ 200 |
| `monkeycode-basic/deepseek-flash` | anthropic | ✓ 200 |
| `monkeycode-pro/*` | — | ✗ 403（basic 权限不够） |
| `monkeycode-ultra/*` | — | ✗ 403 |

⚠ **协议选错会 404**（`路由不存在`），不是 403。这是最容易误判的地方：
`qwen3.8-flash` 走 `openai-responses`，其余 basic 模型走 `anthropic`。

### 4.4 额度

```json
{"balance": 200000, "daily_token_balance": 9973782, "daily_token_limit": 10000000}
```

---

## 5. 对 workbuddy2api 上游接入的意义

### 5.1 可行性：完全可行

| 契约方法 | 实现方式 |
|---|---|
| `ID()` | `"monkeycode"` |
| `Caps()` | `CapChat \| CapModels \| CapQuotaProbe \| CapCheckin \| CapImport` |
| `Chat()` | 双协议：按 model 的 `interface_type` 选 `/v1/responses` 或 `/v1/messages` |
| `Models()` | `GET /api/v1/users/models` + 缓存兜底 |

### 5.2 凭证形态

```json
{
  "auth": {
    "session": "monkeycode_ai_session 的 uuid",
    "api_key": "oma_...",
    "signing_secret": "omas_...",
    "region": "cn" | "intl",
    "nickname": "...",
    "userid": "..."
  },
  "remark": "..."
}
```

文件：`auths/monkeycode/monkeycode-<uid>.json`

### 5.3 关键设计决策

1. **必须自己维护 instructions 常量**。签名对象必须与发出的 `instructions` 逐字一致。
   建议在上游包里定义 `const defaultInstructions = "..."`，签名和请求都引用它，
   避免两处漂移导致 403。

2. **`models` 必须记录每个模型的 `interface_type`**。选错协议 = 404，
   而 404 会被 `ErrorClassifier` 归成 `ErrKindNotFound`（短冷却），
   实际上这是**代码 bug**，应该在上游内部路由正确，不该让它冒到分类层。

3. **不实现 `ErrorClassifier` 的 403 分支**就会误伤：`403 Forbidden` 在上游语义里
   是"该模型不在你的套餐内"，属于 `ErrKindClient`（只换号不罚），
   不是 `ErrKindAuth`（换号）。判错了会把好号禁掉。

4. **无续期**：session 30 天硬过期，没有 refresh 接口。所以
   `CredentialLifetimeExt` 返回 `false`，`RefreshSkewExt` 不实现。

### 5.4 待确认项

- 国际站（`proxy.monkeycode-ai.net`）是否同一套签名 → **结构应该一致，但未实测**
- `CapCheckin` 需要 cap.js PoW（已在国内站验证算法，可在上游里实现或先不声明）
- 流式事件是否需要转成 OpenAI Chat 格式 → 取决于 `wire` 包现有抽象

---

## 6. 复现脚本

```bash
# 1. 算签名
SIG="v1=$(printf '%s' "$INSTRUCTIONS" | openssl dgst -sha256 -hmac "$SIGNING_SECRET" | awk '{print $2}')"

# 2. 调 OpenAI Responses
curl -X POST https://proxy.monkeycode-ai.com/v1/responses \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-OhMyAgent-Signature: $SIG" \
  -d '{"model":"monkeycode-basic/qwen3.8-flash",
       "instructions":"'"$INSTRUCTIONS"'",
       "input":[{"type":"message","role":"user","content":"hi"}],
       "stream":false,"store":false}'

# 3. 调 Anthropic Messages
curl -X POST https://proxy.monkeycode-ai.com/v1/messages \
  -H "Authorization: Bearer $API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -H "X-OhMyAgent-Signature: $SIG" \
  -d '{"model":"monkeycode-basic/qwen3.5-plus","max_tokens":64,
       "system":"'"$INSTRUCTIONS"'",
       "messages":[{"role":"user","content":"hi"}]}'
```

---

## 7. 附录：二进制里的关键字符串

```
github.com/chaitin/ohmyagent/internal/promptauth.Sign
github.com/chaitin/ohmyagent/internal/promptauth/signature.go
X-OhMyAgent-Signature
invalid ohmyagent request
OHMYAGENT_API_KEY
OHMYAGENT_SIGNING_SECRET
OHMYAGENT_CONFIG_DIR
```

配置字段（`ModelConfig`）：

```go
type ModelConfig struct {
    Name          string            `json:"name,omitempty"`
    SigningSecret string            `json:"signing_secret,omitempty"`
    Headers       map[string]string `json:"headers,omitempty"`
    SupportsImages *bool            `json:"supports_images,omitempty"`
}
```

## 8. 修订记录

- 2026-10-23：首次完整打通。签名公式确定为 `HMAC-SHA256(secret, instructions)`，
  双协议验证 200。此前"破不出来"是因为错误假设"对整体 body 签名"。
