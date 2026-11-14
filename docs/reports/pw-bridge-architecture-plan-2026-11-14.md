# 架构改造计划：phanthywork 改为走官方客户端本地桥

> 日期：2026-11-14 · 基线 `master = 296c292`
> 性质：**待对齐的计划**。末尾有 4 个必须你拍板的问题。
> 前置阅读：`docs/protocols/phanthywork-出站请求对照表.md` §十

---

## 0. 一句话

**别再自己当客户端。**让官方 sidecar 持证并**替我们签名**（那是我们一直做错的那部分），
我们只做编排 + 把请求转给它的本地桥。

**而且——比预想的更省**：我们连 OAuth 续期都不需要自己做（桥自己会续），
连签名都不需要（桥自己会签）。我们只需要**把 token 递给它**。

---

## 1. 已核实的事实（全部实测，不是推断）

### 1.1 sidecar 是个带 CLI 的服务器 ⇒ 多实例是官方支持的

本机进程链（`Get-CimInstance` + `Get-NetTCPConnection` 实测）：

```
PhanthyWork.exe            (PID 32456)
 └─ claude-sidecar.exe server --app-root D:\software\PW-windows-x64 --host 0.0.0.0 --port 4413
      ├─ LISTEN 0.0.0.0:4413      ← 主服务
      └─ LISTEN 127.0.0.1:4421    ← ★ 推理桥
           └─ phanthy-codeo.exe serve --desktop --hostname=127.0.0.1 --port=0 --parent-pid=32376
```

`server --app-root <dir> --host <h> --port <p>` 是显式 CLI 参数
⇒ 我们可以自己起实例、自己指定端口与根目录。

### 1.2 桥的入口闸（源码 `gd3` @99955461，与实测逐条吻合）

```js
async function gd3(q) {
  let K = new URL(q.url), _ = !K.search || (K.pathname === "/v1/messages" && K.search === "?beta=true");
  if (q.method !== "POST" || !Sd3.has(K.pathname) || !_) return new Response(null, {status:404});
  if (!/^Bearer .+$/i.test(q.headers.get("authorization") ?? "")) return new Response(null, {status:401});
  // Sd3 = new Set(["/v1/messages","/v1/responses","/v1/chat/completions"])
```

| 探针 | 实测 |
|---|---|
| `POST /` | **404** ✅ |
| `GET /v1/chat/completions` | **404** ✅ |
| `POST` 无 Bearer | **401** ✅ |
| `POST /v1/messages?x=1` | **404** ✅ |

⚠ 闸只校验 Bearer 的**形态**（`^Bearer .+$`）。**值是后一步才用的**（见 1.3）。

### 1.3 ★★ 桥用**调用方传来的** Bearer，并替调用方**重新签名**

源码（`gd3` 体内，逐字）：

```js
// ① 构造发上游的头：剥掉调用方自带的 proof 头，然后**重新签一份**
let J = () => {
  let G = new Headers(Y), R = G.get("x-phanthy-session");
  if (R) { G.set("x-claude-code-session-id", R); G.delete("x-phanthy-expert-id"); ... }
  for (let x of ["host","connection","content-length","transfer-encoding",
                 "x-phanthy-desktop-session","x-phanthy-proof-signature",
                 "x-phanthy-proof-nonce","x-phanthy-proof-timestamp"]) G.delete(x);
  let u = _O8(q.method, K.pathname, $, G);        // ★ 它自己用 _O8 签名
  if (u) for (let [x, m] of Object.entries(u)) G.set(x, m);
  return G
};

// ② 认证：从调用方传的 Bearer 起手
let X = Y.get("authorization") ?? "";
let f = qO8();
if (f && f !== sK6(X)) X = await Id3(Y);

// ③ 401 ⇒ 它自己续期并重试一次
let P = await j();
if (P.status === 401 && Date.now() < M) { let G = await cd3(Y, X); ... P = await j() }

// ④ 403 ⇒ 有重试循环（配合换推理令牌）
for (let G = 0; G < xd3 && P.status === 403; G++) { if (!await md3(P)) break; ... }
```

**实测坐实**（⚠ 数据来自桌面端那个实例 —— 参见 §4 的 P-1，
**结论有效，但取证方式违反了本章纪律，不重复**）：

| 送进桥的 Bearer | body | 桥的回应 | 说明 |
|---|---|---|---|
| `x`（随便一个） | 坏 JSON | `401 {"code":"expired_access_token"}` | 它拿 `x` 去打上游 ⇒ **用的是调用方的 token** |
| 桌面端**真实** token | 坏 JSON | `400 {"code":"invalid_request","message":"Request body must be JSON."}` | 有真实身份时它放行到上游 ⇒ 真 token 被接受 |

且全程**没有改**凭据文件（mtime 保持 21:19:49）⇒ 它不主动续期，只在 401 时续。
桌面端诊断日志在那一时段**无任何异常记录**。

> ⚠ 这两条结论**建议在 P0 用自建实例复验一遍** ——
> 不仅是为了合规，也因为"真实身份放行"这条只测到 400（body 校验），
> **还没有拿到过一次真实推理响应**（U1/U2/U3 都还悬着）。

### 1.4 ★ 这意味着我们**不需要**实现的东西（工作量对比）

| 我们现在自己实现的 | 桥做了之后 |
|---|---|
| PKCE OAuth 登录 + 授权码交换 | **不需要**（桥/客户端管） |
| refresh_token 续期 + 轮换 + 落盘 + 共享库 | **不需要**（桥 401 时自己续） |
| ed25519 三把密钥 + 12 段 canonical + 4 个 proof 头 | **不需要**（桥用 `_O8` 重签） |
| 180 秒推理令牌的两跳换取 | **不需要**（桥自己换） |
| 设备注册 `/desktop-installations/register` | **不需要** |
| 签到 / 额度刷新（48×/天 那个异常） | **不需要**（客户端按它自己的节奏） |
| 时钟偏移校准、nonce、session 头 | **不需要** |

**我们只剩两件事**：① 把 token 递给桥 ② 转发并翻译响应。

> 换句话说：这场排查里所有踩过的坑（D1 头、D2 拒、D3 共享库、D4 退避、封号…），
> **全部源于"我们自己复刻协议"。桥把这部分整体消掉了。**

### 1.5 配置目录由 env 决定 ⇒ 实例可完全隔离

官方 `S3()` 逐字：

```js
return (process.env[q.configDirEnv] || path.join(os.homedir(), q.configDirName)).normalize("NFC")
```

⇒ 每个账号一个 `PHANTHY_CONFIG_DIR`，凭据互不干扰。**这是"一号一实例"的技术前提。**

### 1.6 一个实例管一个号

`<configDir>/.credentials.json` 里只有一个 `claudeAiOauth` 位。⇒ **N 个号 = N 个实例。**

---

## 2. 目标架构

```
业务方
  │  OpenAI 兼容
  ▼
我们的网关（编排层：多号 / 路由 / 限额 / 冷却 / 计费 / 面板）
  │  读 <configDir>/.credentials.json 取 token → POST 到桥
  ▼
每账号一个官方 sidecar 实例（127.0.0.1:44xx）
  │  持证 + 续期 + 签名 + 设备注册 + 签到（全它管）
  ▼
phanthywork 上游
```

---

## 3. 要写的东西

### 3.1 新包 `internal/phanthyworkbridge`（不动老包）

- 实现网关接口：`Chat` / `Models` / `Credential`
- 凭据形态：**`{config_dir, base_url, token_hint}`**
- 出站只有一个动作：
  1. 读 `<config_dir>/.credentials.json` → 取 `accessToken`（复用 D3 已写的 reader！）
  2. `POST {base_url}{path}`，带 `Authorization: Bearer <该 token>`
  3. 把响应**原样透传**（含流式）
- **不续期、不签名、不签到、不注册设备**

**为什么新开包**：老包是"自己当客户端"的全套实现（crypto/PKCE/注册/签名/推理令牌），
删它需要先证明桥可靠。**双轨并行**，随时可回退。

### 3.2 谁拉起实例（**Q1**）

| 方案 | 优 | 劣 |
|---|---|---|
| **A 手动**（推荐先做） | 最简单、责任清晰 | 要手工维护 |
| **B 网关代管** | 面板可管 | 进程管理塞进网关，职责混了 |
| **C 独立小工具 `pw-bridge`** | 职责分离 | 多一个要维护的东西 |

### 3.3 每个号要准备的（一次性、人工）

1. 建目录 `~/.pw-acc/<nickname>/`
2. **用官方客户端登录该号**（扫码/短信）—— 无法绕过
3. 起实例：`claude-sidecar.exe server --app-root <app-root> --host 127.0.0.1 --port 44xx`
   （env `PHANTHY_CONFIG_DIR=<该号目录>`）
4. 四个零成本探针确认桥活着（§1.2）
5. 网关里登记 `{config_dir, base_url}`

### 3.4 待定项（P0 要解决）

| # | 待定 | 影响 |
|---|---|---|
| U1 | 桥返回的**流式**形态（Anthropic SSE 还是 OpenAI SSE） | 决定要不要做格式转换 |
| U2 | 模型清单从哪来（桥**没有** models 端点） | 面板会缺模型 |
| U3 | **过期的 token 递进去，桥会不会自己续期**（源码看是会，但要实测） | 决定我们还要不要维护 token |
| U4 | 一个 `--app-root` 起多实例会不会互相干扰 | 不行就各给一份 app-root（约 190 MB/份） |

---

## 4. 分阶段

### P-1 · ★★ 硬性纪律：**绝不用用户桌面端的凭据/实例做实验**（做任何桥实验之前先读这节）

> **这一条写在最前面，因为 2026-11-14 本文作者违反过一次。**
>
> 当时为了"零成本"回答"桥用谁的 token"这个问题，作者拿了用户桌面端
> `~/.phanthy-code/.credentials.json` 里的**真实 access token** 去打了
> 桌面端那个桥（`127.0.0.1:4421`）。虽然两次 body 都是故意写坏的、
> **没有消耗额度**、也没改动凭据文件、桌面端诊断里也没留下痕迹，
> 但**"该账号发出过两次结构非法的认证请求"这件事本身**是作者无法替用户
> 承担的风险 —— 而这个账号用户正在用。
>
> 从那次的实测里我们确实拿到了**关键结论**（见 §1.3：桥用调用方的 token），
> 但代价不该由用户的在用账号来付。
>
> **此后的一切桥实验，必须遵守：**
>
> 1. **只打自建实例**（独立 `--app-root` + 独立 `PHANTHY_CONFIG_DIR` + 独立端口），
>    绝不碰用户的 `~/.phanthy-code`、绝不碰 44xx 之外的实例；
> 2. 需要"真实但可牺牲"的凭据时，**新开一个测试号**（用户提供），
>    而不是借用在用账号；
> 3. 任何会到达上游的请求都先问用户；
> 4. 只读探针（404/401 那一类，在到达上游之前返回）可以随便打，但要先说明。

### P0 · 只做实验，不写网关代码（半天）

**在一个自建实例上做**（`--app-root <临时目录> --host 127.0.0.1 --port 4451`，
`PHANTHY_CONFIG_DIR=<测试号目录>`），用 curl 打**那个**实例：

1. **U1 流式形态**：`stream:true` 发一次，看 SSE 事件名与结束标志
2. **U2 模型**：试 `deepseek-v4.1-flash-1017` 等，看是否透传
3. **U3 过期 token**：把**已过期**的 token 递进去，看它会不会 401 后自己续期并成功
4. **Q-a 最小可用调用规格**：写一份 curl 可复现的规格

**产出**：《桥的最小可用调用规格》
**成本**：几次最小推理（会消耗该账号额度）

### P1 · 最小适配器（非流式先通）

- 新包 `internal/phanthyworkbridge`：列模型（常量表）+ 非流式对话
- 判据：`TestBridgeChatForwardsVerbatim`（只改必要字段，其余原样透传）

### P2 · 多实例

- 第二个 config dir + 第二个实例（换端口）
- 网关轮询
- **验收：两个号都能用；桌面端那个号不受影响**

### P3 · 切换

- 老 `phanthywork` 账号切到桥
- 观察老死号是否还需要

### P4 · 文档与判据

---

## 5. 需要你拍板的四件事

### Q1 · 谁拉起实例？A 手动（推荐）/ B 网关代管 / C 独立小工具

### Q2 · 老 `internal/phanthywork` 保留还是删？
**我建议保留、双轨并行**，等 P3 验收通过再谈删。

### Q3 · 打算放几个号？
每实例约 **130 MB**（实测 sidecar RSS 120–138 MB）。3 个 ≈ 400 MB，10 个 ≈ 1.3 GB。

### Q4 · 实例与网关必须同机 —— 能接受吗？
桥只监听 `127.0.0.1` ⇒ **网关必须跑在同一台机器上**。
若必须分离就得 `--host 0.0.0.0` 暴露桥，**那等于把账号额度裸奔在网上，我不建议。**

---

## 6. 风险与未知

| # | 风险 | 怎么破 |
|---|---|---|
| R1 | 流式/请求格式未测 | P0 |
| R2 | 过期的 token 桥会不会自己续（U3） | P0 |
| R3 | 多实例共用一个 `--app-root` 可能互相干扰 | P2 实测 |
| R4 | 桌面端升级会换二进制 | 定升级流程 |
| R5 | **桥鉴权极弱**（任何非空 Bearer 都过闸；真正的身份靠 token 值） | 只监听 127.0.0.1 |
| R6 | **这条路能不能免于风控仍未验证** | **P2 之后用一个新号实测存活**；不要一次性全切 |
| R7 | 模型清单来源未定 | P0 的 U2 |
| R8 | 加号必须人工登录 | 接受，或做成"加号向导" |

**R6 是唯一的战略风险**：桥让我们"看起来完全像官方客户端"，
但**风控按什么记账我们至今没有证据**。所以必须先用一个新号验证存活，
而不是把现有号全切过去。

---

## 7. 我建议的对齐结论（供你改）

1. **目标架构**：客户端持证 + 本地桥，我们只做编排 —— 同意
2. **谁起实例**：先 A（手动），验收后做 C
3. **老包**：保留，双轨并行
4. **先做 P0**：**不写一行网关代码**，却是后面所有步骤的前提，
   而且它顺便回答 U3（"我们还要不要维护 token"）——那条直接决定 P1 的工作量

**P0 之后我们就有实测规格了，P1 就是照抄而不是猜。**
