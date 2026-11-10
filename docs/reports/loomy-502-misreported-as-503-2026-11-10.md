# 报告：loomy 的上游 502 被误报成「503 无可用账号」+ 空转切换导致重试翻倍

> **编者注（2026-11-10 补）**：**本报告的两个根因已修复**，见提交 `2c7ba73`。① 工具循环的失败分支补上了 `lastKind = gateway.ErrKindServer`（`handler.go`）；② `switchedFrom` 初始化时登记当前上游 （`switchedFrom := map[string]bool{reqProvider: true}`），空转切换消失；并新增源码级守卫 `internal/server/chat_loop_parity_guard_test.go`（对应本报告 §7 的建议 3）。部署后实测：空转切换 **0 次**（修复前 111 次）。


- **报告日期**：2026-11-10
- **触发**：用户反馈「有一些账号好像死了但是调用的时候还是会一直请求导致重试延迟：3741 毫秒」并贴出
  `503 no_healthy_account ... 137 个账号：9 禁用、0 冷却、3 个在途未结束（最大 1）: upstream server (http 502)`
- **状态**：仅调研，**未改任何代码**
- **数据窗口**：`data/request-log.jsonl`（2026-11-07 07:27 → 2026-11-10，共 55300 条）+ 容器日志

---

## 0. 摘要（两个真 bug，一条完整因果链）

用户看到的那条报错**每一个字段都能被下面的链条逐字解释**：

```
loomy 请求 → 走工具循环（每个请求都走，见 §2.3）
      ↓
上游边缘（讯飞）返回 502
      ↓
① 工具循环的失败路径只设 lastErr、**不设 lastKind**
   ⇒ lastKind 停在零值 ErrKindNone
      ↓
换号预算用尽 → ② nextAliasUpstream 返回 **loomy 自己**
   ⇒ 空转切换 → goto 重来一轮 → **上游调用从 3 次变 6 次**（用户说的"一直请求"）
      ↓
仍失败 → 按 lastKind(=None) 判决：
   · status = 503                （本该 502）
   · code   = no_healthy_account （本该 upstream_error）
   · msg    = all accounts unavailable (cooling/disabled)  ← 用户看到的原文
   · 再拼上池子提示「137 个账号：9 禁用、0 冷却、3 个在途未结束」← 自相矛盾
   · 再拼上最后错误「: upstream server (http 502)」          ← 唯一的真话
```

**量化**：

| 指标 | 数值 |
|---|---|
| loomy 失败总数 | 326 |
| 其中「上游 502 却回 503」 | **296（91%）** |
| **loomy 返回过的 502 次数** | **0**（326 次失败全是 503） |
| 空转切换（切到自己） | **111 次 / 124 次切换 = 89.5%** |
| 其中 loomy 的 `kind=none` 空转 | 79 次 |

---

## 1. 与"账号死了"无关（先排除）

用户的直觉是「账号好像死了」。实测**不成立**：

| 检查 | 结果 |
|---|---|
| loomy 中"被请求过但 0 成功"的账号 | **0 个** |
| 失败是否集中在少数号 | ❌ 失败记录里 **每个号都不同**（10:39 那批 26 条覆盖 26 个不同 uid） |
| 失败的号是否真的坏 | ❌ 同一个号在**几十秒后**成功（例：`26110215112377` 10:39:44 失败 → 10:40:59 成功） |
| 并发是否过高 | ❌ 失败时并发 6-8，成功时 1-2 |
| 41 个"零额度"号 | ✅ **符合设计**：loomy 的 `balance` 恒为 0，额度全在 `dailyBalance`；这 41 个是**今日免费额度用完**，次日自动回到池子（见 §5） |

**⇒ 不是账号问题，是"上游边缘 502"被错误归因到了账号池。**

---

## 2. 根因 A：工具循环不设 `lastKind` ⇒ 502 被误报成 503

### 2.1 证据：同一个日志字段，loomy 是 `kind=none`，别家是正常值

容器日志里"用完换号预算"那行会打出当时的 `lastKind`：

```
loomy      ：用完换号预算（3 次）仍失败（kind=none），切到 "loomy"
workbuddy  ：用完换号预算（3 次）仍失败（kind=client），切到 "monkeycode"
workbuddy  ：用完换号预算（3 次）仍失败（kind=soft_rate），切到 "workbuddy"
```

`kind=none` = `ErrKindNone` = **零值**。而同一时刻的 `decision` 里明明写着
`last=loomy server 502` —— 两个字段互相矛盾，说明**记录"最后错误"和"错误类别"的两条路径不同步**。

### 2.2 代码：工具循环的失败路径少了 `lastKind`

`internal/server/handler.go`，工具循环的硬失败分支：

```go
// 循环内的硬失败（注入失败/上游连续报错）：
// 分类后按换号处理，与普通 chat 路径同一条。
lastErr = werr
fail(acct.UID, failBrief(reqProvider, gateway.ErrKindServer, werr))
continue            // ★ 没有 lastKind = ... ！
```

对照**普通 chat 路径**（共 8 处都有）：

```
handler.go:2620    lastKind = gateway.ErrKindClient
handler.go:2642    lastKind = gateway.ErrKindFatalClient
handler.go:2669    lastKind = kind
handler.go:2717    lastKind = kind
handler.go:2783    lastKind = gateway.ErrKindFatalClient
handler.go:2865    lastKind = kind
handler.go:2901    lastKind = kind
```

**"与普通 chat 路径同一条"的注释与实现不符** —— 注释说同一条，实际漏了最关键的那个赋值。

### 2.3 为什么只有 loomy 中招

`internal/server/chattool.go`：

```go
func (h *Handler) shouldRunToolLoop(providerID string, body []byte) (toolCapable, bool) {
	if h.cfg.DisableChatTools { return nil, false }
	tc, ok := h.toolCapabilityOf()
	if !ok { return nil, false }
	if len(h.chatToolsOf(providerID)) == 0 { return nil, false }
	return tc, true        // ★ body 参数**从未被使用**
}
```

判据里**没有"客户端是否带了工具"这一条** —— 只要该上游声明了 `ChatToolExt`，**每个请求都走工具循环**。

loomy 声明了 2 个工具（`internal/loomy/chattool.go`：`generate_image` / `web_search`），
`config.json` 里 `disable_chat_tools` 未设置（默认 false）
⇒ **loomy 的 100% 请求都走工具循环** ⇒ **100% 的失败都丢掉 kind**。

### 2.4 后果：状态码、错误码、文案全错

`internal/server/handler.go` 收尾处（注释自己就写着这个 bug 的名字）：

```go
//	上游 5xx（服务端故障） → 502 upstream_error：**客户端应该重试**，
//	  账号本身没问题（实测：讯飞网关抖 502，池里 11 个号全健康，
//	  旧行为却回 503 no_healthy_account，把"上游抖"报成"账号不可用"）。
msg := "all accounts unavailable (cooling/disabled)"
if lastKind == gateway.ErrKindServer {
	msg = "上游服务异常（已自动轮换多个账号重试，仍失败）"
}
...
code := gateway.ErrorCodeForKind(lastKind)
switch lastKind {
case gateway.ErrKindNone, gateway.ErrKindSessionDead, gateway.ErrKindAuth:
	code = gateway.CodeNoHealthyAccount      // ← "no_healthy_account"
}
```

`lastKind = ErrKindNone` 时三条全落错分支 ⇒ **用户看到的 `503 no_healthy_account` + "all accounts unavailable" 就是这么来的**。

**这段注释描述的正是用户现在报的这个现象（讯飞 502、池里号全健康）** ——
说明该 bug 此前被修过一次，但修法依赖 `lastKind`，而**工具循环这条路径绕过了那个修法**。

---

## 3. 根因 B：空转切换 ⇒ 上游调用从 3 次变 6 次（"一直请求"）

### 3.1 证据

```
用完换号预算（3 次）仍失败（kind=none），切到 "loomy"（模型 "loomy/DeepSeek-V4.1-Flash"）
                                    ↑ 上游本来就是 loomy
```

统计：**111 次切到自己 / 124 次切换 = 89.5%**（loomy 79 次，workbuddy 32 次）。

### 3.2 代码：`nextAliasUpstream` 不排除"当前上游"

`internal/server/apikey_models.go`：

```go
func (h *Handler) nextAliasUpstream(order []string, tried map[string]bool) (string, string) {
	for _, m := range order {
		prov, _, has := gateway.SplitModel(strings.TrimSpace(m))
		if !has || prov == "" { continue }
		if tried != nil && tried[prov] { continue }        // ← tried 里没有"当前上游"
		if !h.cfg.Provider.Has(prov) { continue }
		if len(h.cfg.Pool.AvailableUIDsFor(prov)) == 0 { continue }
		return prov, m                                     // ★ 于是返回自己
	}
	return "", ""
}
```

而调用方初始化时 **`switchedFrom` 是空 map**：

```go
switchedFrom := map[string]bool{}          // ← 当前上游没有登记进去
...
if next, nextModel := h.nextAliasUpstream(aliasOrder, switchedFrom); next != "" {
	switchedFrom[next] = true
	reqProvider = next
	...
	st.decision = "switched_upstream_after_rotate_exhausted:" + next
	goto retryUpstream                     // ★ 整个 3 次循环重跑一遍
}
```

于是：当前在 loomy、alias 候选里第一个又是 `loomy/DeepSeek-V4.1-Flash`、
loomy 有 94 个可用号（`AvailableUIDsFor > 0`）⇒ **判定"换到 loomy"** ⇒ 空转一轮。

### 3.3 代价

| 项 | 值 |
|---|---|
| 每轮尝试数 | 3（`attempts=3`） |
| 空转后总上游调用 | **6 次**（日志里的 `attempts` 会因 `goto` 重置，所以**只显示最后一轮的 3** —— 真实次数被掩盖） |
| 日志可见性 | `attempts = i + 1` 在每轮开头重算 ⇒ **失败请求的真实尝试次数在日志里看不出来** |

**这正是用户说的"一直请求"**：一次客户端请求，网关把同一家上游、同一批号打了 6 遍。

---

## 4. 附带发现（同一路径上的另两个缺口）

### 4.1 用户报的"重试延迟"在日志里对不上号

用户说 3741ms。实测：

- loomy 的 **503 里没有一条**在 3400–4100ms 区间（分布：0-999ms 187 条、1-2s 19 条、2-3s 1 条）
- 3.5–3.9s 的 loomy 请求**全部是 200 成功**（seq 326913=3710ms、326968=3578ms、326825=3800ms…）

⇒ 用户看到的 3741ms 很可能是**客户端侧的总等待**（含它自己的重试退避），
而网关侧每次失败只花 0.2–2s。**结论：服务端没有 3.7s 的单次失败，延迟来自重试链的叠加。**

### 4.2 那条报错信息本身自相矛盾（第三个独立问题）

```
all accounts unavailable (cooling/disabled) —— 137 个账号：9 禁用、0 冷却、3 个在途未结束（最大 1）
```

`poolUnavailableHintOf` 只统计 3 个维度：

```go
switch {
case st.Disabled: nDis++
case st.Cooling:  nCool++          // ← Cooling = now.Before(until) || now.Before(breakerUntil)
}
if st.InFlight > 0 { nBusy++ }
```

**没有"额度已用完被跳过"这一维** —— 而当时 41 个号正是因为 `creditsExhausted` 被跳过。
读者看到「9 禁用、0 冷却」会以为还剩 125 个可用号，只能往"池子坏了"猜。

这与本仓既有的那次事故（"44 个账号：0 禁用、0 冷却" → 真因是在途占满）**是同一个坑**：
报错没给出真正的那一维。

### 4.3 我自己上一轮改动的缺口（一并如实记录）

`upstream_req`（出站请求体）在 loomy 的失败记录里**全是空的**：

```
08:20 之后失败 523 条，带 upstream_req 的 47 条 —— 全部是 codar(14) / workbuddy(33)
loomy: 0 条
```

原因：`st.upstreamReqRef = outBody` 只写在普通 chat 路径的 `chatVia` 之前，
而 loomy 走工具循环（§2.3）⇒ 这条路径没赋值。
**这是上一轮"失败记出站请求体"改动的漏点，与根因 A 同一条路径。**

---

## 5. 顺带澄清：41 个"零额度"号不是 bug（用户已确认）

loomy 的额度结构（真机探测结果）：

```
零额度号: {"data":{"balance":0,"dailyBalance":0,   "availableBalance":0}}
有额度号: {"data":{"balance":0,"dailyBalance":4712,"availableBalance":4712}}
```

**`balance`（永久积分）对所有号都是 0**，额度全在 `dailyBalance`（每日赠送）。
41 个号是**今日免费额度用完**，符合用户自己定的规则
（「免费额度为 0 就从轮询池出去，池子里都没了才动永久额度」），
**次日自然日重置后自动回到池子**。这 41 个号全部有成功记录（最多 653 次），确认健康。

⇒ 「137 个号只有 94 个可用」= 96（有免费额度）− 2（被我禁用的失效号），**符合设计**。

---

## 6. 影响评估

| 维度 | 影响 |
|---|---|
| **错误归因** | 91% 的 loomy 失败被报成"账号池不可用"，而真因是上游边缘 502 —— 运维会去查错的地方 |
| **客户端行为** | 503 的语义是"重试无用，去改池子"，而 502 是"该重试"。客户端据此做的退避/重试策略全错 |
| **重试开销** | 89.5% 的切换是空转，每次失败多打 3 次上游（3→6），放大上游压力与自身耗时 |
| **观测** | `kind=none` 掩盖了错误类别；`attempts` 因 `goto` 重置而**少报一半**；loomy 失败无 `upstream_req` |
| **自愈性** | ❌ 上游边缘抖动会一直复现；且**每一次抖动都被记成池子故障**，噪声盖住真故障 |
| 涉及上游 | **loomy 最重**（工具循环 + kind=none）；**workbuddy 也有空转切换**（32 次） |

---

## 7. 建议（均未实施）

按性价比排序：

1. **补根因 A**：工具循环的失败分支补 `lastKind`
   （该分支已经把 `ErrKindServer` 写进了 `failBrief`，类别是现成的）。
   修完立刻效果：状态码回 502、`code` 回 `upstream_error`、决策回 `upstream_5xx:system`，
   文案变成"上游服务异常"——**这正是那段注释当初想达到的效果**。

2. **补根因 B**：`switchedFrom` 初始化时把当前上游登记进去
   （或在 `nextAliasUpstream` 里显式跳过"当前 reqProvider"）。
   修完效果：切上游不再是空转，省掉一次 3 连打；日志里的 `attempts` 也能反映真实次数。

3. **断言/守卫**：给"工具循环路径必须与普通路径同源"加一条**源码级守卫测试**
   （本仓已有同款：`TestAllProviderChatsAreArmored`、`alias_health_test.go` 的源码断言），
   钉住"两条路径都必须设置 lastKind"。

4. **错误提示补维度**：`poolUnavailableHintOf` 增加"额度已用完被跳过"的计数。

5. **补我上一轮的漏点**：工具循环路径也记 `upstreamReqRef`（否则 loomy 的失败永远看不到请求体）。

6. **日志**：`attempts` 改成**累计**（或在 decision 里额外记 `rounds=N`），
   否则"空转了一轮"这件事在日志里不可见。

---

## 8. 附录：复检命令

```bash
# 1) loomy 的 502 被误报成 503（应输出 296 / 326）
python3 - <<'EOF'
import json
rows=[json.loads(l) for l in open('data/request-log.jsonl')]
lo=[d for d in rows if d.get('provider')=='loomy' and (d.get('status') or 0)>=400]
mis=[d for d in lo if ' 502' in str(d.get('decision')) and d.get('status')==503]
print('loomy 失败', len(lo), '其中 502→503 误报', len(mis))
print('loomy 返回过的 502 次数:', len([d for d in lo if d.get('status')==502]))
EOF

# 2) 空转切换（切到自己）—— 看 kind= 是否为 none
docker logs workbuddy2api 2>&1 | grep "用完换号预算" | grep '"loomy"' | tail -3

# 3) 工具循环是否恒定触发（body 参数未被使用）
grep -n -A12 "func (h \*Handler) shouldRunToolLoop" internal/server/chattool.go

# 4) 两条路径的 lastKind 对照（普通 8 处 vs 工具循环 0 处）
grep -n "lastKind = " internal/server/handler.go
sed -n '/循环内的硬失败/,/continue/p' internal/server/handler.go

# 5) loomy 失败是否缺 upstream_req
python3 -c "
import json
rows=[json.loads(l) for l in open('data/request-log.jsonl')]
lo=[d for d in rows if d.get('provider')=='loomy' and (d.get('status') or 0)>=400]
print('loomy 失败', len(lo), '带 upstream_req 的', len([d for d in lo if d.get('upstream_req')]))"
```

---

## 9. 本报告未做的事

- ❌ 未修改任何代码
- ❌ 未修改任何配置
- ❌ 未禁用/启用任何账号
- ✅ 只读取了 `data/request-log.jsonl`、容器日志、`/admin/accounts` 与源码
- ✅ 对上游只做了少量只读探测（额度查询 4 次 + 3 次对话探测，用于排除"工具导致 502"的假设）
