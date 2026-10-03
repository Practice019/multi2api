# 反代生图能力实现说明

> 实现日期：2026-10-03
> 关联探究报告：`docs/loomy-local-toolchain.md`
> 端点：`POST /v1/images/generations`

---

## 一、一句话概括

网关多了一条**生图出口**，把请求转发给 Loomy 上游的 `/images/generations`，
用账号池里的 loomy 账号 `session` 鉴权，调用**豆包 Seedream** 或**通义万相**出图。

实测跑通：`18.4 秒`出一张 `1024x1024` PNG，扣 `110` 积分。

---

## 二、为什么之前不行（根因）

不是"没有实现"，而是**两个各占一半的缺口**：

### 缺口 1：出口层只有 chat 一条路

网关此前只有 `/v1/chat/completions`。而 Loomy 上有两个模型是
**只能生图、不能对话**的：

```
POST /chat/completions   {"model":"doubao-seedream-5-lite"}  → 404 该模型暂未开放
POST /images/generations {"model":"doubao-seedream-5-lite"}  → 200 + 图片
```

两条都是实测。所以"能不能用"取决于**端点**，单靠 chat 出口表达不了生图。

### 缺口 2：模型表把"不能对话"记成了"不可用"

`internal/loomy/models.go` 里这两个模型标着 `Unavailable: true`，
而那个标记的判据只是"chat 端点 404"。于是它们既不进 `/v1/models`，
也不会有人来调生图端点 —— **端点就算实现了也没人知道能用**。

⚠ 这是本项目一个反复出现的形态：**用一个 bool 表达了两个轴**。
修正后语义写清楚了：

| 轴 | 含义 | 载体 |
|---|---|---|
| `Unavailable` | 能不能走 `/chat/completions` | `models.go` 的 `knownModels` |
| `imageModelIDs` | 能不能走 `/images/generations` | `imagegen_ext.go` |

---

## 三、实现结构

```
POST /v1/images/generations
  └─ server.imagesGenerations          internal/server/images.go
       ├─ 入口校验（prompt / JSON / 上游前缀）
       ├─ 选号  Pool.PickFor(provider, model, nil)
       └─ 转发  装配层 ImageGen 适配器      cmd/server/multiprovider.go
            └─ gateway.ImageGenExt        internal/gateway/imagegen_ext.go
                 └─ loomy.Provider.GenerateImage   internal/loomy/imagegen_ext.go
                      └─ loomy.Client.GenerateImage internal/loomy/client.go
                           └─ POST <base>/images/generations
```

### 3.1 两个新扩展点（与既有范式一致）

`gateway.ImageGenExt` / `gateway.ImageModelExt`：

```go
type ImageGenExt interface {
    GenerateImage(ctx context.Context, uid string, body []byte) ([]byte, int, error)
}
type ImageModelExt interface {
    ImageModels() []ImageModel
}
```

**为什么是两个接口而不是一个**：消费者不同 —— 前者只在生图端点的处理路径上，
后者只在模型目录的构建路径上。合起来会让"只想在目录里露个名字"的上游
被迫实现发请求的方法。

**为什么 `GenerateImage` 收 `uid` 而不是 `Credential`**：
生图要的是 `session`，而核心的 `Credential` 投影里**没有** session
（只有 uid/nickname）。所以给主键才是够的信息 —— 让出口层去拼凭证反而做不到。
上游自己按 uid 从它的凭证目录取（`p.authByUID`）。

### 3.2 响应**原样透传**

不转存图片、不重排字段、不改名。理由：

- 上游返回的 `data[0].url` 是腾讯 COS 的**签名直链**，调用方拿到即下载
  （所有生图客户端都是这个行为）。**不需要转存**，12 小时有效期远够。
- `points_consumed` 是上游的非标准字段，但调用方很需要 ——
  重排字段就会丢掉它，而"上游加了新字段"就变成"要改网关"。

⚠ **透传的代价也是真的**：上游忽略 `response_format=b64_json`（实测恒回 `url`，
见第七节），而网关不会替它补齐 —— 那需要网关自己下载并 base64 编码。
**刻意不做**：那是"网关替上游造一个它没承诺的行为"，会让
"上游哪天真的支持了"与"网关一直在模拟"无法区分。

### 3.3 鉴权：两个头都发

`client.go` 已记录的"双轨鉴权"判据是"一个端点认一个头"
（`/models` 认 `token`、`/chat/completions` 认 `Authorization`）。

生图端点实测**两个都认**，且 Loomy 官方客户端也是两个都发：

```js
// Loomy 自己的 image-generation-service.js
// "session 模式同时塞 token + Authorization（兼容 Loomy iModel 两种入参）"
```

本实现跟随这个**唯一有实测背书**的形态。测试把它钉住了
（`TestGenerateImageSendsBothAuthHeaders`）—— 免得后人"统一成 chat 那样只发一个"。

---

## 四、几个刻意的设计决定

### 4.1 生图失败**不换号重试**

chat 出口失败会换号重试。生图**不**：

一次生图 20+ 秒，且**上游已计费**（实测 110 分/张）。悄悄换号重试意味着
用户被扣两次分却可能只拿到一张图（或什么都没有）—— 那比直接失败糟得多。

### 4.2 "没号"报 503，"不支持"报 501

两者对调用方的含义完全相反：

```
503 no_healthy_account   没有可用账号 → 等一会儿再来（它会自己好）
501 image_not_supported  上游不支持   → 永远别再来（重试也不会好）
```

把"暂时没号"报成"不支持"会让调用方**永久放弃一个完全正常的功能**。
有测试分别钉住这两种。

### 4.3 上游的业务错误**不冷却账号**

chat 出口有熔断语义（`applyErrorPolicy`）。生图**不做**：

"提示词被拒"或"模型不支持"不是"这个号坏了"。照抄一套不适用的策略
会让一个健康账号因为一次参数写错而被冷却 —— 表现为"随机性的账号故障"。

只有**传输层**失败（网络/取不到凭证）才 `NoteError`。

### 4.4 生图模型带 `capabilities` 标记进目录

```json
{"id":"loomy/doubao-seedream-5-lite","capabilities":["image_generation"], …}
```

不靠名字猜（"seedream" 看起来像生图是脆的 —— 名字是上游可改的展示字符串，
而这是**能力声明**）。调用方据此知道该走哪个端点。

---

## 五、验证

### 5.1 真实端到端（走网关，非直连）

```bash
POST http://127.0.0.1:7895/v1/images/generations
Authorization: Bearer <api_key>
{"model":"loomy/doubao-seedream-5-lite","prompt":"极简插画：…","n":1,
 "size":"1024x1024","response_format":"url"}
```

结果：

```
耗时        18.4 秒
created     1790999994
points_consumed 110
data[0].url https://aigc-output-image-….cos.ap-guangzhou.myqcloud.com/…
```

下载该 URL：`210.3 KB`、PNG 头 `89 50 4e 47`、`1024x1024` —— 图片真实有效
（人眼确认：深蓝夜空 + 暖黄路灯 + 扁平风格 + 大量留白，与提示词一致）。

### 5.2 目录

```
GET /v1/models
  id=loomy/doubao-seedream-5-lite  caps=image_generation
  id=loomy/qwen-image-3.0-pro      caps=image_generation
```

### 5.3 自动化测试

| 文件 | 覆盖 |
|---|---|
| `internal/loomy/imagegen_ext_test.go` | 双头鉴权 / 错误透传 / 缺凭证报错 / 用对账号 / 两个轴的清单不混 |
| `internal/server/images_test.go` | 路由已注册 / 入口校验 5 例 / 503 vs 501 / 目录含生图模型与标记 / 未实现时不加条目 |

**变异验证**（均已还原）：

| 变异 | 结果 |
|---|---|
| 去掉 `token` 头（只发 Authorization） | `TestGenerateImageSendsBothAuthHeaders` 红 |
| 忽略 uid、改用目录里第一个账号 | `TestGenerateImageUsesTheRequestedAccount` 红 |

### 5.4 CI 五件套

`go build` / `go vet` / `go mod tidy` / `go test ./...` / `gofmt` 全绿（29 包）。

---

## 六、怎么用

```bash
curl -X POST http://127.0.0.1:7863/v1/images/generations \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"loomy/doubao-seedream-5-lite",
       "prompt":"一只在雨里打伞的橘猫，水彩风格",
       "size":"1024x1024"}'
```

- `model` 必须带 `loomy/` 前缀（多上游部署下前缀是路由记号）
- `size` 常用：`1024x1024` / `2304x1728`
- 返回 `data[0].url`，**立刻下载**（签名链有效期约 12 小时）
- 每次扣 110 积分（Loomy 的计费口径）

---

## 七、已知限制（如实列出，全部实测过）

### 7.1 上游**忽略** `response_format=b64_json`

实测两次都是：

```
请求 {"model":"doubao-seedream-5-lite","response_format":"b64_json"}
响应 {"data":[{"url":"https://…"}]}      ← 仍然是 url，没有 b64_json 字段
```

本实现**原样透传**这个行为，不替上游补齐。
若调用方依赖 `b64_json`，需要自己 GET 那个 url 再编码 ——
网关不假装支持一个上游没承诺的能力。

### 7.2 `n > 1` **只返回 1 张**，也只扣 1 次费

```
请求 {"n":2}  →  data 条数 = 1，points_consumed = 110
```

即上游**忽略 `n`**（不是漏了，是根本没用这个参数）。想要多张就发多次请求。
⚠ 若调用方以为 `n=2` 会拿到两张，它会拿到一张且不会报错 ——
**静默少给**是这个上游的固有行为，网关不纠正。

### 7.3 其它

- **只接了 loomy**。其它上游没有生图端点（已确认），要接需各自实现
  `ImageGenExt`。
- **`Hy-Image-3.5-preview` 未验证** —— 名字像生图模型，但没实测过它能否走
  `/images/generations`，因此**没有**放进生图清单。宁缺勿滥：
  目录里放一个调不通的模型比不列它更糟。
- **图生图未实测**。上游支持 `images` 参数（Loomy 客户端用它做参考图改图），
  但本次只实测了文生图。本实现是透传，带上 `images` 应当能工作，
  但**没有证据**，故不写进能力声明。
- **签名 URL 的确切有效期未实测** —— 仅从 `q-sign-time` 推断约 12 小时。
- **没有 WebUI 入口** —— 目前只能通过 HTTP API 调用（见第八节）。

---

## 七之补：图片"无法预览"的两个根因（2026-10-03 追加）

用户反馈「图片无法预览 · 电商产品主图宣传海报，正方形1:1构图…」，
一度被判断成"链接通道不稳定 / 链接偶尔损坏"。实测后确认**不是随机故障**，
是两个确定性的 bug，各自 100% 可复现。

### 7.4 根因一：签名 URL 的 HEAD 请求**必然 403**

上游返回的是腾讯 COS **签名** URL。实测（10/10 复现）：

| 请求方法 | 带签名 URL | 裸 URL（去掉 `?…`） |
|---|---|---|
| GET | 200 ✓ | 200 ✓ |
| **HEAD** | **403 ✗（10/10）** | 200 ✓ |

**COS 的签名把 HTTP method 也算进签名**，而这个 URL 是上游按 GET 签的。
COS 的判定顺序：

```
query 里带 q-signature → 校验它 → 不匹配就 403（不再看 bucket 权限）
query 里没有签名       → 走 bucket ACL → 本 bucket 公开读 → 放行
```

而**预览器/取图器通常先发 HEAD** 探测 `Content-Type` 与大小 ——
一探测就 403，于是显示"无法预览"。走 GET 的客户端一切正常，
**这就是为什么它看起来像"偶尔损坏"**：取决于那个客户端先发哪种请求。

另一个实测：签名**过期后**（12 小时窗口）带签名的 URL 403，而裸 URL 仍 200。

**修法**（`internal/loomy/imagedisplay.go`）：探测裸形式是否可用，
可用就用它（HEAD/GET 都通、不过期、markdown 里也没有 `&` 与 `;`）；
不可用则**退回签名 URL**（它至少 GET 可用）。

⚠ 不硬编码"bucket 公开读"这个假设 —— 那是**上游的部署事实**，
不是我们能保证的。上游哪天改成私有，探测会失败并自动退回签名形式。
两个 URL 都交给调用方（`url` = 展示用，`url_signed` = 兜底）。

### 7.5 根因二：提示词的 `]` 会**截断** markdown 链接

```go
Markdown: fmt.Sprintf("![%s](%s)", markdownAltText(prompt), url)
```

提示词是**用户输入**，而 CommonMark 规定 alt 文本在**第一个未转义的 `]`**
处结束：

```
提示词 "用 [主推款] 标签"  →  ![用 [主推款] 标签](https://…)
                                    ↑ 第一个 ] 在此 → 解析到此为止
```

后果：图不显示，URL 变成不可点的纯文本。**用户看到的那段
"图片无法预览 · 电商产品主图宣传海报，正方形1:1构图…"就是截断后的 alt**。

这是本仓的 bug（`alt` 用了未转义的用户输入）。
修法：`escapeMarkdownText` 转义 `\ [ ] ( ) < > * _ ` ~ | #`。

两个顺序细节（都有测试钉住）：
- **反斜杠要先转**，否则插入的反斜杠会被自己再转一遍（`]` → `\]` → `\\]`）
- **先截断再转义**，否则可能把 `\x` 从中间切断，露出悬空反斜杠

### 7.6 教训

"链接偶尔损坏"这类描述容易把人引向"通道/网络不稳定"，
于是去重试、去换通道 —— 而真正的原因是两个**确定性**的缺陷。
**先把"偶发"证伪成"必现"**（同一 URL 连测 10 次、分别用 GET 与 HEAD），
根因就直接浮出来了。

## 八、后续可做（未做）

1. **WebUI 里加一个生图面板**（提示词输入 + 结果预览）。
   目前只有 API，管理台里没有入口。
2. **验证 `Hy-Image-3.5-preview`**，确认后加进清单。
3. **图生图实测**（带 `images` 参数），确认后写进文档。
4. 若将来别的上游有生图能力，只需实现 `ImageGenExt` + `ImageModelExt`，
   核心与出口层**零改动**（这正是扩展点分层的价值）。
