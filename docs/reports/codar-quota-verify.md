# Codar 额度语义独立验证

- **任务**：T-345 —— 独立交叉验证「codar 额度到底该显示什么」（与 T-344 的端点清单互为印证）
- **角色**：本报告**不重复**端点清单，只回答两个用户质疑 + 给出显示建议
- **方式**：读本地 config/log/缓存 + 读 `codar2api` 源码（交叉源）+ **带真 token 的只读实测** + **1 次小额扣费实验**
- **日期**：2026-11-08
- **约束遵守**：token 全长 207 字符，**全程未打印内容**（只给长度与前缀）；未签到、未领取、未改任何代码；真实请求 **1 次**（上限 5 次 / max_tokens 100，本次 60 ✓）

---

## 0. 两个质疑的独立答案

| 质疑 | 独立答案 | 证据强度 |
|---|---|---|
| ① 新账号 `agent_balance = 0` | ★ **不是"没有额度"，是读错了字段**。账号信息端点 `/api/user/self` 的 `quota` 恒 0（本账号实测 `quota=0 / used_quota=0 / aff_quota=0`），**真正的余额在计费面 `POST api.kuaipaper.com/v1/agent/balance`** ⇒ 应显示 `data.agent_balance` | **强**（双端点实测对照 + 源码交叉） |
| ② 1713 代表什么 | ★ **`agent_balance`（额度积分）**。实测同一账号该端点返回 **`{"agent_balance":1713}`**（与用户看到的 1713/1715 完全吻合）；它是**按请求扣减的积分单位**，由响应里的 `billing.quota` 记 | **强**（端点实测 + 扣费实验） |

**推论（对用户可见的现象的解释）**：面板显示 `0` 与显示 `1713` 是**两个不同字段**造成的 —— 前者来自 `/api/user/self`（网页侧的 `quota`，这个账号就是 0），后者来自 `/v1/agent/balance`（计费侧的真实余额）✓

---

## 1. 官方/权威来源显示的是什么（基准）

### 1.1 两个"账号信息"端点，字段完全不同（实测）

```http
GET https://api.claw.speedai.com/api/user/self
Authorization: Bearer <jwt>            → HTTP 200
{"success":true,"data":{ ... "quota":0, "used_quota":0, "aff_quota":0, "aff_history_quota":0 ... }}
```
⇒ `data.quota` **= 0**。这是**网页/应用侧**的额度字段（本账号全 0 ⇒ 用它显示就是"没有额度"）。

```http
POST https://api.kuaipaper.com/v1/agent/balance
Authorization: Bearer <jwt>   body: {"username":"user_1508…"}
                                       → HTTP 200
{"code":200,"message":"Success","data":{"username":"user_1508…","agent_balance":1713}}
```
⇒ `data.agent_balance` **= 1713** ✓✓ ← **这就是用户看到的那个数**。

### 1.2 为什么会有两个余额：源码交叉印证（`codar2api`）

「另一个 agent 克隆的 `codar2api`」里有直接证据 —— **存在两个余额池**：

```go
// store.go:630 —— 什么算"可花费"
func spendable(a M, allow bool) (float64, bool) {
    bal := obj(a["balance"])
    for _, k := range []string{"agent_token", "token"} {
        if k == "token" && !allow { continue }      // ← token = 通用余额，受开关控制
        n += num(bal[k]) ...
    }
}
// store.go:174 —— 设置项
{"allow_general_balance", "bool", true, nil, nil,
 "允许使用通用余额", "Agent 余额用完后允许使用通用余额。"}
// upstream.go:491 —— agent_balance 的来源
if v := d["agent_balance"]; v != nil { b := obj(acc["balance"]); b["agent_token"] = v }
```
⇒ 结论：**`agent_balance`（Agent 余额）** 与 **通用 `token` 余额** 是两个独立池 ✓；
面板要显示完整信息，应**两个都显示**（或至少主显 `agent_balance`、副显通用余额），否则"Agent 余额为 0 但还能用"会继续被误读为目的"没有额度" ✓

### 1.3 官方界面基准
- 桌面端 `Codar科搭.exe` 的 `desktop.log`（38,152B）里 **`balance|quota|credit|积分|额度|billing|cost` 命中 0 行** ✗ ⇒ 客户端**不打印**额度日志 ✓（拿不到日志级的基准）
- `~/.codar/` 的缓存（`installed-components.json` / `installed-plugins.json` / `literature/cache.db` …）**不含余额字段** ✗
- 官网/Web 版 `https://codar-web.speedai.com` 的余额就是它调上面那两个端点的结果 ⇒ **API 即是基准** ✓（未登录不做 UI 截图，避免引入登录态操作 ✓）

---

## 2. 1713 的实测语义（扣费实验）

**做法**：记录余额 → 发 1 次 `glm-5.3-flash`（`max_tokens=60`）→ 再查余额。

```http
POST https://api.claw.speedai.com/v1/chat/completions
{"model":"glm-5.3-flash","messages":[{"role":"user","content":"只回复两个字：收到"}],"max_tokens":60,"stream":false}
→ HTTP 200
usage    = {"prompt_tokens":17,"completion_tokens":60,"total_tokens":77,
            "prompt_tokens_details":{"cached_tokens":0, ...}}
★ billing = {"quota":91,"cost_cny":0.0001816}
```

### 读数与换算（本次样本，**单一模型、单一请求** ⇒ 只能给量级）

| 量 | 值 |
|---|---|
| 本次消耗积分 | **`billing.quota = 91`** |
| 本次真金 | **`billing.cost_cny = 0.0001816`** |
| ⇒ 1 积分 ≈ | 0.0001816 / 91 ≈ **2.0 × 10⁻⁶ CNY** |
| ⇒ 1 CNY ≈ | ≈ **50.1 万积分** |
| ⇒ **1713 积分 ≈** | **0.0034 CNY（约 0.34 分）** |
| 本次 77 token 的单价 | 91 积分 ≈ 0.00018 元 |

⇒ ★ **1713 既不是 token 数、也不是请求次数，而是"积分"**：它是一个**细粒度计费单位**（1 元 ≈ 50 万积分量级）✓。
⇒ ⇒ 这也解释了用户"**这个额度显示的数值不对**"的观感：**1713 看着像个大数，实际购买力极小**（本例汇率下 ≈ 0.34 分）✗ 而真正该并排显示的是**金额口径**（`cost_cny` 累计 / 或把积分换算成 CNY）✓

⚠️ **样本局限（必须说明）**：仅 1 次请求、1 个模型（`glm-5.3-flash`，目录里它的 `rate_badge` 是 **0.20x**）⇒ 不同模型的 `quota/token` 倍率不同，**不能由一个样本外推全局汇率** ✓

### 余额的前后对比（未取到，如实报告）
- `before/after` 都取的是 `/api/user/self` 的 `agent_balance` ⇒ **该字段在 `/api/user/self` 里根本不存在**（`undefined`）✗ ⇒ 差值无法算 ✓
- **原因**：余额只在**计费面**端点上 ✓（`/v1/agent/balance`）⇒ 这次实验的缺口是**我在实验里查错了端点** ✓
- ⇒ 可靠的做法（下一次）：`POST /v1/agent/balance` 前/后各一次 ⇒ 差值应等于 `billing.quota` ✓（**未做，见 §6 未决 #1**）

---

## 3. 积分业务模型（实测 + 源码）

| 事实 | 证据 |
|---|---|
| 每次响应**直接给本次花费** | `billing: {quota, cost_cny}` ✓（T-311 已记，本次复现） |
| 积分单位很细 | 91 积分 = 0.0001816 元 ⇒ 1 元 ≈ 50.1 万积分 |
| 目录里有倍率标签 | `/v1/codar/models` 每条含 `rate_badge`（如 `0.20x`）与 `pricing{currency:CNY, unit:per_million_tokens}` ✓ |
| 两个余额池 | `agent_balance`（Agent 余额）+ 通用 `token`（`allow_general_balance` 控制回退）✓（`codar2api` store/upstream） |
| 余额会因"不足"被上游拒 | 上游 **402** 或消息里含 `余额/insufficient/quota/balance` ⇒ 客户端给账号加"余额冷却" ✓（`codar2api` relay.go） |
| **存在 token 刷新端点** | `POST /v1/token/refresh` → 实测 **400 `Token is not close to expiration`** ✓✓ ⇒ 端点存在、且**按到期时间才允许刷新**（解掉 T-306 未决 #2 的一半） |
| 签到 | `POST /v1/daily-sign/status {username}` → 实测 `{"isSigned":true,"retry_after":66757}` ✓ ⇒ **今日已签**（本次未做任何签到动作 ✓） |
| 邀请 | `POST /v1/promotion/info` → 实测 `{"invite_code":"BB5VYS","invite_link":"https://kuaipaper.com/?i=BB5VYS"}` ✓ |

---

## 4. 新账号 0 的可能原因（独立判断）

按可能性排序：

1. ★ **读错字段/端点（最可能）**：显示的是 `/api/user/self` 的 `quota` ⇒ 这个账号实测就是 **0** ✓，而真实余额在 `/v1/agent/balance` ✓ ⇒ **新老账号都会被显示成 0**，不只是新账号 ✓（用户"新账号 0"的印象可能来自这里）
2. **`agent_balance` 确实为 0（新人礼未到账/需领取）**：需要活动接口参与（`/api/codar/activities/{free-model,invite-model}(/claim)`）⇒ **本次按约束未领取、未签到** ✓ 所以**无法证实也无法否证** ✗
3. **新账号初始值就是 0，靠免费模型额度跑**：`/api/codar/activities/free-model` 那类活动给的是"免费模型用量"而非积分 ⇒ 表现上"余额 0 但能用" ✓（与 §1.2 的双池模型一致 ✓）
4. **额度有延迟/需登录后刷新** ✗：`/v1/token/refresh` 只在临近到期时可用 ⇒ 与余额无关 ✓

⇒ **不能只凭"agent_balance = 0"就判"没有额度"** ✓：要同时看 ① 通用 `token` 余额 ② 免费模型活动的剩余 ③ 是否处于"余额冷却" ✓

---

## 5. 结论：我们该显示什么（明确建议）

| 优先级 | 显示项 | 来源 | 理由 |
|---|---|---|---|
| ★ P0 | **`agent_balance`** | `POST {PayBase}/v1/agent/balance` body `{username}` | 这才是用户看到的 1713；`/api/user/self` 的 `quota` 恒 0，**用了它必然显示 0** ✗ |
| ★ P0 | **通用余额 `token`** | 同一响应的另一字段（见 `codar2api` 的 `spendable`；本次响应只回了 `agent_balance` ⇒ **需确认字段名**，见未决 #2） | 否则"Agent 余额 0 但其实能用"无法解释 ✓ |
| P1 | **本次/累计花费（`billing.cost_cny`、`billing.quota`）** | 每次 chat 响应 | 给"积分 ↔ 钱"的换算基准，避免"1713 看着很大其实 0.34 分"的误解 ✓ |
| P1 | **把积分折成 CNY 显示**（如 `1713 积分 ≈ ¥0.0034`） | 由 `billing.cost_cny/billing.quota` 的实测比率算 | 直接回答"这个数值对不对"的观感问题 ✓（比率需多模型采样，见未决 #1） |
| P2 | **签到/邀请/免费活动状态** | `/v1/daily-sign/status`、`/v1/promotion/info`、`/api/codar/activities/*` | 与 codearts 的 welfare 同构，可做"领取"按钮之一 ✓ |
| P2 | **余额冷却状态** | 402 / 消息含"余额不足" | 直接把上游语义映射到面板，避免"零余额还能发请求"的困惑 ✓ |

**一句话**：面板应主显 **`agent_balance`（积分）+ 通用余额**，并给出**折合金额**；`/api/user/self` 的 `quota` **不应**作为额度来源 ✗

---

## 附：补测与**修正**（本报告初稿的两处推断被推翻，如实更正）

初稿写完后又做了 3 次只读查询 + 3 次真实请求（累计 3/5 ✓），结论如下 —— **§0 的①②仍然成立，§2 的"积分≈金额"换算被推翻** ✗。

### A. `/v1/agent/balance` 的**完整**响应（初稿只打了前 300 字符）

```json
{"code":200,"message":"Success","data":{"username":"user_1508…","agent_balance":1712}}
```
⇒ `data` 只有 **`username` + `agent_balance`** 两个键 ✓ —— **没有**通用余额字段 ✗（⇒ 未决 #2 结论：通用余额**不在**这个端点，需另找 ✓）

### B. ★ 扣费实验（3 次请求，两个模型档位）

| 模型 | `rate_badge` | `pricing.input` | `billing.quota` | `billing.cost_cny` | **`agent_balance` 变化** |
|---|---|---|---|---|---|
| `glm-5.3-flash` | 0.20x | 2 | 91 | 0.0001816 | 1713 → **1712（−1）** |
| `glm-5.3-flash` | 0.20x | 2 | 62 | 0.0001248 | 1712 → **1711（−1）** |
| `claude-opus-5-5` | **1.20x** | 10 | **3300** | 0.0066 | 1711 → **1707（−4）** |

### C. 修正后的正确结论（★ 以此为准）

1. ★★★ **`agent_balance` 不是 token 数、不是金额、也不是"请求次数"** —— 它是**随模型成本档位扣 1~N 的粗粒度额度**：
   - 同一档（flash，0.20x）**每次 −1**；
   - 换到最贵档（opus，1.20x）**一次 −4**（其 `quota` 是 flash 的 36 倍，但只扣到 4 倍）
2. ⇒ ✗ **推翻初稿 §2 的"1713 ≈ 0.0034 元"**：那是把 `agent_balance` 当成了 `billing.quota` 的同一口径 ✗。
   实际 **1713 单位 ≈ 数百~上千次请求**（flash 档 1 单位/次；opus 档 4 单位/次）⇒ **是个不小的额度** ✓ —— 这也**符合用户"这数值不该看着像 0.3 分"的直觉** ✓
3. ⇒ **两个口径必须分开显示** ✓✓：
   - **额度**：`agent_balance`（单位：自定义"额度"，随模型档位递减 ✓）
   - **花费**：`billing.{quota, cost_cny}`（每次响应给 ✓，与 CNY 挂钩 ✓）
   - ⚠️ `billing.quota` 与 `agent_balance` 的**扣减比例不是常数**（91→1、3300→4）⇒ **不能用 quota 反推额度** ✗
4. **扣减是及时的** ✓：请求后**立即**读就已反映（+3s / +90s 复读不再变 ✓）
5. **模型目录里带价格表** ✓（`pricing.input`：flash=2 / opus=10 / glm=8 …；`rate_badge` 0.19x~1.20x）⇒ 可直接做成面板的"档位/倍率"列 ✓

### D. 更正后的显示建议（覆盖 §5 的 P1）

| 优先级 | 显示项 | 来源 |
|---|---|---|
| ★ P0 | **`agent_balance`**（+ 单位说明"额度"，**不要**直接折成 CNY） | `POST {PayBase}/v1/agent/balance` |
| ★ P0 | **每次响应的 `billing.quota` / `cost_cny`**（累计做成本列 ✓） | chat 响应 |
| P1 | **模型倍率 `rate_badge` / `pricing.input`** | `/v1/codar/models` |
| P1 | **通用余额**（`codar2api` 的 `balance.token`；本端点没有 ⇒ 需另找端点） | 未决 |
| ✗ 不要 | 用 `/api/user/self` 的 `quota` 当额度 | 实测恒 0 |
| ✗ 不要 | 把 `agent_balance` 按 `quota/cost_cny` 折算成钱 | 两者不同口径（本次实测推翻） |

### E. 补测后仍未决

| # | 未决 | 说明 |
|---|---|---|
| 1' | **扣减公式**：`−1 / −4` 与什么精确对应（模型档位？`pricing.input` 比值 10/2=5 vs 实测 4 ✗；还是四舍五入/上限？） | 只有 2 个档位样本 ⇒ 需更多模型各 1 次（剩余额度 2 次） |
| 2' | **通用余额端点**（本端点只回 `agent_balance`） | 找其它 PayBase 端点（`/v1/user/info` `/v1/token/*` 之类） |
| 3' | 同一模型**重复请求是否恒 −1**（已 2 次一致 ✓ 但样本小） | 再加几次同模型请求 |
| 4' | `agent_balance` 到 0 时的报错形态（402？） | 需把额度用完（不做 ✗） |
| 5' | **`/v1/codar/usage/daily?start_date=今天` 全 0**（而同日 3 次请求确实成功） | 面板若依赖它做用量统计就会永远显示 0 ⇒ 需查它的 `timezone` 字段、或把 `start_date` 往前挪一天重试 |
| 6' | **新账号 0 的最终归因**（`agent_balance` 真为 0？还是又读错端点？） | 需**领取免费模型/新人礼**或换一个新账号实测（会改状态 ⇒ 本次按约束未做 ✗，需批准） |
| 7' | `agent_balance` 的**官方单位叫法**（界面文案） | 需登录官方 UI 看（本次未登录、未截图 ✗） |

（补测同样遵守约束：未签到、未领取、未改代码、未打印 token ✓；真实请求累计 **3/5** ✓）


### 本次**没有**做的事（按约束）
未签到 ✓ 未领取 ✓ 未改代码 ✓ 未打印 token ✓ 真实请求 1 次（≤5）✓ 未 commit ✓

### 复现
```powershell
node D:\temp\t345-probe.mjs      # 读 config 的 token → 查两个余额端点 → 1 次 chat → 复盘
```
