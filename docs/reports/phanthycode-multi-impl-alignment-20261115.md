# PhanthyCode 多实现离线对齐报告

> 日期：2026-11-15
> 方法：**9 个来源 → 按血统合并 → 共识矩阵 → 只把"≥3 个独立源头一致"的当结论**
> 原则：**重复度高不等于对**（同一个源头被抄 4 次，共识也是假的）。所以先查血统。
> 全部离线，不改代码、不发请求。

---

## 0. 来源清单

| # | 代号 | 来源 | 类型 | 血统 |
|---|---|---|---|---|
| 1 | **ours** | 本项目 `workbuddy2api` | Go | — |
| 2 | **lwj** | `lwjlwjlwjlwj/phanthycode2api` | Go | **原始**（2026-08-15，作者 闲聊花花） |
| 3 | **gotsome** | `gotsomethings/phanthywork2api` | Go | **fork of lwj**（自述「基于 lwj 改进」） |
| 4 | **karina** | `karinaf1iiios-byte/phanthywork2api` | Go | **独立**（2026-10-30，AGPL，自述「PhanthyWork 桌面端 macOS 2.43.0 / 内核 2.1.92 协议逆向」） |
| 5 | **mycf** | `MYCF711/magpie-PhanthyCode` | .mjs | **原始**（2026-11-09，作者 baijiu666） |
| 6 | **baijiu** | `baijiu666/magpie-PhanthyCode` | .mjs | **mycf 的后续维护**（0.2.x，2026-11-14） |
| 7 | **pcl** | `phanthy-code-launcher`（npm） | JS | 第三方 launcher（`spalxyz13`） |
| 8 | **kernel** | `@phanthy/phanthy-codeo@0.2.13`（npm） | JS | **官方终端 CLI**（源码可读） |
| 9 | **sidecar** | 本机 `claude-sidecar.exe`（`PhanthyWork.exe` **2.51.0**） | 二进制 dump | **官方 Windows 桥** |

⚠ **合并后的独立实现只有 6 个**：ours / lwj+gotsome / karina / magpie(mycf+baijiu) / pcl / kernel(+sidecar 同源)。

---

## 1. 共识矩阵

`★N` = 该来源里出现 N 次；`.` = 没有。
（"出现"≠"发送" —— 有些是注释/负断言，看到 ★ 还需要看代码位置。）

```
事实                        ours  gotsome karina   lwj   mycf baijiu    pcl kernel sidecar
ep authorize                ★21     ★2     ★3    ★1    ★3   ★26     ★2   ★11    ★10
ep token                    ★36     ★3    ★12    ★3   ★11   ★11     ★1   ★12    ★10
ep code/success             ★7      ★5     ★7    ★4     .      .     ★1    ★1     ★9
ep profile                  ★24     ★2    ★17    ★2   ★10   ★11     ★1    ★1     ★4
ep usage                    ★17      .    ★17     .   ★27   ★47     ★1    ★1     ★5
ep models                   ★30      .     ★1     .   ★13   ★21     ★1    ★2     ★4
ep summary                  ★20     ★1    ★10     .    ★8    ★8      .     .     ★3
ep claim                    ★8      ★3     ★9     .    ★2    ★3      .     .     ★1
ep rewards(work)            ★6       .     ★4     .   ★11   ★17     ★1     .     ★2
ep register                 ★22      .    ★14     .    ★6    ★9      .     .     ★1
ep create_api_key            .      ★3     ★1    ★3     .     .      .     .      .
ep event_logging            ★16      .      .     .     .     .     ★1    ★1     ★1
ep v1/messages             ★164     ★5    ★51    ★3   ★40   ★45      .   ★16   ★129
ep v1/chat/completions     ★261    ★13     ★9    ★6    ★8    ★8      .     .     ★5
ep v1/responses             ★31     ★1     ★1     .    ★7    ★7      .    ★1     ★4
hdr cli-device              ★24      .    ★10     .     .   ★12     ★1    ★1     ★1
hdr client-source           ★50     ★4    ★10     .   ★18   ★26      .     .     ★4
hdr product                 ★10      .      .     .     .     .      .     .      .
hdr project                 ★20      .      .     .     .     .     ★1    ★1      .
hdr session                 ★27      .     ★9     .   ★10   ★12     ★1    ★1     ★1
hdr request                 ★12      .      .     .     .     .     ★1    ★3     ★1
hdr client                  ★79     ★5    ★15     .   ★18   ★36     ★1    ★1     ★5
hdr proof                   ★58      .     ★7     .   ★29   ★31      .     .     ★6
hdr desktop-session         ★33      .     ★6     .   ★13   ★14      .     .     ★2
hdr x-desktop-              ★71      .    ★27     .   ★56   ★79      .     .     ★4
hdr anthropic-beta           ★6    ★13    ★14    ★3     .     .      .   ★38    ★51
hdr anthropic-version       ★24     ★9     ★3     .   ★11   ★11      .    ★3    ★19
hdr x-app                   ★10     ★1     ★6    ★1     .   ★12      .    ★2    ★12
hdr claude-session-id       ★33      .    ★10     .   ★11   ★14      .     .     ★3
sign ed25519                ★75      .    ★38     .   ★40   ★41      .    ★1    ★10
sign canonical             ★377      .    ★29     .  ★143  ★159      .  ★138    ★51
id phanthy-code-cli         ★43     ★3    ★15    ★3    ★7   ★36     ★1    ★1     ★3
grant refresh_token        ★826    ★10    ★25    ★8   ★74   ★86     ★5  ★105   ★104
```

---

## 2. 全票共识（9/9 或 8/9）—— 这些可以直接当事实

| 事实 | 证据 |
|---|---|
| **`client_id = phanthy-code-cli`** | 9/9 |
| **`POST /oauth/token` 换票 + `grant_type=refresh_token` 续期** | 9/9 |
| **`GET /oauth/authorize` 授权码 + PKCE S256** | 9/9 |
| **`/api/oauth/profile` 读账号** | 8/9 |
| **`/v1/messages` 是上游推理路径** | 8/9（`pcl` 是纯转发，不自己拼路径） |
| `GET /api/phanthy_code/models` 模型目录 | 6/9 |

---

## 3. ★★ 最重要的发现：**我们的两个 UA 都是错的**

### 3.1 证据链（三个独立来源互证）

**① 本机 sidecar（官方 2.51.0）源码里读到的真值**

```js
var al = "2.1.92"                                  // 推理面 UA 的版本段，**字面量**
GK9 = process.env.APP_VERSION || "2.1.0"           // 数据面 UA 的版本段，运行期取值
// 还发现一个我们从没发过的头：
// x-anthropic-billing-header: cc_version=2.1.92.${fingerprint}; cc_entrypoint=${K}; cch=00000;
```

**② 本机 `PhanthyWork.exe` FileVersion = `2.51.0`** ⇒ `APP_VERSION = 2.51.0`

**③ magpie `src/http.mjs`（2026-11-12，桌面端 2.51.0 静态反编译）**

```js
export const OFFICIAL_DATA_UA      = "phanthy-code-cli/2.51.0"
export const OFFICIAL_INFERENCE_UA = "claude-cli/2.1.92 (external, cli)"
```

并注明：**「2.51.0 起两条路径分别用上面两个 UA；混用会被网关按『非当前官方客户端』处理
（旧版 `phanthy-code-cli/2.1.0` 曾命中 `403 TM.00000001`）」**

### 3.2 我们的差距

| 位置 | 我们用的是 | **应该是** | 判定 |
|---|---|---|---|
| `/api/*` 数据面 UA | `phanthy-code-cli/2.1.0`（`client.go:216`） | **`phanthy-code-cli/2.51.0`** | ❌ **旧版本号，社区实测会 403** |
| 推理面 UA | `Bun/1.3.14`（`DefaultUA`） | **`claude-cli/2.1.92 (external, cli)`** | ❌ 2.48.0 时代的值 |
| `anthropic-version` | **不发** | `2023-06-01` | ❌ 缺 |
| `anthropic-beta` | **不发** | `oauth-2025-04-20`（或 `claude-code-*`） | ❌ 缺 |
| `x-anthropic-billing-header` | **不发** | `cc_version=2.1.92.<fp>; cc_entrypoint=<ep>; cch=00000;` | ❌ 缺 |

**根因**：我们一直在逆向 **2.48.0 时代**的行为（`Bun/1.3.14` 正是那个时代注册请求 200 的实测值），
而当前桌面端已经是 **2.51.0**，UA 换了。

### 3.3 还有一个方向性差异

| | 官方内核（CLI） | 官方桥（Windows） | 我们 |
|---|---|---|---|
| `x-phanthy-client-source` | **`cli`** | `desktop` | `desktop` |

我们的推理走 `/v1/chat/completions` + `desktop`；官方内核走 `/v1/messages` + `cli`。

---

## 4. 共识矩阵的另一半：**签名到底要不要？**

合并血统后是 **3 : 3**：

| 阵营 | 独立实现 | 它们的取源 |
|---|---|---|
| **签名** | `karina`（自述反编译 macOS 桌面端 2.43.0/内核 2.1.92）、`magpie`、**我们** | 从**桌面端**逆向 |
| **不签名** | `lwj`(+gotsome)、`pcl`、**官方内核 `phanthy-codeo`** | 从 **CLI** 角度 |

**⇒ 两种都"能工作"。** 这本身就是结论：**服务端对两种形态都接受**（至少曾经）。
所以"要不要签名"不是一个有唯一答案的问题 —— 它是一个**取决于你走哪条路**的问题：

- 走 **桌面端形态** → 必须设备注册 + 推理令牌 + 4 个 proof 头（否则缺 desktop session）
- 走 **CLI 形态** → 只要 Bearer + `anthropic-beta`，一个签名都不要

**magpie 的选择**：它宣传"不需要桌面端"，但实际**实现了完整签名链**（`src/proof.mjs`、
`src/registration.mjs`、三把 ed25519 密钥）。也就是说它选了桌面形态。

### 4.1 magpie 的真机实测（2026-11-09）—— 与我们的实现逐条一致

> 1. `device_public_key` **必须**与 `public_key` 不同 —— 官方用**三把独立 ed25519 密钥**；
> 2. `body.device_signature` 用 **device 私钥**签 device canonical；
> 3. 请求头 `x-desktop-signature` 用 **identity 私钥**签 v1 canonical。
>
> 配套：`inference_public_key` 是 **inference（sidecar 会话）**公钥，**每次注册换新**；
> `/v1/messages` 的 12 段推理证明用 **inference 私钥**签。

**⇒ 与我们 `IDENTITY_POLICY` / `DeviceCanonical` / `InferenceCanonical` 完全对应。**
这是一份独立实现对我们签名链的**验证**。

它还把 `].join(<LF>)` 的坑记下来了：

> 「`].join(\`…\`)` 后紧跟 `0x0A`。渲染工具会把 `0x0A` 显示成『点』，只看渲染会写错（**本项目一度也读错**）。」

**⇒ 和我在 2026-11-14 踩的是同一个坑、同一句教训。**

---

## 5. 封禁问题：**三个独立实现都没解决**

| 实现 | 原话 |
|---|---|
| `baijiu`（magpie） | 「使用第三方客户端 / 反代会触发服务端风控，账号可能被禁用或封禁，**风控问题目前尚未解决**。插件无法解除封禁，也无法保证账号安全。请勿在重要账号上使用」 |
| `gotsome`（phanthywork2api） | 「签到与多账号轮转本身存在封号风险，**加固只能降低概率**。建议主力账号只跑 chat」 |
| `karina` | 「仅用于协议学习与个人自用研究……不要用于……任何**绕过服务端风控**的用途」 |

他们的"加固"全是**池子层面的止损**，不是解决：

- 一号一 IP 代理（减少关联）
- 命中风控关键词 → 立刻禁用 + **不换号重试**（避免扩散）
- 指数退避冷却、并发/日限额、请求抖动

**⇒ 与我们的结论一致：请求形状能做到 1:1，但封禁在账号层，社区同样未解。**

---

## 6. 可直接吸收的东西（按价值排序）

### 6.1 ★★★ 错误码表（magpie `ERROR-CODES.md`，v1.2 冻结）

这是目前最完整的上游错误分类，含**真机实测**过的几个：

| code | status | 含义 | 出处 |
|---|---|---|---|
| `account_disabled` | 403 | **账号被服务端禁用/冻结**，不是登录过期 —— **重新登录不会恢复** | `disabled_account` / `account_frozen` / 网页「此账户已被禁用」 |
| `device_binding_conflict` | 409 | **账号级设备绑定**：installation id 已绑定官方桌面端公钥，**服务端不接受第二台设备**（终态，熔断 24h） | **2026-11-08 真实 409 实测** |
| `audit_blocked` | 400 | 内容被上游风控拦「暂时无法回答此问题」 | — |
| `concurrency_conflict` | 409 | 同账号并发撞车（资源已存在） | — |
| `quota_exhausted` | 402 | `insufficient_credits` | — |
| `plan_required` | 403 | `missing_or_expired_plan` / `no active plan` | — |
| `upstream_timeout` / `upstream_unavailable` | 504 / 503 | 超时 / 5xx | — |

**⇒ 两条对我们直接有用**：

1. **`account_disabled` 的判据是"重新登录不会恢复"** —— 正好对应我们遇到的
   `disabled_account`，确认它不是"会话过期"。我们 5 个号里有 2 个是这种（真封）。
2. **`device_binding_conflict` 是终态** —— 我们 `registrationCooldown` 的 24 小时熔断
   与它同值，方向是对的。

### 6.2 ★★ `/api/oauth/usage` 的口径

magpie `scripts/check-spend.mjs` 第一行：

```
// 权威口径：/api/oauth/usage 的 current_plan.used_credits
```

**⇒ 它是一个累计值字段，不是我们仓库担心的"四窗口重复计权"。**
（我们 `quota.go:20` 的否决理由基于"四窗口"，而 magpie 用的是 `current_plan.used_credits`，
两者不是同一个字段。值得重新评估。）

### 6.3 ★ 验证脚本的设计（值得抄）

它们的 `scripts/verify-protocol.mjs`：

> - 调用上限写死在脚本里：换票 1 次、模型目录 1 次、注册 ≤2 次、activities ≤1 次，超了直接退出；
> - **不访问任何推理接口**；
> - token 只在内存里，报告只写长度、是否存在、错误码。

**⇒ 这正是我们说「缺出站审计」时该做的东西**，而且他们把它做成了硬上限。

### 6.4 `create_api_key` 端点（lwj/gotsome）

```
POST /api/oauth/phanthy_cli/create_api_key      Authorization: Bearer <access_token>
→ prod 固定 404（gotsome 注释：官方 CLI 亦如此），回落到 access_token
```

**⇒ 这个端点存在（会在 404 前先要鉴权），但生产不可用。** 记录一下，不必实现。

---

## 7. 结论与建议

### 7.1 已确认的事实（≥3 独立源头）

1. `client_id = phanthy-code-cli`，OAuth 授权码 + PKCE，`refresh_token` 续期 —— 全票一致
2. 推理上游路径是 **`/v1/messages`**（Anthropic wire）
3. **官方 2.51.0 的两个 UA**：`phanthy-code-cli/2.51.0`（数据面）+ `claude-cli/2.1.92 (external, cli)`（推理面）
4. 签名链**真实存在**（三把 ed25519 + 设备注册 + inference token + 4 proof 头），
   magpie 的实测与我们的实现逐条一致
5. **封禁在账号层，社区三个实现都没解决**

### 7.2 我们该改的（5 项，全部有证据）

| # | 改动 | 依据 |
|---|---|---|
| **F1** | `/api/*` UA 版本：`2.1.0` → **`2.51.0`** | 本机 exe 2.51.0 + magpie 实测（旧值会 403 TM.00000001） |
| **F2** | 推理面 UA：`Bun/1.3.14` → **`claude-cli/2.1.92 (external, cli)`** | sidecar `al="2.1.92"` + magpie |
| **F3** | 补 `anthropic-version: 2023-06-01` | 官方 + lwj + gotsome + karina |
| **F4** | 补 `anthropic-beta: oauth-2025-04-20` | 官方 + magpie + karina + lwj |
| **F5** | 补 `x-anthropic-billing-header` | sidecar `Y_8()` |

**⚠ 但要说清**：F1–F5 影响的是**"能否拿到 200"**，**不影响封禁**。
我们已经证明第 5 个号在**零上游请求**的情况下就被封了 —— 所以这五项**不会解决封号**。

### 7.3 不要做的

- **不要**为了"更像官方"去改成 `/v1/messages` 而不做 Anthropic→OpenAI 翻译 —— 我们下游是 OpenAI 兼容的
- **不要**因为社区"3:3"就去掉签名 —— magpie 的实测证明桌面形态**需要**签名
- **不要**再在请求形状上找封禁原因 —— 五个号的证据已经排除

### 7.4 唯一还没排除的变量

**账号来源**（手机号段 / 注册方式 / 环境）。三个社区实现都不提供账号，所以**无从对比** ——
这条只能靠用户回答：

1. 第五个号（昵称回落成 uid）是怎么注册的？
2. 那四个 `192` 号段从哪来？
3. 桌面端那个正常号是怎么来的？

---

## 8. 参考物（本机路径）

| 内容 | 路径 |
|---|---|
| 我们的项目 | `D:\project_GIT\workbuddy2api` |
| lwj（原始 Go 实现） | `D:\project_GIT\ref\lwj-phanthycode2api` |
| gotsomethings（lwj 的加固分支） | `D:\project_GIT\ref-gotsomethings-phanthywork2api` |
| karina（独立 Go 实现） | `D:\project_GIT\ref\karina-phanthywork2api` |
| magpie（独立 .mjs 实现，文档最全） | `D:\project_GIT\ref\baijiu-magpie`（新版）/ `ref\mycf-magpie`（旧版） |
| 官方终端 CLI（源码） | `D:\npm-global\node_modules\@phanthy\phanthy-codeo\dist\cli.js` |
| 官方 Windows 桥（dump） | `D:\temp\phanthy-sidecar-ascii.txt` |
| 第三方 launcher | `D:\temp\wb2a-test\pcl\package\dist\cli.js` |
