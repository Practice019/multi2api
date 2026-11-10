# 报告：workbuddy 报错但不被禁用 —— 两个"死号"持续消耗请求

> **编者注（2026-11-10 补）**：**本报告的两个问题已修复**，见提交 `2c7ba73`（空体 403 误判 + 工具循环丢错误类别 + 换上游空转）。修复方式与本报告 §7 的建议 1 一致：`internal/workbuddy/errorclassifier.go` 对「403 + 完全空体」偏离 `upstream.Classify`，判为 `ErrKindServer`（进熔断 + 禁用两条线），并在注释里引用了本报告的实测数字（2 个号连续 8 天 0 成功、3.5 天 76 次 503、`last_err` 为零值）。


- **报告日期**：2026-11-10
- **触发**：用户报「针对 workbuddy 模型报错但是没有禁用的问题」
- **状态**：仅调研，**未改任何代码**
- **数据窗口**：`data/request-log.jsonl`（2026-11-07 07:27 → 2026-11-10 10:38，共 55300 条）

---

## 0. 摘要（一句话）

**workbuddy 有 2 个账号已经"死"了至少 8 天**（日志窗口内**成功 0 次**），但它们
`cooling=false`、`disabled=false`、`breaker_fails=0`、`last_err` **为零值** ——
在管理台上**看起来完全健康**，因此每次都被正常选中，每次都以 503 收场。

根因不是"禁用阈值没到"，而是**这条错误根本没被记进任何计数器**：

```
workbuddy 403（响应体为空）→ 分类为 ErrKindClient
  → applyErrorPolicy 的 default 分支「只换号不罚」
  → 不调 NoteError / 不冷却 / 不喂自动禁用线
  → 账号状态一个字节都不变
```

**对照组（决定性）**：同样是 HTTP 403，

| 上游 | 响应体 | 分类 | 会禁用吗 |
|---|---|---|---|
| **codar** | `{"detail":{"code":"account_restricted","message":"API access is restricted…"}}` | `server` | ✅ 会（5 次连败后摘号） |
| **workbuddy** | **空字符串** | `client` | ❌ **永不** |

差别**只在响应体是否为空**。

---

## 1. 现象与量化

### 1.1 客户端可见的影响

| 项 | 数值 |
|---|---|
| workbuddy 请求总数 | 6810 |
| 返回 200 | 6734 |
| **返回 503** | **76（1.1%）** |
| 其中今日（11-10）实测到 `upstream_err` 的 | 18 次 |

按天分布：

```
2026-11-07 : 16 次 503
2026-11-08 :  1 次 503
2026-11-09 : 38 次 503
2026-11-10 : 21 次 503
```

### 1.2 失败集中在 2 个账号

| uid（截断） | 日志窗口内成功 | 失败次数 | 池内 `last_success` |
|---|---|---|---|
| `7285efdf-1eaa-4ffd-…` | **0** | 21 | 2026-11-02（**8 天前**） |
| `4c434e8d-f7a8-42b5-…` | **0** | 16 | 2026-11-02（**8 天前**） |

另外 4 个账号的失败（16/12/6/5 次）**全部是 429 限流**，它们已经被正确地软冷却
（`cooling=true, reason="429 rate limit"`）—— **那部分是健康行为，不在本报告的批评范围**。

### 1.3 为什么"看起来完全健康"（观测陷阱）

那 2 个号的完整池内状态：

```json
{
  "uid": "4c434e8d-f7a8-42b5-809d-bc92bc2fe14e",
  "credits": 1597,                      // ← 有额度，所以不会被"额度为 0"规则跳过
  "cooling": false,                     // ← 从不冷却
  "disabled": false,                    // ← 从不禁用
  "success_count": 942,
  "last_success": "2026-11-02T03:29:31", // ← 8 天没成功过
  "last_err": "0001-01-01T00:00:00Z",    // ← ★ 零值！从没记过一次错
  "breaker_fails": 0,                    // ← 熔断计数也是 0
  "breaker_until": "0001-01-01T00:00:00Z",
  "last_used": "2026-11-10T10:36:25",    // ← 但今天还在被选中
  "today_checkin": "already"             // ← 签到正常（签到线也救不了它）
}
```

**`last_err` 是零值是最刺眼的一处**：它意味着 `NoteError` 从未被调用。
在管理台上，这个号与一个刚导入的健康号**看不出任何区别** ——
`success_count=942` 还是历史累计，无法反映"最近 8 天 0 成功"。

### 1.4 ★ 决定性对照：同一个上游内部就分成两类（2026-11-10 10:40 快照）

workbuddy 全部 6 个号的实际状态：

| uid（截断） | cooling | disabled | `breaker_fails` | **`last_err`** | `last_success` | 判定 |
|---|---|---|---|---|---|---|
| `0234c548-963c` | True | False | 0 | 2026-11-03 | **2026-11-10** | ✅ 健康（429 冷却中） |
| `2e37e4f4-0e23` | True | False | 0 | 2026-11-03 | **2026-11-09** | ✅ 健康（429 冷却中） |
| `4e0fe0e9-e804` | False | False | 0 | 2026-11-03 | **2026-11-10** | ✅ 健康 |
| `ca19abfd-e68e` | False | False | 0 | 2026-11-03 | **2026-11-10** | ✅ 健康 |
| **`4c434e8d-f7a8`** | False | False | **0** | **`0001-01-01`（零值）** | **2026-11-02** | ❌ **死号（8 天 0 成功）** |
| **`7285efdf-1eaa`** | False | False | **0** | **`0001-01-01`（零值）** | **2026-11-02** | ❌ **死号（8 天 0 成功）** |

**这张表就是本报告的完整证明**：

- 上面 4 个号：`last_err` **有值**（2026-11-03，来自 429 限流），`last_success` 是**今天** →
  它们正常在干活，429 也被正确记账并冷却
- 下面 2 个号：`last_err` 是**零值** —— **一次错误都没被记过**，而 `last_success` 停在 8 天前

**同一个上游、同一个池、同一天，4 个号正常记账、2 个号完全无记录。**
差别不在"阈值没到"，而在**那 2 个号的错误从未进入任何计数器**
（`last_err` 零值 + `breaker_fails=0` 是同一件事的两面：`NoteError` 没被调用过）。

而 `disabled` 这一列** 6 个号全是 False** —— 也就是说，即使上面 4 个健康号
将来开始持续 403，它们也会走上同一条路：静默失败、永不禁用、8 天后依然"健康"。

---

## 2. 根因链（逐段核实）

### 2.1 第一段：403 响应体为空

```
upstream client (http 403):        ← 冒号后面什么都没有
```

逐字节核实（`upstream_err` 字段原文，注意引号内的空尾）：

```
seq=…  provider=workbuddy  'upstream client (http 403): '
```

### 2.2 第二段：空体 403 被分类为 `ErrClient`

`internal/upstream/client.go` 的 `Classify(status, body)` 按顺序判：

```go
for _, m := range hardMarkers       { ... }   // 空体 → 不命中
for _, m := range sessionDeadMarkers{ ... }   // 空体 → 不命中
for _, m := range contentBlockedMarkers { ...}
for _, m := range fatalClientMarkers { ... }
if status == 429 { return ErrSoftRate }       // 403 ≠ 429
if status == 404 { return ErrNotFound }       // 403 ≠ 404
if status >= 500 { return ErrServer }         // 403 < 500
if status >= 400 { return ErrClient }         // ★ 落在这里
```

**所有判据都基于响应体内容**（markers）。响应体为空 ⇒ 全部不命中 ⇒ 一路落到
状态码兜底 ⇒ `ErrClient`。

workbuddy 只是把它翻译成 `gateway.ErrKindClient`：

```go
// internal/workbuddy/errorclassifier.go
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	return toGatewayKind(upstream.Classify(status, body))
}
```

### 2.3 第三段：`ErrKindClient` 不喂任何计数器

`internal/server/handler.go` 的 `applyErrorPolicy(uid, kind)`：

```go
switch kind {
case gateway.ErrKindSessionDead, gateway.ErrKindNotFound, gateway.ErrKindServer:
	if h.cfg.Pool.RecordAutoDisableFailure(uid, "upstream "+kind.String()) { ... }  // 禁用线
}
switch kind {
case gateway.ErrKindHardCredit: ...
case gateway.ErrKindSoftRate:   ... Cooldown(CoolSoft, ...) ...
case gateway.ErrKindSessionDead:  ... NoteSessionDead ...
case gateway.ErrKindNotFound:     ... Cooldown(CoolSoft, ...) ...
case gateway.ErrKindServer:       h.cfg.Pool.NoteError(uid)      // ★ 只有这里喂熔断
default:
	// 其余（ErrKindAuth/ErrKindClient/ErrKindNone）：只换号不罚（防雪崩），不喂熔断。
}
```

`ErrKindClient` → `default` → **什么都不做**。

于是：
- `NoteError` 没被调用 ⇒ `last_err` 保持零值、`e.fails` 保持 0
- `Cooldown` 没被调用 ⇒ `cooling=false`
- `RecordAutoDisableFailure` 没被调用 ⇒ 禁用线计数不增长

**失败被完全"蒸发"了。**

### 2.4 第四段：于是它一直留在候选集里

失败只做两件事（`fail()`）：

```go
fail := func(uid string, reason string) {
	...
	releaseHeld()                       // 释放在途租约
	if stickyUID != "" && uid == stickyUID { h.cfg.Session.Unbind(sessKey) }  // 解绑粘性
}
```

**都不改变账号可用性状态** ⇒ 下一轮 `Pick` 仍然会选中它。

实测每次失败请求的判决：

```
provider_breaker:system                              attempts=3 spentMs=1719 last=workbuddy client 403
switched_upstream:workbuddy                          attempts=3 spentMs=637  last=workbuddy client 403
pool_unavailable:global                              attempts=3 spentMs=1522 last=workbuddy client 403
switched_upstream_after_rotate_exhausted:workbuddy   attempts=3 spentMs=2741 last=workbuddy client 403
```

**`attempts=3` 是 18 条 403 记录的**固定值** —— 每次都把所有候选号试一遍，
全部失败后返回 503。**

---

## 3. 这是 bug 还是设计？（诚实区分）

**必须承认：`ErrClient` 不计错、不罚号，是有意的设计，而且理由充分。**

`applyErrorPolicy` 的注释里写着这条设计依据（引用了两次真实报障的历史）：

> 后三种是"换号就能好，但号本身没毛病"（凭证边界、请求问题、分类未知）——
> 把它们计进去会让**一次客户端发错参数的行为，在连发 5 次后禁掉一个健康号**。

这个方向是**对的**。历史教训也支持它：
- 429 曾被计入禁用线 ⇒ 「余额充足的号突然消失」（已修）
- HardCredit 曾被计入禁用线 ⇒ 「第二天额度恢复、账号还是禁用」（已修）

**但本报告指出的是一个真实缺口**：上面那句注释的**前提**是
"客户端发错参数"（一次性的、请求侧的问题）。而 workbuddy 的 403 空体
**不是请求侧问题** —— 它是**账号级**的持续拒绝：

| 判据 | 客户端参数错误 | workbuddy 的 403 空体 |
|---|---|---|
| 同一请求换个账号能成功吗 | ✅ 能（也可能都不行，但错在请求） | ❌ 换到好号能成功（另 4 个号能返回 200） |
| 这个号每次都失败吗 | ❌ 只在这类请求上失败 | ✅ **8 天 0 成功** |
| 是账号自身状态吗 | ❌ 不是 | ✅ 是（该号被上游拒绝） |
| 会自己好吗 | — | ❌ **不会**（8 天了） |

**结论：`ErrClient` 这个分类对"空体 403"是错的。** 403 的语义是
"Forbidden（禁止访问）"，在 AI 上游里最常见的原因正是**账号被封/被限制/凭证被拒** ——
那是账号级、不自愈的状态，与"请求参数写错"（400）在语义上完全不同。

---

## 4. 三个具体缺口（可以分别修，本报告只列不修）

### 缺口 A（根因）：空体 403 被误判为 `ErrClient`

`Classify` 的所有判据都基于响应体 marker，**空体时没有任何判据能区分 403 的语义**，
于是落到"状态码 ≥400"的兜底。

**对比证据**（同一天、同一批、同为 403）：

```
codar     403 + {"detail":{"code":"account_restricted",…}}  →  ErrServer → 会禁用 ✅
workbuddy 403 + （空）                                        →  ErrClient → 永不禁用 ❌
```

建议方向（三选一，按保守程度排序）：
1. 把「403 + 空体（或无法识别内容）」判为 `ErrServer` 或独立的"账号被拒"类 ——
   保守版：**不给 403 空体发 `ErrClient`**，至少换成 `ErrServer`（进禁用线 + 熔断线）。
2. 新增 `ErrKindAccountRejected`，动作 = 复用 SessionDead 的门控（连续 N 次才摘）。
3. 若担心误伤，最少也要**喂熔断线**（`NoteError`）—— 让账号进入短暂歇息，
   而不是完全无状态。

### 缺口 B：没有任何"长期低成功率"的兜底

现在的自动禁用**只看"最近连续失败"**（阈值 5，`DefaultAutoDisableThreshold`），
对"**持续失败但偶尔夹杂成功**"或"从某天起再没成功过"的号**没有判据**。

以本报告的号为例：`last_success = 11-02`，距今 8 天，
但"连续失败"计数会因为**偶尔被选中一次却没被记账**（见缺口 A）而永远是 0。

建议方向：加一条**独立**判据 ——
「最近 N 小时被选中 M 次（M ≥ 某阈值）且成功率为 0 ⇒ 摘号/告警」。
它与现有的两条线（熔断 3 次、禁用 5 次）判据不同，不会互相干扰。

### 缺口 C：自动禁用线的唯一"非 chat"入口是签到，而签到这条号是正常的

`RecordAutoDisableFailure` 在 chat 路径之外只有一个调用者：

```
internal/scheduler/dailycheckin_disable.go:90
	rec.RecordAutoDisableFailure(r.UID, "签到凭据失效: "+r.Detail)
```

而这两个号的 `today_checkin = "already"` —— **签到是正常的**。
也就是说：**唯一一条能摘掉它们的线也不适用**，因为坏的不是凭证，是"上游拒绝这个号的对话请求"。

---

## 5. 为什么这次能查到（上一轮日志改造的直接收益）

这个 bug **此前是不可诊断的**：

- 旧日志只有 `status=503`，没有上游原文 ⇒ 不知道是 403、429 还是超时
- 新加的 `upstream_err` 字段第一眼就给出 **`upstream client (http 403): `（空体）**
- 配合 `upstream_req` 可确认"我们发出去的请求是正常的"（对比 codar 那次的 tools 问题）
- 管理台账号列表的 `last_err` 零值 + `breaker_fails=0` 直接指向"计数没被喂"

**这正是上一轮把「非 200 记录上游原文」补上的价值**：从"猜 403 是什么"
变成"3 秒看出是空体 403，且分类成了 client"。

---

## 6. 影响评估

| 维度 | 影响 |
|---|---|
| 用户可见 | 3.5 天 **76 次 503**（1.1% 失败率），且**集中在 workbuddy 一个上游** |
| 单次代价 | 每次浪费 **3 次上游往返**（attempts=3），实测 `spentMs` **637–2741 ms** |
| 产能 | 2 个号（credits 1597 / 325）**完全无法产生有效输出**，却持续占用选号与在途名额 |
| 雪崩风险 | 失败号占候选集比例高时（6 个号里 4 个在 429 冷却 + 2 个死号），`pool_unavailable:global` 必然出现 |
| 运维可观测性 | ⚠ **最严重**：账号在台上显示为健康（`last_err` 零值、`breaker_fails=0`），**没有任何线索指向真因** |
| 自愈性 | ❌ 不会自愈（8 天 0 成功），必须人工发现并处理 |

---

## 7. 建议（均未实施）

按性价比排序：

1. **补缺口 A（根因）**：让「403 + 空体」不再落入 `ErrClient`。
   最小改动是把它归到 `ErrServer`（进熔断 + 禁用两条线），
   更精确的做法是新增"账号被拒"类，动作对齐 SessionDead 的门控。
   **这一个是治本的。**

2. **补缺口 B（兜底）**：加"最近 N 小时 0 成功率"的独立判据 + 告警。
   作用是覆盖"分类器没预料到的形态"—— 本次就是这类。

3. **诊断信息**：`upstream client (http 403): ` 后面空着，看不出响应头。
   建议在空体时补记响应头里的**关键几条**（`server` / `cf-ray` / `x-request-id`），
   用来区分"边缘/WAF 拒绝"与"应用层拒绝"。

4. **人工处理（立即）**：把 `7285efdf-…` 与 `4c434e8d-…` 两个号在控制台**禁用**
   （8 天 0 成功，已无可用性），观察 workbuddy 的 503 是否归零。

5. **观测面板**：「最近成功时间」应当成为账号列表的一列（或告警项）——
   本报告的所有结论都源自这一个字段，而它目前在台账上不显眼。

---

## 8. 附录：复检命令

```bash
# 1) 看 workbuddy 的失败与上游原文
python3 - <<'EOF'
import json
rows=[json.loads(l) for l in open('data/request-log.jsonl')]
for d in rows:
    if d.get('provider')=='workbuddy' and d.get('status',200)>=400:
        print(d['seq'], d['at'][:19], d['status'], repr(d.get('upstream_err')))
EOF

# 2) 看某账号是否"长期 0 成功"
python3 - <<'EOF'
import json
rows=[json.loads(l) for l in open('data/request-log.jsonl')]
for uid in ('7285efdf-1eaa-4ffd-a85f-f028c17d7f3e','4c434e8d-f7a8-42b5-809d-bc92bc2fe14e'):
    ok=[d for d in rows if d.get('uid')==uid and d.get('status')==200]
    print(uid[:13], '成功', len(ok), '最后一次', ok[-1]['at'][:19] if ok else '无')
EOF

# 3) 看池内状态（关键：last_err 是否为零值）
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://127.0.0.1:7863/admin/accounts | python3 -c "
import json,sys
for a in json.load(sys.stdin)['accounts']:
    if a['provider']=='workbuddy':
        print(a['uid'][:13], 'cool=',a['cooling'], 'dis=',a['disabled'],
              'breaker=',a['breaker_fails'], 'last_err=',a['last_err'], 'last_success=',a['last_success'])"

# 4) 对照：codar 的 403 有体、被正确分类（同一日志）
python3 -c "
import json
for l in open('data/request-log.jsonl'):
    d=json.loads(l)
    if d.get('provider')=='codar' and '403' in str(d.get('upstream_err')):
        print(repr(d['upstream_err'])[:160]); break"
```

---

## 9. 本报告未做的事

- ❌ 未修改任何代码
- ❌ 未修改任何配置
- ❌ 未禁用任何账号（建议 4 需你确认后执行）
- ✅ 只读取了 `data/request-log.jsonl`、`/admin/accounts` 与源码
