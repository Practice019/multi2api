# Codar 实测验证报告（live check）

> 执行者：T-311（承接 T-307/T-309/T-310）。
> 时间：2026-11-08 02:1x（Asia/Shanghai）。**本报告全部结论来自真实请求**，非源码推断。
> 消耗控制：**共 5 次请求**（catalog ×2[其中一次 URL 拼错]、缺 model 的 400 ×1、非流式 200 ×1、SSE ×1），
> `max_tokens ≤ 20`；**未触发 403/429**（因此没有触发"遇风控立即停"的分支）。
> token 全程只在内存变量里，报告只给长度与前 8 位；未改 `config.json`、未登录/注册。

---

## 0. 结论摘要

| 验证项 | 结果 | 证据 |
|---|---|---|
| **api_base 真实值** | **`https://api.claw.speedai.com/v1`** | `~/.codar/config.json` → `provider.api_base` **与** `provider.provider_profiles.speedai.api_base` **都**是这个值（"之前读到空"应是读到了别的键/旧副本） |
| **聊天端点** | ★ **`POST https://api.claw.speedai.com/v1/chat/completions` ⇒ HTTP 200** | 真实请求；`Server: nginx/1.24.0 (Ubuntu)`，`x-request-id: c66a935e-…` |
| **鉴权方式** | **`Authorization: Bearer <speedai token>`**（该 token 即桌面端 `provider.speedai.token`，len=207） | 200 响应；目录里 11 个模型的 `protocol.authentication` 明确写 `bearer` |
| **请求体格式** | **标准 OpenAI**：`{model, messages[{role,content}], max_tokens, stream}`（未加任何私有字段即 200） | 见 §3 |
| **响应结构** | **标准 OpenAI + 4 个私有扩展**：`reasoning_content`、`billing{quota,cost_cny}`、`service_tier`、`prompt_tokens_details.cached_tokens` | 见 §3 |
| **SSE** | ★ **标准 OpenAI 事件流**：`data: {choices:[{delta:{content,reasoning_content,role}}]}` × N + `data: [DONE]` | 见 §4 |
| **错误格式** | `{"error":{"message":"Missing 'model' field","type":"invalid_request_error","code":400}}` | 见 §5（我故意漏 model 打出来的 400） |

---

## 1. 凭据（脱敏）

```
来源        : %USERPROFILE%\.codar\config.json（**只读**，未改）
auth_type   : speedai
api_type    : openai
api_base    : https://api.claw.speedai.com/v1        ← provider 顶层 + provider_profiles.speedai 两处一致
active_profile: speedai
token       : eyJhbGci…  len=207                       ← JWT（前 8 位 + 长度；明文未落任何文件）
username    : user_1508……_7985（**中间位脱敏**）
login_at    : 1794070329644 (ms)
```
**附**：模型目录是**免鉴权**的 ⇒ 探测模型与价格不需要凭据（`GET /v1/codar/models` 直接 200）。

---

## 2. 模型列表（15 个，含协议与价格）

`GET https://api.claw.speedai.com/v1/codar/models`（免鉴权）⇒ `schema_version: 2`，`data[]` 15 项。

| 模型 id | protocol.type | path | authentication | 价格（CNY / 百万 token，in/out） |
|---|---|---|---|---|
| `auto` | openai_chat_completions | /v1/chat/completions | **bearer** | 2.0 / 8.0 |
| `glm-5.3-flash` | openai_chat_completions | /v1/chat/completions | bearer | ★ **0.8 / 2.8（最便宜）** |
| `gpt-6.1-sol` | openai_chat_completions | /v1/chat/completions | bearer | 1.6 / 8.0 |
| `gpt-6-sol` | openai_chat_completions | /v1/chat/completions | bearer | 1.6 / 8.0 |
| `deepseek-v4.1-flash` | openai_chat_completions | /v1/chat/completions | bearer | 2.0 / 8.0 |
| `deepseek-v4-flash` | openai_chat_completions | /v1/chat/completions | bearer | 2.0 / 8.0 |
| `gpt-5.6-sol` | openai_chat_completions | /v1/chat/completions | bearer | 4.0 / 24.0 |
| `gpt-6-astra` | openai_chat_completions | /v1/chat/completions | bearer | 6.2 / 31.0 |
| `glm-5.2` | openai_chat_completions | /v1/chat/completions | bearer | 8.0 / 28.0 |
| `glm-5.3` | openai_chat_completions | /v1/chat/completions | bearer | 8.0 / 28.0 |
| `deepseek-v4-pro` | openai_chat_completions | /v1/chat/completions | bearer | 9.0 / 27.0 |
| `claude-opus-5-5-kiro` | **anthropic_messages** | **/v1/messages** | ★ **x-api-key** | 4.0 / 20.0 |
| `claude-sonnet-5-5` | anthropic_messages | /v1/messages | x-api-key | 5.0 / 25.0 |
| `claude-opus-5-5` | anthropic_messages | /v1/messages | x-api-key | 10.0 / 50.0 |
| `claude-sonnet-5-5-kiro` | anthropic_messages | /v1/messages | x-api-key | （`pricing` 为空） |

每条目录项还带：`context_window`（多为 1,000,000）、`input_modalities`、`output_modalities`、
`request_schema`、`defaults`、`reasoning`、`capabilities`、`parameter_notes`、`rate_badge`、`sort_order`。

**⇒ 两条重要结论**
1. **协议是"每模型自带"的**：`openai_chat_completions`（11 个，Bearer）与 `anthropic_messages`（4 个，**x-api-key**）**不是同一套鉴权**。
2. ★ **目录里的 `path` 字段就是 `/v1/chat/completions`（OpenAI）或 `/v1/messages`（Anthropic）** ——
   **不存在 `/model` 这种路径**（T-306 那条 `/model` 记录可据此结案）。

---

## 3. 非流式请求（完整请求 + 响应）

**请求**（第 4 次，真实发出）
```http
POST https://api.claw.speedai.com/v1/chat/completions
Authorization: Bearer eyJhbGci…(len=207)
Content-Type: application/json
Accept-Encoding: identity
User-Agent: codar-desktop/0.3.3

{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"1+1=?"}],"max_tokens":8,"stream":false}
```
**响应**：`HTTP 200`，`Server: nginx/1.24.0 (Ubuntu)`，`x-request-id: c66a935e-9e46-45c5-890c-64c246043c31`
```json
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,
  "message":{"content":"","reasoning_content":"We need answer simple. Need respond likely","role":"assistant"}}],
 "created":1794075232,"id":"0217940752321685fbc2824c96fe07bca989dad8a4db3a0a8b642",
 "model":"deepseek-v4-1-flash-261017","service_tier":"default","object":"chat.completion",
 "usage":{"completion_tokens":8,"prompt_tokens":34,"total_tokens":42,
          "prompt_tokens_details":{"cached_tokens":0},
          "completion_tokens_details":{"reasoning_tokens":8}},
 "billing":{"quota":0,"cost_cny":0.0}}
```
**要点**
- 结构 = **标准 OpenAI**（`choices[].message.content`）⇒ 任何 OpenAI 客户端可直接用 ✓
- **`reasoning_content`**：推理模型把思维链放在这里（不是 `content`）✓
- ⚠️ `max_tokens=8` 被 `reasoning_tokens: 8` **全部吃掉** ⇒ `content` 为空（实际用要给足 `max_tokens`，例 200+）
- **`billing: {quota, cost_cny}`** 是它独有的：**每次响应直接给出本次花费**（本次 0.0）⇒ 可做成本记账 ✓
- 请求的 `deepseek-v4.1-flash` 在响应里回显为内部名 `deepseek-v4-1-flash-261017`（**不要拿回显名当请求名**）

---

## 4. 流式请求（SSE，前 5 个事件）

**请求**：同上但 `{"model":"deepseek-v4.1-flash", …, "max_tokens":20, "stream":true}`（`Accept-Encoding: identity`）
**结果**：3.33 KB 事件流，格式 = **标准 OpenAI SSE**
```
data: {"choices":[{"delta":{"content":"","reasoning_content":"","role":"assistant"},"index":0}],"created":1794075234,"id":"021794075234…"}
data: {"choices":[{"delta":{"content":"","reasoning_content":"We","role":"assistant"},"index":0}],"created":1794075234,"id":"…"}
data: {"choices":[{"delta":{"content":"","reasoning_content":" need answer count","role":"assistant"},"index":0}],"created":1794075234,"id":"…"}
data: {"choices":[{"delta":{"content":"","reasoning_content":" 1 to 3.","role":"assistant"},"index":0}],"created":1794075234,"id":"…"}
（…中间省略若干 delta…）
data: [DONE]
```
⇒ 事件之间空行分隔、以 `data: [DONE]` 收尾 —— 与 codar2api 的透传实现完全吻合（它只是 `io.Copy` 转发）。

---

## 5. 错误响应格式

**样本**（我故意只发 `messages` 不写 `model`，得 400）：
```json
{ "error": { "message": "Missing 'model' field",
             "type": "invalid_request_error",
             "code": 400 } }
```
⇒ 错误体**同时有 HTTP 状态码与 body 内 `code`**（两者一致），`type` 用 OpenAI 风格命名（`invalid_request_error`）。
**上游错误可直接透传给调用方**（codar2api 也是原样回写，`relay.go:397-399`）。

**风控观测**：本次 5 次请求**未出现 403/429**；但 T-307 已记录社区反馈（2026-11-01 起出现
`API access is restricted. Please contact support.`）⇒ 接入时仍须做限速/冷却。

---

## 6. 与 codar2api 源码对照

| 项 | 源码（T-310） | 本次实测 | 一致？ |
|---|---|---|---|
| API 基址 | `APIBase = https://api.claw.speedai.com` | `api_base = https://api.claw.speedai.com/v1` | ✓ |
| 聊天路径 | `target = APIBase + r.URL.Path`（客户端 path 直通） | `POST /v1/chat/completions` ⇒ 200 | ✓ |
| 鉴权 | `Authorization: Bearer <账号 token>` | 同样 ⇒ 200；目录里 openai 系模型标注 `bearer` | ✓ |
| UA | 默认伪装 `codar-desktop/0.3.3` | 本次照用（未对照"不伪装"的差异） | ✓（照搬） |
| 请求体 | 原样透传（默认） | 标准 OpenAI body 直接可用 | ✓ |
| SSE | 原样回写 | 标准 `data:`/`[DONE]` | ✓ |
| **`/model` 路径** | **源码里没有** | 目录 `path` 明确是 `/v1/chat/completions` | ✗ **T-306 记录有误，应作废** |
| Anthropic 系模型的鉴权 | 它转发时只设 `Bearer`，但 **`copyForwardedHeaders` 会透传客户端的 `x-api-key`** | 目录要求 `x-api-key` | ⚠ 见未决 3 |

---

## 7. 结论：能否接入 + 关键风险

**结论：能接入，且是本仓目前"最省事"的一类上游。**
```
上游实现只需要：base_url = https://api.claw.speedai.com/v1  +  Authorization: Bearer <speedai token>
路径与请求体就是标准 OpenAI ⇒ 不需要任何协议改写（连 PrepareBody 都可以不加特判）
凭据：可直接读桌面端 .codar\config.json（provider.speedai.token），或走它自己的登录面（api.kuaipaper.com）
模型：GET /v1/codar/models 免鉴权 ⇒ 可做目录同步与价格展示
成本：响应里的 billing.cost_cny 可直接用于记账（本仓当前没有这个维度）
```

**关键风险 / 注意**
1. **风控**：社区已有 `API access is restricted` 报告（T-307）⇒ 必须限速 + 账号/出口冷却；建议抄 codar2api 的"IP 出口封控"与"会话粘滞"。
2. **两类鉴权不可混**：`openai_chat_completions` 模型用 `Bearer`；`anthropic_messages` 模型（4 个 claude）要求 **`x-api-key`** ⇒ 上游若统一只用 Bearer，需先验证这 4 个是否也接受 Bearer（未测，见未决 3）。
3. **reasoning 吃额度**：推理模型的 `max_tokens` 会被 `reasoning_tokens` 吃满 ⇒ 客户端要给足上限，否则 `content` 为空（本仓转发时不要给默认小上限）。
4. **模型名映射**：请求名 ≠ 响应回显名（`deepseek-v4.1-flash` → `deepseek-v4-1-flash-261017`）⇒ 统计/记账要用请求名。
5. **`auto` 模型**：目录第一位是 `auto`（2.0/8.0）⇒ 可作为默认，但价格高于 `glm-5.3-flash`（0.8/2.8）。

---

## 8. 未决

1. **未测** Anthropic 系模型（`/v1/messages` + `x-api-key`）—— 受"≤5 次请求"约束，留待后续。
2. **未测** 无 UA 伪装（`User-Agent` 用别的值）是否仍 200 —— 不确认上游是否校验 UA。
3. **未测** `openai_chat_completions` 模型传 `x-api-key` 会怎样（两者能否互换）。
4. **未测** 429/403 的真实触发条件与错误体（本次没撞上）。
5. **未测** `GET /v1/codar/usage/daily`（用量/额度）与 `POST api.kuaipaper.com/v1/daily-sign/status`（签到）—— T-310 从源码读到，未实测。
6. 本次 5 次请求的**实际花费 = 0.0 CNY**（`billing.cost_cny: 0.0`，且未消耗积分迹象），但仍消耗了 `prompt_tokens` 记账（34），**未做更多请求**以免超约束。
