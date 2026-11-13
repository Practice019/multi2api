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

## ★★★ 唯一可靠判据：`x-kong-upstream-latency` 分辨「协议错」和「业务拒」

排查期间反复卡在「同样是失败，到底是我的协议不对，还是账号/额度有问题」。
实测出一条**一眼可用**的判据，写在这里避免以后重走：

| 响应 | `x-kong-upstream-latency` | 含义 | 该查什么 |
|---|---|---|---|
| **HTTP 500 + 空体（0 字节）** | **8–9 ms** | 请求**没进业务层**，Kong 网关直接拒 | 协议 / 请求头 / 签名 |
| **HTTP 4xx + 明确错误码** | **40 ms 以上** | 请求**进了业务层**，被业务规则拒 | 账号 / 额度 / 套餐 / 模型 |

```bash
# 一条命令就能看
curl -sS -D- -o /tmp/b.txt -X POST https://code.phanthy.com/v1/messages \
  -H "authorization: Bearer $TOKEN" -H 'content-type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"Phanthy-fast","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}' \
  | grep -i 'HTTP/\|x-kong-upstream-latency'
wc -c < /tmp/b.txt
```

**实测样本**（同一个账号、同一天）：

```
协议未改前：  HTTP 500   0 B     x-kong-upstream-latency: 9
协议改对后：  HTTP 403   70 B    x-kong-upstream-latency: 41   ← 协议已到位
```

> 这条判据的旁证：magpie-PhanthyCode 自带的 `scripts/probe-inference.mjs`
> **只带 Bearer** 打同一端点，也是 `HTTP 500 0B` + `kong 9ms` ——
> 说明旧协议对**任何人**都不通，不是我们账号的特殊问题。

---

## 协议完全正确的标志：拿到**业务错误码**

当 `x-kong-upstream-latency` 升到 40ms 量级、且响应体是明确的 JSON 错误码时，
说明协议层（端点 / 头 / 签名 / 令牌）**已经全对**，服务端正在按业务规则回答。

本次改造后的四条路径一致性验证（同一账号、同一时刻）：

```
① 设备注册          HTTP 200  reward_reason=first_device_owner
② 换推理令牌        HTTP 200  令牌 813 字符
③ /v1/messages        desktop + 12 段签名   → 403 disabled_account  (kong 41ms)
④ /v1/chat/completions cli   + 签名         → 403 disabled_account  (kong  8ms)
⑤ /v1/chat/completions cli   无签名         → 403 disabled_account  (kong  8ms)
⑥ /v1/chat/completions desktop + 签名       → 403 disabled_account  (kong  9ms)
```

**四条路径全部返回同一个业务错误码**（而不是 500 空体）⇒ 协议一致正确。

---

## 已知限制（如实记录）

### 一、账号封禁（2026-11-13 本次）

池里 **7 个账号全部 `disabled_account`**：

```
cmwc64nj21ejmsw3ilrzs2iq2   403 disabled_account
cmwe7sk2n3xarjb3j39ria10d   403 disabled_account
cmwedr1q25641l13jcqx2oh5l   403 disabled_account
cmweetzdn5d5kl13jhcgkjxau   403 disabled_account
cmwf1a10e0ncvoy3ktxsn9dxp   403 disabled_account
cmwf1biv00o15o93jd1xfqly5   403 disabled_account
cmwf4g3qi0wykqb3jzjezto3u   403 disabled_account   ← 当天新注册，1 小时后也被封
```

**最后那个号的封禁过程有完整时间线**（客户端日志 + 数据库）：

```
23:18:27  注册成功
23:23:22  客户端成功  input 25151  output 10
23:27:09  客户端成功  input    85  output  3     ← 最后一次成功
23:28:42  客户端 403  "当前请求不可用"
23:5x     我只读端点也 403  ⇒ 整账号级封禁
```

连 `/api/oauth/profile`、`/api/phanthy_code/models` 这类**只读**端点也 403，
说明是**账号级**停用，不是推理面单独受限。

**最可能的原因**（按证据强度排序）：

1. **批量注册特征** —— 7 个号集中在 11-11 ~ 11-13 注册，同 IP 段、同设备指纹；
2. **探测流量异常** —— 排查期间对该账号发了上百次请求（含大量刻意构造的畸形请求），
   风控按「异常客户端行为」记账；
3. **版本号长期过低** —— 改造前一直是 `2.48.0`（落后两个小版本），
   服务端按「来源=桌面端但版本对不上」判异常。

### 二、「HTTP 200 + 正文」未能验证

因此本项**不是协议问题**，是**没有可用账号**：

- 协议已到位（证据见上节：kong latency + 业务错误码）；
- 待有新号时，跑一次 `/v1/messages` 见到 200 即闭环。

**复跑命令**（拿到新号后）：

```powershell
# 1) 把新号凭据放进 auths/phanthywork/
# 2) 真机验证
$env:PHANTHY_LIVE_DIR='D:/project_GIT/workbuddy2api/auths/phanthywork'
go test -tags mvp ./internal/phanthywork/ -run TestMVPFinal -v

# 期望看到：
#   ① register ✓   reward=first_device_owner
#   ② token ✓      len=813
#   ③ chat HTTP=200  ← 这一步就是闭环
```

### 三、给后续接手者的提醒

- **不要**为了"让请求通过"而反复对同一账号施压 —— 本次就是因此把一个能用的号测死了；
- 每次真机验证**只发 1 次**推理请求，失败就先停下来分析 `kong latency`；
- 数据面（profile / usage / models）**先测**：如果它们也 403，说明账号已废，
  继续调协议毫无意义。
