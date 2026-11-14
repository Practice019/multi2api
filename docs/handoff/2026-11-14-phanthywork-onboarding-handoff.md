# 交接文档：phanthywork 新手任务（onboarding）面板

> 接手人：外包高级 agent。本文是**自包含**的 —— 你不需要看任何历史对话。
> 读完第 1–3 节就能开工；第 4 节之后是干活时随时回来查的参考。
>
> 交接日期：2026-11-14　仓库：`D:\project_GIT\workbuddy2api`　分支：`master`
> 交接时 HEAD：`013074c`（`feat(phanthywork): 补上 rewards / referrals-relations / onboarding 三端点；修正取码幂等键`）
> 交接时基线：`go build ./...` exit 0，`go test ./... -count=1` **52 包零 FAIL**

---

## 1. 一句话任务

**给 `phanthywork` 上游补一个「新手任务」面板**：让网关能把该账号的 onboarding 任务列出来，并支持逐个 `start` / `complete`，然后把结果体现在界面上。

**为什么现在能做**：客户端方法已经写好了（`internal/phanthywork/onboarding.go`，5 个端点全覆盖），缺的是
**响应字段结构 → 数据层 → 能力声明 → 界面 → 判据**。

---

## 2. 你的交付物（按顺序，每项都可独立验收）

| # | 交付物 | 验收标准 |
|---|---|---|
| T1 | **字段结构取证报告**（写进 `docs/protocols/phanthywork-出站请求对照表.md` 新增一节） | 给出 `onboarding` 对象的**逐字字段名**，并注明是从官方哪个文件哪一行读到的 |
| T2 | `summaryResponse` 补 `onboarding` 解析（`internal/phanthywork/client.go`） | 新增单测：喂一段官方形状的 JSON，解出的任务列表条数 / 字段与预期一致 |
| T3 | 声明 `CapTasks`（`internal/phanthywork/provider.go` 的 `Caps()`，L510 起） | 原本断言"不许声明 CapTasks"的判据（`internal/phanthywork/provider_more_test.go:472`）必须**改成断言"必须声明"**，且 T4 的视图能跑通 |
| T4 | 任务视图数据层（照 `internal/loomy/provider.go` 的任务视图模板） | 有单测覆盖：空任务、进行中、已完成、上游报错四种形态 |
| T5 | 前端面板（照邀请码面板模板） | 走 `tools/toast_sync.js --check`；主题合规 7/7；内联脚本语法 17 段全过 |
| T6 | 判据（每个新行为一条） | 每条都**反向验证过**（见第 7 节），并在协议文档的判据索引里登记 |

**T1 是硬前置**：拿不到字段名就不许往下做（原因见第 6 节第 7 条）。

---

## 3. 开工前必读（按这个顺序，别跳）

```
1. docs/protocols/phanthywork-出站请求对照表.md      ← 最重要，这个上游的全部协议事实
2. docs/decisions/0001-phanthywork-canonical-separators.md
3. docs/decisions/0002-device-binding-is-terminal.md
4. internal/phanthywork/onboarding.go               ← 已有的 5 个客户端方法（本轮的地基）
5. internal/phanthywork/onboarding_test.go           ← 已有判据的写法风格
6. internal/phanthywork/invite.go                    ← 「读了会写」的面板怎么做的（护栏写法）
7. internal/loomy/provider.go                        ← CapTasks + 任务视图的现成模板（`Caps()` 在 L380-390，已声明 CapTasks|CapInvite）
8. docs/protocols/前端操作反馈协议.md                 ← 动界面之前**必须**读，有三条铁律
9. AGENTS.md（仓库根）与 docs/protocols/00-index.md   ← 项目约定与协议索引
```

`00-index.md` 是协议总入口；如果你要做的事在上面找不到对应协议，说明你可能在发明第五套做法 —— 先找，再写。

---

## 4. 这个上游是什么 / 当前做到哪

`phanthywork` 是 `workbuddy2api`（一个多上游 2API 网关，Go 实现，对外提供 OpenAI 兼容接口）里的一个上游适配器。
它模拟**官方 PhanthyWork 桌面端**去调 `https://code.phanthy.com`，用自己的 ed25519 三把密钥做签名链。

### 4.1 已经做完的（**不要重做，也不要"顺手改回去"**）

2026-11-14 一轮系统性对照之后修好/新增的：

| 项 | 状态 | 判据 |
|---|---|---|
| 三条 canonical 分隔符 = **LF（0x0A）** | ✅ 已按官方源码逐字节核实 | `TestCanonicalSeparatorsArePerString` |
| 推理头与官方 1:1（`/v1/chat/completions`） | ✅ 去掉 3 个幽灵头、`client-source: desktop`、补设备指纹 | `TestInferenceHeadersMatchOfficialClient` |
| `/api/*` 的 UA = `phanthy-code-cli/2.1.0` | ✅ 原先错用 `Bun/1.3.14` | `TestCommonHeadersOnEveryRequest` |
| canonical 的 path 段**不许带 query** | ✅ `signV1` 里当场断言 | `TestSignV1RejectsQueryInPath` |
| 取码幂等键 = `referral-code:<installationId>`（两段） | ✅ 原先多一个日期段 | `TestReferralCodeIdempotencyKeyIsByteExact` |
| rewards / referrals-relations / onboarding×3（5 个端点） | ✅ 已实现客户端方法 | `onboarding_test.go` 里 7 条 |

### 4.2 环境硬事实

- **生产跑在 7863 端口**（`start.bat` 拉起 `bin\wb2api-server.exe`）
- **改文件不影响已在跑的进程** —— 必须重建 + 重启（见第 8 节）
- 当前线上：668 模型 / 15 供应商
- **这个上游的账号已被封**（uid `cmwfxhqzl12tkr53jjqq3dhad`），**无法端到端验证**。
  所以本轮所有验证都只能做到"对假上游 + 对官方源码"这一层。见第 9 节。

---

## 5. 官方真源在哪（这是本轮最重要的资产）

**不需要抓包、不需要逆向 exe。** 官方前端源码在本机就是解压好的：

```
D:\software\PhanthyWork-windows-x64\_up_\dist\           ← 430 个 .js / 17.9 MB
  └ assets\App-dSHMvpu3.js                              ← 1.9 MB，活动面 + 邀请面的全部 API 定义
```

另两条更省事的取法（都不碰官方进程）：

```powershell
# ① 运行中的 sidecar 在 38011 端口就服务这套前端，任意 chunk 直接取
curl.exe -s -o App.js "http://127.0.0.1:38011/assets/App-dSHMvpu3.js"

# ② sidecar 的二进制字符串转储（108.4 MB，服务端路由/头构造器都在里面）
D:\temp\phanthy-sidecar-ascii.txt
```

### 5.1 官方活动面 API 定义（`Po` 对象，逐字，来自 `App-dSHMvpu3.js`）

```js
register: async e => {
  let t = re();                                    // platform: macos|windows|linux
  if (!["macos","windows","linux"].includes(t)) throw Error("desktop_platform_unsupported");
  return No({ localPath:"/api/daily-activity/register",
              upstreamPath:"/api/oauth/desktop-installations/register",
              method:"POST",
              body: JSON.stringify({public_key:e.publicKey, edition:jo(), platform:t, version:Mo()}),
              idempotencyKey:`register:${e.installationId}`, identity:e }) },
summary: async e => {
  let n = await fetch(_("/api/daily-activity/session-key"), ...).then(r=>r.json());
  return No({ upstreamPath:"/api/oauth/activities/summary", method:"POST",
              body: JSON.stringify({inference_public_key:n.public_key}), identity:e }) },
claim: (e,t) => No({ upstreamPath:"/api/oauth/activities/daily-login/claim", method:"POST",
                     body:`{}`, idempotencyKey:`daily:${e.installationId}:${t}`, identity:e }),
startOnboardingTask: (e,t) => No({
  upstreamPath:`/api/oauth/activities/onboarding/tasks/${encodeURIComponent(t)}/start`,
  method:"POST", body:`{}`,
  idempotencyKey:`onboarding:start:${e.installationId}:${t}`, identity:e }),
completeOnboardingAction: (e,t) => No({
  upstreamPath:`/api/oauth/activities/onboarding/tasks/${encodeURIComponent(t)}/complete`,
  method:"POST", body:`{}`,
  idempotencyKey:`onboarding:complete:${e.installationId}:${t}`, identity:e }),
reconcileOnboarding: (e,t) => No({
  upstreamPath:"/api/oauth/activities/onboarding/reconcile", method:"POST",
  body: JSON.stringify({...t?{session_id:t}:{}}),
  idempotencyKey:`onboarding:reconcile:${e.installationId}:${t??"latest"}:${Date.now()}`, identity:e }),
rewards: e => No({ localPath:"/api/daily-activity/rewards?type=daily_login",
                   upstreamPath:"/api/oauth/rewards", method:"GET", identity:e })
```

`jo()` = `"phanthy_work"`，`Mo()` = `"2.51.0"`。

### 5.2 官方邀请面 API 定义（`Id` 对象，逐字）

```js
summary:   e => No({ upstreamPath:"/api/oauth/referrals/me",        method:"GET",  identity:e }),
code:      e => No({ upstreamPath:"/api/oauth/referrals/code",      method:"POST",
                     body:`{}`, idempotencyKey:`referral-code:${e.installationId}`, identity:e }),
relations: e => No({ upstreamPath:"/api/oauth/referrals/relations", method:"GET",  identity:e })
```

### 5.3 官方发送器 `No()`（逐字，签名链全在这里）

```js
async function No(e){
  let t = String(Date.now()), n = Ao(), r = e.body ?? ``;
  if (e.localPath === `/api/daily-activity/register`) {
    let a = await fetch(_(`/api/daily-activity/session-key`)).then(r=>r.json());
    let o = await R(`sign_desktop_device_binding`, { payload: [
      `desktop-device-v1`, e.identity.accountScope, e.identity.installationId,
      e.identity.publicKey, a.public_key, t, n ].join(`<LF>`), accountScope: e.identity.accountScope });
    r = JSON.stringify({...JSON.parse(r), device_public_key: e.identity.devicePublicKey,
                        device_signature: o, inference_public_key: a.public_key});
  }
  let i = await R(`sign_desktop_activity`, { payload: [
    `v1`, e.method, e.upstreamPath, t, n, await ko(r), e.idempotencyKey ?? `` ].join(`<LF>`),
    accountScope: e.identity.accountScope });
  let a = { "Content-Type":`application/json`,
            "x-desktop-installation-id": e.identity.installationId,
            "x-desktop-timestamp": t, "x-desktop-nonce": n, "x-desktop-signature": i };
  e.idempotencyKey && (a[`idempotency-key`] = e.idempotencyKey);
  let o = l(); o && (a.Authorization = `Bearer ${o}`);
  let t2 = await fetch(_(e.localPath), { method:e.method, headers:a,
                       body: e.method===`POST` ? r : void 0, signal: ... });
  ...
}
function Ao(){ return Oo(crypto.getRandomValues(new Uint8Array(18))) }   // nonce 18 字节
function ko(e){ return Oo(new Uint8Array(await crypto.subtle.digest(`SHA-256`,
                        new TextEncoder().encode(e)))) }                  // sha256(body 原文)
```

⚠ 上面 `<LF>` 是**真实换行符**（`U+000A`）。源码里的写法是模板字面量 `` .join(` + 换行 + `) ``。
已按字节核实过：`sign_desktop_device_binding` / `sign_desktop_activity` / `desktop-inference-v2`
三处**都是 LF**，本仓 `deviceV1Separator = "\n"` 与之一致。

### 5.4 sidecar 的推理头构造器（`/v1/chat/completions`，逐字）

```js
X = () => ({
  Authorization: `Bearer ${token}`,
  "Content-Type": "application/json",
  Accept: "application/json",
  "x-phanthy-client-source": "desktop",
  "x-phanthy-cli-device": <设备指纹>,
  "x-claude-code-session-id": sessionId,
})
// 调用处再补 "x-request-id": <uuid>
```

官方 `/api/*` 通用调用器（`wtK`）：

```js
{ Accept:"application/json", "Content-Type":"application/json",
  "User-Agent":`phanthy-code-cli/${GK9}`,      // GK9 = process.env.APP_VERSION || "2.1.0"
  "x-phanthy-client-source":"desktop",
  ...q.headers, Authorization:`Bearer ${K}` }
```

---

## 6. 硬约束与陷阱（全是踩过的，别重踩）

1. **三条 canonical 的分隔符是 LF，不是空格。** 按字节核对，不要按"看起来像"判断。
   （曾因写成空格导致设备注册恒 401 `invalid_desktop_signature`，而错误码完全指不到根因。）

2. **canonical 的 path 段不许带 query。** URL 可以带 `?type=daily_login`，但签名的 path 必须是 pathname。
   `signV1` 已加断言，会当场报错。

3. **GET 也要 v1 签名。** 官方 `No()` 对所有方法都算签名；GET 的 body 是 `undefined`
   ⇒ canonical 第 6 段是 `sha256("")` 的 base64url（`47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU`）。
   **不要**写成"只读请求不用签名"。

4. **body 一律是字面量。** `start`/`complete`/`claim`/`referral-code` 是 `{}` ——
   不是空串、不是 `null`、不是 `{"foo":1}`。已有判据会红。

5. **幂等键的语义各不相同，不要互相类推**：
   - `daily:<installationId>:<日>` —— **带**日期（表达"这一天"）
   - `referral-code:<installationId>` —— **不带**任何时间（表达"这个安装一个码"）
   - `onboarding:start|complete:<installationId>:<taskId>` —— 稳定，重复调用命中同一条
   - `onboarding:reconcile:<installationId>:<sessionId|"latest">:<ms>` —— **带**毫秒，每次都是新操作
   - `activities/summary` —— **不发**幂等键（canonical 第 7 段是空串，但分隔符不能省）

6. **任务 id 必须匹配 `^[a-z0-9_]{1,80}$`**（sidecar 的路由判据，逐字）。
   已在 `onboardingTaskPath` 里本地校验 → 非法 id **零请求**。

7. **注释也会骗人 —— 以代码为准。** 交接时刚修掉两处会误导人的注释：
   - `internal/phanthywork/provider.go` 的能力位注释原来把 `CapInvite` 列在"不声明"里，
     而 `Caps()` 实际**早就声明了**（邀请码面板 2026-11-12 就实现了）。已更正。
   - `docs/decisions/0001-*.md` 正文里"device/v1 用空格"的结论是错的（真机反证 + 官方源码
     双重否定），已在**顶部**加更正块，但**正文没改**（ADR 只增不改）—— 读到正文时别被带偏。
   ⇒ 接手任何一处之前，先看代码本身，再看注释。

8. **响应结构不许猜。** 这是本轮的硬规矩：`onboarding` / `wallet` 的字段名如果没从官方源码
   或真实响应里读到，就**返回 `json.RawMessage` 并记档为"未知"**，不许照名字编。
   历史上就是因为"看起来对"而写错了分隔符、写错了 UA、写错了幂等键 —— 三次都通过了全部测试。

9. **判据不许守护错误行为。** 本项目出过两次：一条判据**断言了**两个官方根本不存在的头；
   另一条在注释里被引用但**从来没写出来**。接手后如果你发现某条判据在锁死一个可疑的行为，
   先怀疑判据，再怀疑实现。本轮又出过一次：两条判据在断言"取码幂等键带日期段"这个**错格式**。

10. **不要动 `worktree` 里那 100 多个历史遗留改动**（如 `cmd/server/*creds*.go`）。
    提交时**显式按路径 stage**，不要 `git add -A`。

11. **界面的三条铁律**在 `docs/protocols/前端操作反馈协议.md` 里（单一通知层、按钮宽度不漂移、
    结果不进按钮文案）。动任何表格/按钮前必读，有 8 条判据守着。

---

## 7. 判据纪律（这个项目的核心工程习惯）

**每一条新行为都要有一条能失败的命令。**

- 判据写在 `internal/phanthywork/*_test.go` / `internal/server/*_test.go`。
- 注释里必须写 **真源**（从哪个文件哪一行读到的），而不是"我以为"。
- **必须做反向验证**：故意注入一处违规，确认判据真的会红，然后还原。
  没有反向验证过的判据**不算数**。
- 参考已有写法：`internal/phanthywork/onboarding_test.go`（7 条，其中 3 条做过注入验证）。

反向验证的做法（本仓常用）：

```powershell
$f = 'D:\project_GIT\workbuddy2api\internal\phanthywork\onboarding.go'
$bak = "$f.bak"
Copy-Item $f $bak -Force
$txt = [System.IO.File]::ReadAllText($f)
[System.IO.File]::WriteAllText($f, $txt.Replace('<正确写法>', '<违规写法>'), (New-Object System.Text.UTF8Encoding $false))
Set-Location 'D:\project_GIT\workbuddy2api'; go test ./internal/phanthywork/ -count=1 -run '<判据名>'
Copy-Item $bak $f -Force; Remove-Item $bak -Force   # 还原后必须重新跑一次确认变绿
```

⚠ **`.NET` 文件 API 用的是进程 CWD，不是 PowerShell 的 `cd` 目录** —— 一律用**绝对路径**。

---

## 8. 环境与命令

```powershell
$repo = 'D:\project_GIT\workbuddy2api'
Set-Location $repo
$env:GOGC='60'; $env:GOMAXPROCS='4'

# 构建 / 测试（交付前两条都必须过）
go build ./...                                   # 期望 exit 0
go test ./... -count=1                           # 期望 ok x 52 包，零 FAIL
go vet ./internal/phanthywork/                   # 期望干净

# 前端三条闸（动了 .html/.js 才需要）
node tools/toast_sync.js --check                 # 通知层与宿主页同步
node tests/frontend/verify_theme_compliance.js   # 期望 7/7
node tests/frontend/verify_inline_script_syntax.js  # 期望 17 段
```

**部署**（改文件不影响已运行进程，必须重建 + 重启）：

```powershell
go build -o bin\wb2api-server.exe.tmp .\cmd\server
Copy-Item bin\wb2api-server.exe "bin\wb2api-server.replaced-$(Get-Date -f yyyyMMdd-HHmmss).exe" -Force
$c = Get-NetTCPConnection -State Listen -LocalPort 7863 -ErrorAction SilentlyContinue
if ($c) { Stop-Process -Id $c.OwningProcess -Force; Start-Sleep 2 }
Move-Item bin\wb2api-server.exe.tmp bin\wb2api-server.exe -Force
Start-Process cmd.exe -ArgumentList '/c','start.bat' -WorkingDirectory $repo
```

**回归确认**（部署后）：

```powershell
$cfg = Get-Content config.json -Raw | ConvertFrom-Json
$ck = "$env:TEMP\ck.txt"
curl.exe -s -o NUL -c $ck "http://127.0.0.1:7863/ui?token=$($cfg.admin_token)"
curl.exe -s -b $ck http://127.0.0.1:7863/ui/models/data    # 期望 668 模型 / 15 供应商
curl.exe -s -H "Authorization: Bearer $($cfg.api_key)" http://127.0.0.1:7863/v1/models
```

---

## 9. 未知项 / 需要外部输入的地方（**这些不要自己拍**）

1. **`onboarding` 对象的字段名**（T1 的核心）。
   已知：官方前端把 `summary.onboarding` 整个对象往 UI 传，但**没有在 bundle 里展开字段名**，
   所以要么从 `App-dSHMvpu3.js` 里继续挖（搜 `onboarding?.`、`summary?.onboarding`、
   以及 `onboarding.tasks` 之类的访问链），要么等真实账号跑一次 `summary` 看响应。
   **挖不到就记档为未知，不许编。**

2. **任务 id 的实际取值清单**。官方 8 项任务的 id 由服务端下发，本机没有任何一处存着它们。
   ⚠ 注意一个**已验证的矛盾**：`internal/loomy/provider.go:383` 的注释说任务端点是
   `/api/v1/onboarding/tasks*`、"8 项共 10000 分"，而 phanthywork 官方是
   `/api/oauth/activities/onboarding/tasks/{id}/{action}`。**两家的任务体系可能是不同的**，
   不要直接照搬 loomy 的 id 或分值。

3. **端到端验证需要一个全新的、未被封的账号**。
   当前账号已被封（403 `disabled_account`），所以 T1–T5 只能验证到
   "对官方源码 + 对假上游"这一层。真实链路验证需要用户提供新号。
   **拿到新号后的第一次调用顺序**（有理由，别改）：
   `register` → `summary`（拿推理令牌）→ **先做 quotas / 目录 / 签到** → 隔几小时 → 才第一次推理。

4. **`CapTasks` 的面板要不要做**是产品决策。本文默认"要"，但如果用户只想先有数据层，
   T5 可以拆出去单独一轮。

---

## 10. 交接边界（**别越界**）

- ✅ 可以做：`internal/phanthywork/**`、`internal/server/webui-*.html` 与对应的
  `app.js` 片段、`internal/server/*_test.go`、`docs/protocols/**`、`docs/decisions/**`（**只追加**）
- ⚠ 改之前先问：`internal/gateway/**`（能力枚举）、`cmd/server/**`（装配）
- ❌ 不要碰：其他上游的目录（`internal/loomy`、`internal/zcode`、`internal/monkeycode` …）、
  `tools/toast_sync.js` 的 `PAGES` 之外的部分、worktree 里那 100 多个历史遗留改动
- ADR 是**只增不改**：结论变了就写新的并标注 `Superseded by ADR-XXXX`，不要改旧 ADR 的正文
  （已有的更正段是"顶部加更正块"的写法，可以照抄）

---

## 11. 建议的开工顺序

```
第 0 步  读第 3 节那 9 份文件；跑一次 go test ./... 确认 52 包绿（建立基线）
第 1 步  T1：挖 onboarding 字段结构。挖不到就先停下来找用户要一次真实 summary 响应
第 2 步  T2：补解析 + 单测（这一步最容易，也最能立刻验证 T1 的结论）
第 3 步  T3：声明 CapTasks，把原来那条"不许声明"的判据改成"必须声明"
第 4 步  T4：任务视图（照 loomy 模板），四种形态各一条判据
第 5 步  T5：前端面板（照 invite 面板模板），过三条前端闸
第 6 步  T6：补齐判据索引，逐条做反向验证
第 7 步  build + test + vet 全绿 → 提交（显式按路径 stage）→ 部署 → 回归确认
```

---

## 12. 交付时的回报格式

```text
状态：OK / BLOCKED（BLOCKED 必须写明卡在哪、需要用户提供什么）
改动的文件清单（相对路径）
执行过的命令 + 输出摘要（含 go test 的包数与 FAIL 数）
新增判据清单（判据名 / 守什么 / 反向验证怎么做的）
未决问题（尤其是第 9 节那四项的进展）
遗留风险
```

---

## 附：一句话记住这个项目的工程文化

**"看起来对"不算证据。** 这个上游一天之内挖出过三个"看起来对、其实不对"的缺陷
（分隔符、幽灵头、UA 分档），每一个都**通过了全部测试**，因为判据本身在守护错误行为。
所以：真源 → 逐字节核对 → 写判据 → **反向验证判据会红** → 才敢说做完了。
