# Loomy 本地工具链与生图能力探究报告

> 探究日期：2026-10-03
> 探究对象：本机 Loomy 桌面客户端（v0.9.37 / v0.9.38 安装包）
> 目的：搞清「Loomy 怎么生图」，并在本反代里实现同一能力

本文记录**实测得到的事实**，不写推测。凡未验证的一律标注「未验证」。

---

## 一、结论速览

| 问题 | 答案 |
|---|---|
| 生图走哪个端点？ | `POST https://loomyad.xunfei.cn/api/v1/images/generations` |
| 用哪个模型？ | `doubao-seedream-5-lite`（默认）、`qwen-image-3.0-pro` |
| 怎么鉴权？ | 账号 `session`（32 位 hex），**同时**发 `token` 与 `Authorization: Bearer` |
| 返回什么？ | `{created, data:[{url}], points_consumed}`，url 是腾讯 COS **签名直链** |
| 要 Cookie 吗？ | **不需要**。它认证的是 `session` 这个 token，不是浏览器 Cookie |
| 本项目的 loomy 上游能直接用吗？ | **能** —— 同一个上游（`loomyad.xunfei.cn`），已有 session 凭证 |
| 为什么现在不能用？ | 模型表里两个生图模型被标了 `Unavailable: true`，而那个标记是**基于 chat 端点的 404** 得出的错误结论 |

---

## 二、Loomy 客户端的目录布局

### 2.1 三个关键路径

```
程序本体   D:\software\Loomy-Setup-0.9.37\          ← Electron 应用（正在运行的是这个）
             resources\app.asar                       ← 主 bundle（220 MB）
             resources\app.asar.unpacked\electron\     ← 未打包的模块（可直接读）
用户数据   C:\Users\Public\Loomy\502d24baf4b6\      ← 全部用户态数据
工作区     C:\Users\21877\Documents\Loomy Workspace\ ← 生成的文件落在这里
```

⚠ 用户数据目录名 `502d24baf4b6` 是**账号/安装的哈希**，不是固定值 —— 换机或换账号会变。
代码里通过 `getProfileDir()` 取，不要硬编码。

### 2.2 `opencode\tools\` —— 暴露给 Agent 的工具

五个工具，**全部是薄客户端**（只做参数归一化 + 转发）：

| 文件 | 作用 | 转发目标 |
|---|---|---|
| `loomy_image.js` | 生图 / 识图 | `LOOMY_IMAGE_TOOL_BRIDGE_URL` |
| `loomy_config.js` | MCP / 远程 / 定时任务配置 | `LOOMY_CONFIG_TOOL_BRIDGE_URL` |
| `loomy_websearch.js` | 联网搜索 | bridge |
| `loomy_soul.js` | 人格/灵魂设定 | bridge |
| `loomy_glasses.js` | 眼镜端（翻译） | bridge |

**关键事实**：工具文件里**没有任何生图逻辑**，它们只读环境变量里的 bridge 地址与 token，
把 `{action, prompt, images, directory, worktree}` POST 过去。

```js
// loomy_image.js:80-106（逐字）
const BRIDGE_URL = String(process.env.LOOMY_IMAGE_TOOL_BRIDGE_URL || "").trim()
const BRIDGE_TOKEN = String(process.env.LOOMY_IMAGE_TOOL_BRIDGE_TOKEN || "").trim()
```

`action` 只有两个值：`generate` / `analyze`。

### 2.3 bridge —— 本机回环 HTTP 服务

`electron\image\image-tool-bridge.js` 实现了它：

```
POST http://127.0.0.1:<随机端口>/v1/loomy-image-tool
Authorization: Bearer <随机 UUID>
```

- 端口 `listen(0, '127.0.0.1')` —— **每次启动随机**
- token 是 `crypto.randomUUID()` —— **每次启动变化**
- 只接受 `POST` + 精确路径 `/v1/loomy-image-tool`，否则 404
- token 不匹配 → 401

它会把请求头里的 `chatid` / `msgid` / `traceparent` / `invokeorigin`
透传进 `payload.requestContext`。

`getRuntimeEnv()` 返回这两个环境变量，由 Electron 注入给 opencode 子进程。

**为什么这样设计**：工具跑在 opencode 子进程里，而真正的生图逻辑（需要读配置、
读 session）在主进程。bridge 是两者之间唯一的通道，token 防同机其它程序乱调。

### 2.4 真正的生图实现

`electron\image\image-generation-service.js`（857 行）—— 这是全部逻辑所在。

---

## 三、生图请求的完整契约（实测）

### 3.1 端点与鉴权

```http
POST https://loomyad.xunfei.cn/api/v1/images/generations
Authorization: Bearer <session>
token: <session>
Content-Type: application/json
Accept: application/json
loomy-version: 0.9.37
X-Loomy-Request-Purpose: image.generate
```

⚠ **两个鉴权头都要发**（`token` + `Authorization`）。源码注释写明了原因：

> session 模式同时塞 token + Authorization（兼容 Loomy iModel 两种入参）
> —— `image-generation-service.js:88-101`

这与本项目 `internal/loomy/client.go` 已记录的「**双轨鉴权**」现象一致：
上游不同端点认不同 header。生图端点**两个都认**（实测都发才稳）。

### 3.2 请求体

```json
{
  "model": "doubao-seedream-5-lite",
  "prompt": "……",
  "n": 1,
  "size": "2304x1728",
  "response_format": "url"
}
```

前四项是默认值（`image-generation-service.js:25-29`）：

```js
const DEFAULT_IMAGE_GENERATION_OPTIONS = {
  n: 1,
  size: '2304x1728',
  response_format: 'url',
}
```

图生图时**多一个 `images` 数组**，元素是 data URL 或远程 URL（`_generateImageToImage`）。

### 3.3 响应

```json
{
  "created": 1790998630,
  "data": [{ "url": "https://aigc-output-image-1326893053.cos.ap-guangzhou.myqcloud.com/..." }],
  "points_consumed": 110
}
```

- `data[0].url` 是**腾讯云 COS 签名直链**（带 `q-signature`/`q-sign-time`）
- `points_consumed` 是本次扣的积分（实测生图 **110 分/张**）
- 也支持 `b64_json`（`_persistImages` 两种都处理）

### 3.4 实测数据

| 项目 | 实测值 |
|---|---|
| 端点 | `https://loomyad.xunfei.cn/api/v1/images/generations` |
| 耗时 | **23.3 秒**（2304x1728） |
| 图片 | 2304x1728 PNG，306 KB ~ 4.2 MB |
| 扣分 | 110 |
| 图片下载 | **裸 URL 可直接下**（签名在 URL 里，不需要额外鉴权头） |

下载那一条值得单独说：源码里 `_persistImages` 在 session 模式下**会**带
`Authorization` 去下 URL，但实测**带与不带都能下**（签名已在 URL 里）。
第三方 provider 反而可能因多余 header 被拒 —— 所以本项目实现时选择**不带**。

---

## 四、⚠ 关键发现：`Unavailable` 标记是错的

### 4.1 现象

本项目 `internal/loomy/models.go:66-67` 把两个生图模型标成不可用：

```go
// 下面两个实测返回 404「该模型暂未开放」——保留在表里但标 Unavailable，
{ID: "doubao-seedream-5-lite", ContextWindow: 128000, MaxOutputTokens: 131072, Unavailable: true},
{ID: "qwen-image-3.0-pro", Unavailable: true},
```

### 4.2 实测反驳

```
POST /api/v1/chat/completions  {model:"doubao-seedream-5-lite", ...}
  → HTTP 404 {"message":"该模型暂未开放,请切换其他模型","type":"not_found_error"}

POST /api/v1/images/generations {model:"doubao-seedream-5-lite", ...}
  → HTTP 200 {"data":[{"url":"https://…"}],"points_consumed":110}   ← 完全可用
```

`qwen-image-3.0-pro` 同样：chat 404 / images 200。

### 4.3 根因

**一个模型能不能用，取决于端点**。当初的实测只打了 `/chat/completions`
（因为那时只做对话转发），于是「这个模型不能对话」被写成了「这个模型不可用」。

这两个模型是**专用于生图**的：它们的 `modalities.output` 只有 `image`
（见 `opencode.json`），本来就不该走 chat 端点。

### 4.4 影响

- 反代 `/v1/models` 目录里看不到它们 → 客户端不会调用
- 即使强行调用，`internal/loomy` 会因 `Unavailable` 而在某些路径跳过处理

---

## 五、模型清单（实测 `/models`，11 个）

| 模型 ID | 用途 |
|---|---|
| `deepseek-v4-flash-0731` | 对话 |
| `MiniMax-M3` | 对话 |
| `Kimi-k2.6` | 对话 |
| `qwen-3.8-max` | 对话 |
| `GLM-5.3-Flash` | 对话 |
| `qwen3.8-flash` | 对话 |
| `spark-x` | 对话（讯飞星火） |
| `mimo-v2.5` | 对话 |
| **`doubao-seedream-5-lite`** | **生图**（豆包 Seedream 5 Lite） |
| **`qwen-image-3.0-pro`** | **生图**（通义万相） |
| **`Hy-Image-3.5-preview`** | **生图**（实测通过，见第十节） |

---

## 六、凭证

### 6.1 session 从哪来

`userData\auth-session.json`：

```json
{ "session": "<32位hex>", "userid": "...", "phone": "...", "updatedAt": 1790997249597 }
```

真机实测有值，可直接用来鉴权（本文档中的探测均用它）。

### 6.2 与本项目的关系

本项目 `internal/loomy` 的凭证格式（`auths/loomy/loomy-<uid>.json`）里
`session` 字段就是同一个东西 —— **两边是同一个上游的同一个凭证**。
换句话说：**给反代配了 loomy 账号，就等于配了生图能力**，不需要额外的 Cookie。

### 6.3 ⚠ 不是 Cookie

用户提到「调用账号的一些 Cookie」。实测结论：**生图不需要浏览器 Cookie**。

- 客户端**不读**浏览器 Cookie
- 鉴权靠 `Authorization`/`token` 头里的 `session` 值
- 该值存在普通 JSON 文件里，不是加密的 cookie store

所以本实现用「session token」而不是「cookie」—— 这更简单且更稳
（不受浏览器登录态、SameSite、无痕模式影响）。

---

## 七、其它工具（简略）

| 工具 | 事实 |
|---|---|
| `loomy_websearch.js` | 走 bridge，`action` 与搜索参数由主进程处理；未深入探究 |
| `loomy_config.js` | MCP/远程/定时任务管理；有 `needsInput` / `requireConfirmation` 确认协议 |
| `loomy_soul.js` / `loomy_glasses.js` | 人格设定 / 眼镜翻译，与生图无关 |

其中 `loomy_config.js` 的**确认协议**值得一提（破坏性操作要先确认再带 `confirmed:true` 重调），
本项目若将来做写操作的管理端点可借鉴这个形态。

---

## 八、对本项目的实现建议

1. **不要重写一个 Loomy 客户端** —— 复用 `internal/loomy`（同一个上游、同一份 session）
2. **新增一个扩展点** `gateway.ImageGenExt`，与既有 `QuotaExt`/`CheckinExt` 同一范式：
   核心不解释生图协议，只负责把请求交给上游
3. **模型表要区分「对话可用」与「生图可用」** —— 现在的 `Unavailable` 语义混淆了两者
4. **图片落盘**：上游给的是**会过期的签名 URL**（`q-sign-time=1790998622;1791041832`，
   约 12 小时），必须及时下载并转存 —— 直接把 URL 交给客户端，几小时后就是死链
5. **超时**：实测 23 秒，客户端超时至少要 3 分钟（Loomy 自己用 5 分钟）
6. **接口形状**：按 OpenAI Images API 暴露（`/v1/images/generations`），
   这样现有的 OpenAI 客户端（含各种生图工具）可直接用

---

## 九、未验证 / 存疑

- ~~`Hy-Image-3.5-preview` 是否支持 `/images/generations`~~ —— **已实测通过**
  （2026-10-03，返回 200 + PNG，扣 110 积分），已进生图清单。见第十节。
- `size` 的合法取值集合 —— 只测了 `2304x1728` 与 `1024x1024`
- **图生图**（带 `images` 参数）—— **未测**（本文只实测了文生图）
- 签名 URL 的**确切**有效期 —— 仅从 `q-sign-time` 推断约 12 小时，未实测过期行为

## 十、实现阶段补测的事实（2026-10-03 追加）

### 10.1 `response_format=b64_json` 被**忽略**

```
请求 {"model":"doubao-seedream-5-lite","response_format":"b64_json"}
响应 {"data":[{"url":"https://…"}]}     ← 仍是 url（实测两次）
```

即上游只支持 `url`。想要 base64 得自己 GET 那个 url。
（实现里**没有**替它补齐 —— 见 `docs/image-generation.md` 3.2 的理由。）

### 10.2 `n > 1` 只返回 **1 张**，且只扣 1 次费

```
请求 {"n":2}  →  data 条数 = 1，points_consumed = 110（不是 220）
```

上游忽略 `n`。**静默少给**：调用方不会收到错误，只会拿到一张图。
这是上游的固有行为，网关如实透传、不纠正（纠正意味着网关自己猜
"用户其实想要两张"，并自行发两次请求 —— 那会在计费上做出未经同意的决定）。

### 10.3 `Hy-Image-3.5-preview` 走生图端点是通的

```
POST /images/generations  {"model":"Hy-Image-3.5-preview", …}
→ 200，data[0].url 是 PNG，points_consumed = 110
```

**关键教训**：它的模型名里含 "Image"，但从名字推断是错的两种方向都有 ——
当初因为"名字含 Image 但没实测"就排除它，现在实测证明它可用。
反过来也应警惕：不含 Image 的名字未必不能生图。
清单只收**实测通过**的成员。

### 10.4 对话模型**不能**生图（这是"工具注入"方案的前提）

```
qwen3.8-flash + "帮我画一只橘猫"（不带工具）
→ HTTP 200，content 里是一段 **SVG 源码**，不是图片
```

原因在模型声明的 `modalities.output`：

| 类别 | 模型 | `output` | `tool_call` |
|---|---|---|---|
| 对话 | deepseek-v4-flash / MiniMax-M3 / Kimi-k2.6 / qwen-3.8-max / GLM-5.3-Flash / qwen3.8-flash / spark-x / mimo-v2.5 | `["text"]` | `true` |
| 生图 | doubao-seedream-5-lite / qwen-image-3.0-pro / Hy-Image-3.5-preview | `["image"]` | `false` |

**两个轴完全对齐**：会说话的都不会画，会画的都不会说话。
所以"让对话模型生成图片"在协议层就不可能 —— 只能由**外部**
（原先是 Loomy 客户端，现在是本网关）替它去调生图模型。

### 10.5 上游**支持工具调用**，且能跑通完整闭环

这是"客户端零改动"方案可行性的根据，三条都实测过：

**① 给它工具定义，它自己会调**

```
qwen3.8-flash + tools:[generate_image] + "帮我生成一张图：一只橘猫"
→ HTTP 200，finish_reason = "tool_calls"
  tool_calls[0].function.name = "generate_image"
  arguments = {"prompt":"一只可爱的橘猫，圆圆的眼睛，毛色橙黄带条纹，坐在阳光下的窗台上…"}
```

**② 把工具结果喂回去，它能收尾**

```
messages += [assistant(tool_calls), tool(tool_call_id, content=结果 JSON)]
→ HTTP 200，finish_reason = "stop"
  content = "图片已经生成好了：一只橘猫悠闲地坐在窗台上 🐱 …"
```

**③ 上游接受 `stream: false`**

工具循环必须拿完整响应才能判断"要不要执行工具"，
所以非流式是前提。实测 `{"stream":false}` 正常返回（`finish_reason=stop`）。

### 10.6 联网搜索：**同一个上游、同一个 session**

```
POST {base}/search/tencent   {"query":"…","Mode":0}
→ 200 {"Pages":[…],"Query":"…","RequestId":"…","Version":"…","points_consumed":25}
```

⚠ **`Mode` 必须是 int（0/1/2），传字符串会 400**：

```
{"error":{"code":"10011","message":"请求体解析失败: json: cannot unmarshal
 string into Go struct field TencentSearchRequest.mode of type int64"}}
```

Loomy 客户端的 `normalizeMode` 有 `natural/vr/mixed → 0/1/2` 的映射，
但**那个映射在客户端侧，不在线上协议里** —— 照抄客户端的入参形态
会写出一个恒定 400 的调用。这是"读代码而不打上游"的典型代价。

`Pages[]` 条目的**实测**字段（不是猜的）：

```
authority_level, content, date, favicon, passage, pics, score, site, title, url
```

站点名是 **`site`**（不是 `site_name`）。猜错不会报错 —— 只会让
每个条目的站点/标题悄悄变空，喂给模型的物料质量下降而无人察觉。

成功判据照抄客户端：`code` 字段**存在且 ≠ "000000"** 才算业务错误。
⚠ 实测成功响应里**根本没有 `code` 字段** —— 写成"code 必须等于 000000
才算成功"会让每一次成功调用都被判失败。

### 10.7 五个内置工具的**后端归属**（决定哪些能搬进网关）

| 工具 | 后端 | 能否搬 |
|---|---|---|
| `loomy_image` | `{base}/images/generations` | ✅ 同上游同 session |
| `loomy_websearch` | `{base}/search/tencent` | ✅ 同上游同 session |
| `loomy_config` | 本地 MCP/定时任务配置（写本地文件、spawn 子进程） | ❌ 那是 Loomy 客户端自己的配置，网关里没有对应物 |
| `loomy_soul` | 本地 `loomy-souls.json` 人设文件 | ❌ 同上 |
| `loomy_glasses` | 眼镜端翻译（需 `GLASSES_MODE_ENABLED`） | ❌ 硬件绑定 |

前两个只是"往同一个上游发不同 body"，所以能搬；后三个的语义依赖
Loomy 客户端本地的状态与硬件，**搬过来就是空壳**。

