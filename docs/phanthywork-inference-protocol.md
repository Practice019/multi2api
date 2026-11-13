# PhanthyWork 推理协议：新旧内核对照（2026-11-13 抓包取证）

> 这份文档的唯一目的：**防止以后有人把协议改回 `/v1/messages`**。
> 每一条都有抓包原文或真机响应作证据，不是推测。

## 一句话结论

| 内核 | 版本 | 推理端点 | 签名 | 本包默认 |
|---|---|---|---|---|
| **Phanthy Codeo**（新版） | 0.2.13 | `POST /v1/chat/completions` | **无** | ✅ `protocol=chat` |
| claude-sidecar（旧版） | — | `POST /v1/messages` | 12 段 ed25519 | `protocol=messages` |

**默认走新版**。旧版仅在明确对接 claude-sidecar 时才需要。

---

## 取证方法（可复跑）

```powershell
# 让 bun 把每个出站请求打成 curl
$env:BUN_CONFIG_VERBOSE_FETCH = 'curl'
& "$env:USERPROFILE\.bun\bin\phanthy-codeo.exe" run "1+1=?" *> D:\temp\ph-test\verbose.log
```

然后统计 POST 目标：

```powershell
Get-Content D:\temp\ph-test\verbose.log |
  Select-String -Pattern 'HTTP/1\.1 POST https' |
  ForEach-Object { [regex]::Match($_.Line,'POST (https://[^\s]+)').Groups[1].Value } |
  Sort-Object -Unique
```

**实测输出**（0.2.13）：

```
https://code.phanthy.com/api/event_logging/batch
https://code.phanthy.com/v1/chat/completions
```

`/v1/messages` 出现次数 = **0**。

---

## 请求形态逐项对照

### 新版（`/v1/chat/completions`）—— 抓包原文

```http
POST /v1/chat/completions HTTP/1.1
Authorization: Bearer <access_token>
Content-Type: application/json
User-Agent: phanthy-codeo/0.2.13
X-Phanthy-Client-Source: cli
X-Phanthy-Product: phanthy_code
x-phanthy-cli-device: <base64 设备指纹 JSON>
x-phanthy-client: cli
x-phanthy-project: global
x-phanthy-request: <uuid v4>
x-phanthy-session: ses_<24 位 cuid2>
Accept: text/event-stream
```

body（OpenAI 格式，`stream_options` 是特征字段）：

```json
{
  "model": "Phanthy-fast",
  "max_tokens": 32000,
  "messages": [
    {"role": "system", "content": "..."},
    {"role": "user", "content": "..."}
  ],
  "stream": true,
  "stream_options": {"include_usage": true}
}
```

### 旧版（`/v1/messages`）—— 本包改造前的实现

```http
POST /v1/messages
Authorization: Bearer <access_token>
anthropic-version: 2023-06-01
x-desktop-installation-id: <di_...>
x-desktop-timestamp: <毫秒>
x-desktop-nonce: <随机>
x-desktop-signature: <ed25519>
x-phanthy-desktop-session: <813 字符推理令牌>
x-phanthy-proof-timestamp / -nonce / -signature: <12 段 canonical 签名>
```

body：Anthropic 格式（`system` 是顶层字段，无 `stream_options`）。

---

## 三个「不同端点上取值不同」的头（最容易搞错）

| 头 | 推理端点 | 遥测端点 |
|---|---|---|
| `X-Phanthy-Product` | `phanthy_code` | `phanthy_codeo` |
| `X-Phanthy-Client-Source` | `cli` | `phanthy-codeo` |

代码里已写成 `chatProduct` / `chatClientSource` 两个常量并注明。
**不要合并成一个全局常量** —— 混用会让服务端走错校验链。

---

## `x-phanthy-session` 的形态要求

抓包原文：`ses_e2aa5d923ffe02djYUjBpWTS5I`

- 前缀 `ses_`
- 后缀 **24 位 cuid2**：小写字母 + 数字，**首字符必须是字母**

⚠ 不要用 UUID 或 base64url —— 后者会产出 `-` / `_` / 大写字母，形态不符。
回归断言：`TestChatSendsSessionHeaders`。

---

## 不要「顺手」加回签名头

改造过程中实测过：给 `/v1/messages` 加 `x-phanthy-product` 头之后，
响应从 **500 空体** 变成 **403 带错误码**。

这说明**带签名但对不上**会被服务端当异常处理（500 = 未捕获异常）。
新版内核零签名，跟着它走即可。
回归断言：`TestChatSendsNoSignatureHeaders`（8 个头一个都不许出现）。

---

## 错误码对照（真机实测）

| 响应 | 含义 | 本包分类 | 处置 |
|---|---|---|---|
| `403 {"code":"disabled_account","message":"当前账户已被封禁…"}` | 账号封禁（**终态**） | `ErrKindSessionDead` | 标记并停用该号 |
| `403 {"code":"request_not_authorized","message":"当前请求不可用，请更新或重新打开客户端后重试。"}` | 含糊（拼装问题/令牌问题） | `ErrKindClient` | 换号但**不罚号** |
| `500` 空体 | 旧协议打错路径的表现 | `ErrKindServer` | 换号重试 |

⚠ **`disabled_account` 必须排在状态码分支之前判**：它也是 403，
若让 `status==403` 先命中会落到 `ErrKindAuth`（"刷新一下可能就好"），
于是一个死号被反复刷新、反复重试，永远在池子里空转。
回归断言：`TestBannedBeatsStatusCodeBranch`。

---

## 端到端验证（本次实测）

```powershell
# 用新二进制起测试实例（独立端口，不碰生产）
$env:WB2A_LISTEN   = '127.0.0.1:7870'
$env:WB2A_API_KEY  = 'testkey'
$env:WB2A_AUTH_DIR = '<auths 副本>'
wb2-test.exe -config config.json

# 调
curl -X POST http://127.0.0.1:7870/v1/chat/completions \
  -H "authorization: Bearer testkey" -H "content-type: application/json" \
  -d '{"model":"phanthywork/Phanthy-fast","max_tokens":16,"stream":false,
       "messages":[{"role":"user","content":"hi"}]}'
```

**实测响应**（6 个号全封禁状态的如实反映）：

```json
{"error":{"code":"no_healthy_account",
  "message":"all accounts unavailable … : upstream session_dead (http 403):
   {\"error\":{\"code\":\"disabled_account\",
             \"message\":\"当前账户已被封禁，请加微信群联系客服处理\"}}",
  "type":"api_error"}}
HTTP=503
```

**这段信息本身就证明改造成功**：

| 特征 | 证明 |
|---|---|
| `http 403` + `disabled_account` | 走的是 `/v1/chat/completions`（旧协议只会回 500 空体） |
| `upstream session_dead` | `bannedMarkers` 分类生效 |
| 上游中文原文一字不差 | 响应体完整透传 |
| `no_healthy_account` + 熔断提示 | 核心的账号池逻辑正常 |

**改造前同样请求只会得到**：`upstream server (http 500): `（空消息，看不出任何原因）。

---

## 已知限制（如实记录）

本次上线时，池里 **6 个账号全部 `disabled_account`**（注册机批量注册被平台风控识别）。
因此「真机返回 200」这一项**未能验证** —— 不是协议不对，是没有可用账号。

**待有好号时**，用上面「端到端验证」那段命令即可确认；把 `no_healthy_account`
变成正常 SSE 流就说明全通了。
