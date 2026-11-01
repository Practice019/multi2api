// adminrenew.go 批量续期端点：POST /admin/monkeycode/renew。
//
// # 为什么需要它（用户报的问题）
//
// monkeycode 的 session 是 **30 天硬过期**，界面上「Token 到期」列只能提前预警
// （见 expiry.go：那是个**推断值**）。到期之后原本只有两条路：重新走 GitHub OAuth，
// 或者从浏览器 DevTools 复制一份新 session 贴进来。
//
// 但账号导入时**已经把 email + password 存在凭证文件里**了，于是可以拿它们走
// `POST /api/v1/users/password-login`（配 cap.js PoW）自己换一份**全新 session**：
// 实测响应 `Set-Cookie` 带 `Max-Age=2592000`，也就是服务端亲口告知的 30 天
// —— 比 expiry.go 那个"锚点 + 30 天"的推断值可信。
//
// 本文件把那件事变成一个端点（前端「续期」按钮 POST 它）。PoW 登录本身在 renew.go。
//
// # 三条硬语义（都是 Node 版真机踩出来的，不是设计洁癖）
//
//  1. `uids` 为空 = 全部。但**必须跳过**两类号，否则会出现两个具体故障：
//
//     · 缺 email/password → 后端没有可用的登录材料，只会一直失败重试
//     → `skipped:true, reason:"no-credentials"`
//     · 24 小时内刚续过 → 同一批号被反复重续（PoW 白烧 CPU + 在上游堆无效登录）
//     → `skipped:true, reason:"recently-renewed"`，`force:true` 可无视这个窗口
//
//     Node 版实测（不跳过时）：连着 5 批调用**每批都返回 remaining=15**，
//     前端"还剩 15 个"永不收敛，95 次调用只为 33 个号 —— 用户看到的是转圈不停。
//
//  2. `remaining` = 「本轮之后**仍未处理完的候选数**」，必须能递减到 0。
//
//     skipped 的号**永远不计入**（它们不是候选：跳过的语义是"不需要/不能做"，
//     把它算进 remaining 就等于把上一段那个死循环原样搬过来）。
//     ⚠ 由此推出一个必要的补充：`remaining > 0` 只在**本轮被切掉一部分候选**时出现，
//     所以 body 额外接受一个可选 `limit`（本轮最多处理几个候选；0/缺省 = 一次做完）。
//     没有 limit 的话 remaining 恒为 0，"递减到 0"这件事根本无法表达。
//
//  3. **单号失败不中断整批**：每号独立记录错误，其余照跑。
//
// # 响应里绝不能出现 session 值（连前缀都不给）
//
// 这一点用**结构**保证，而不是靠"记得别打印"：
// 续期只回 `expires_in_sec` / `ttl_source` 两个事实，新 session 由引擎自己写盘 / 写池
// （见 renew.go），**完全不经过 HTTP 层**。于是即使将来有人往 handler 里加日志，
// 也没有 session 可打。这个上游的三把凭据（session / oma_ / omas_）里 session 是
// 唯一能换钱的，日志与抓包里出现它等于泄漏。
//
// # 并发为什么自己写信号量（而不是"复用本仓共享件"）
//
// 先找过了：本仓**没有跨包共享的并发原语**。唯一先例是
// `internal/workbuddy/growth.go:1023` 的 `growthProbeSem` —— workbuddy 包**私有**类型，
// 导不进来；把它抽成共享件属于"抽共享层"的独立决定（AGENTS.md 共享层协议），
// 不该塞进本次改动里顺手做。所以这里用 `sync.WaitGroup` + 带缓冲 channel 的信号量，
// 并且每个 goroutine **只写自己的下标**（与 growth.go:375 那条注释同一条纪律：
// 不共享就不加锁）。
//
// 并发度 8 的依据：PoW 的 CPU 求解只占小头，实测时间拆解是
// challenge 网络 4117ms / CPU 求解 457ms / redeem 296ms —— 84% 在等网络，
// 所以并发收益极大（串行 5 个 32.5 秒 → 并发 5 个 494ms；8 并发 8/8 全成功）。
package monkeycode

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// renewConcurrency 批量续期的并发度。依据见文件头（84% 时间在等网络）。
const renewConcurrency = 8

// recentRenewWindow 刚续过就跳过的窗口（除非 force）。
//
// 24 小时不是"上游规定的"：上游只给了 30 天有效期，没有任何节流要求。
// 这里取 24h 纯粹是为了挡住"用户连点按钮 / 前端循环调用"这类重复动作
// —— 同一个号一天续一次已经远超需要。
const recentRenewWindow = 24 * time.Hour

// renewAccountFacts 端点做「跳不跳」判断所需的、关于一个账号的事实。
//
// ⚠ 刻意**不直接读 *Auth 的字段**：`Email` / `Password` / `SessionRenewedAt`
// 是 renew.go（并行任务）新增的字段，本文件直接读会把两个任务在**编译期**绑死
// （谁后落地谁就把对方编译红）。所以这三个事实由引擎侧一次性交过来（见 renewSeam）。
type renewAccountFacts struct {
	// UID 账号标识（端点回填，引擎可留空）。
	UID string
	// HasCredentials email 与 password 是否都非空。
	//
	// 非空才可能续期：password-login 需要这两样，缺一个就只能回 GitHub OAuth 那条路。
	HasCredentials bool
	// RenewedAt 上次续期时刻。零值 = 从未续过（不触发 recently-renewed 跳过）。
	RenewedAt time.Time
	// Err 读取该账号事实本身失败（如凭证不存在、类型不对）。
	//
	// ⚠ 与 reason 分开：这是**我们的**失败，不能伪装成 skipped ——
	// skipped 的语义是"不需要做"，把它混进来会让用户以为一切正常。
	Err string
}

// renewSeam 续期引擎的装配点（唯一需要 renew.go 代码的地方）。
//
// 之所以做成接缝而不是直接调 `p.RenewSession(...)`：T-020 与本文件是**并行**任务，
// 直接调用会让本仓在引擎尚未落地时编译失败（编译期耦合）。接缝让本文件
// 现在就能编译、能被完整测试；引擎落地后接线一次即可（见 SetRenewEngine 的示例）。
type renewSeam struct {
	// facts 读出该账号的三个事实（见 renewAccountFacts 的注释）。
	facts func(p *Provider, uid string) renewAccountFacts
	// run 真续期。返回新 session 的剩余秒数与有效期来源（实测/推断）。
	//
	// 只回这两个事实、**不回 session 本身**是关键设计（见文件头）。
	run func(ctx context.Context, p *Provider, uid string) (expiresInSec int64, ttlSource string, err error)
}

// renewEngine 当前装配的续期引擎。nil = 未接线（端点回 501）。
//
// ⚠ 只在装配期（init / 测试开头）写，运行期只读。
var renewEngine *renewSeam

// SetRenewEngine 装配续期引擎。任一参数为 nil 视为"未接线"。
//
// 生产路径由本文件的 init() 自动接上 renew.go 的
// `(*Provider).RenewSession` / `(*Auth).HasRenewCreds`；这里是给**测试**注入假实现的入口
// （接缝的价值就在这：契约测试要能在不联网的前提下构造"缺凭据 / 刚续过 / PoW 超时"这些路径，
// 而 Provider 的方法没法被替换）。
func SetRenewEngine(
	facts func(p *Provider, uid string) renewAccountFacts,
	run func(ctx context.Context, p *Provider, uid string) (int64, string, error),
) {
	if facts == nil || run == nil {
		renewEngine = nil
		return
	}
	renewEngine = &renewSeam{facts: facts, run: run}
}

// init 把续期引擎接到 renew.go 的实现上。
//
// 三个事实的来源都是 renew.go 那一侧的事实，本文件不重复判断：
//
//	HasRenewCreds()  "材料在不在"（Email + Password），**不判 session 是否有效**
//	                 —— 续期恰恰是给 session 已经失效的号用的
//	SessionRenewedAt 上次续期成功的时刻（renew.go 写回凭证时盖的）
func init() {
	SetRenewEngine(renewFactsOf, func(ctx context.Context, p *Provider, uid string) (int64, string, error) {
		_, res, err := p.RenewSession(ctx, uid)
		if err != nil {
			return 0, "", err
		}
		if res == nil {
			return 0, "", errors.New("续期返回空结果")
		}
		// ⚠ 只取这两个事实 —— res.Session 绝不外传（见文件头）。
		return res.ExpiresInSec, res.TTLSource, nil
	})
}

// renewFactsOf 读出「这个账号能不能续、上次什么时候续的」。
//
// ★ 为什么不能直接读 `p.cred(uid)` 一份（真机踩到的坑，2026-11-02 实测）
//
// `p.cred()` 的顺序是【注入的凭证源（生产=核心账号池）】→ 本地 `p.creds`。
// 而续期成功后的写回是【磁盘 + 本地 `p.creds`】—— **不碰池里那份**（见 renew.go:380-393：
// 那是刻意的，池里存的是启动时的快照/投影，就地改会让并发请求读到半写状态）。
// 于是同一个进程里第二次调用时：池里那份 `SessionRenewedAt` 仍是零值 ⇒
// 「24 小时内已续过」这条跳过**永不生效**，用户连点两次就会白烧两次 PoW。
//
// 实测证据（本端点真机跑）：修之前连着调两次，两次都回 `renewed:1 / skipped:0`；
// 修之后第二次回 `skipped:1 / reason:"recently-renewed"`。
//
// 所以这里取**两份里更新的那个**续期时刻（池快照 vs 本地写回），
// 而不是盲信 `p.cred()` 给的那一份。`HasRenewCreds` 同理：本地那份有材料也算有。
func renewFactsOf(p *Provider, uid string) renewAccountFacts {
	pooled := p.cred(uid)
	if pooled == nil {
		return renewAccountFacts{Err: "凭证不存在"}
	}
	f := renewAccountFacts{
		HasCredentials: pooled.HasRenewCreds(),
		RenewedAt:      pooled.SessionRenewedAt,
	}

	// 本地写回那份（续期成功后由 renew.go 更新）。
	p.mu.RLock()
	local := p.creds[uid]
	p.mu.RUnlock()
	if local != nil {
		if local.SessionRenewedAt.After(f.RenewedAt) {
			f.RenewedAt = local.SessionRenewedAt
		}
		if local.HasRenewCreds() {
			f.HasCredentials = true
		}
	}
	return f
}

// RenewEngineReady 报告续期引擎是否已接线（自检 / 前端探测用）。
func RenewEngineReady() bool { return renewEngine != nil }

// renewRow 单号结果。
//
// 键名按本仓惯例用 snake_case（`expires_in_sec` / `ttl_source`）——
// 与同目录的 `api_key` / `daily_token_balance` 等响应字段一致；
// 前端还没写，契约现在统一成本最低。
type renewRow struct {
	UID          string `json:"uid"`
	OK           bool   `json:"ok"`
	Skipped      bool   `json:"skipped"`
	ExpiresInSec int64  `json:"expires_in_sec,omitempty"`
	TTLSource    string `json:"ttl_source,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

// renewScopeUIDs 解析本次请求的 uid 范围。
//
// 给了就用给的（去重、保持顺序）；没给 = 全部（走 uidEnumerator）。
//
// ⚠ 不能只读 p.creds：生产上凭证由注入的凭证源提供，p.creds 只有测试会填
// —— zcode 上踩过（端点返回 200 + 空数组，看起来像"没有账号"）。
func (p *Provider) renewScopeUIDs(given []string) []string {
	if len(given) > 0 {
		seen := make(map[string]struct{}, len(given))
		out := make([]string, 0, len(given))
		for _, uid := range given {
			uid = strings.TrimSpace(uid)
			if uid == "" {
				continue
			}
			if _, dup := seen[uid]; dup {
				continue
			}
			seen[uid] = struct{}{}
			out = append(out, uid)
		}
		return out
	}
	if p.uidEnumerator != nil {
		return p.uidEnumerator()
	}
	return nil
}

// handleRenew 批量续期。
//
// 请求：`POST /admin/monkeycode/renew`
//
//	{"uids": ["..."], "force": false, "limit": 0}
//
//	uids  省略/空 = 全部账号；force = 无视 24h 跳过窗口；limit = 本轮最多处理几个候选
//
// 响应：
//
//	{"ok":true,
//	 "results":[{"uid","ok","skipped","expires_in_sec","ttl_source","reason","error"}, ...],
//	 "renewed":N,"failed":N,"skipped":N,"remaining":N}
func (p *Provider) handleRenew(w http.ResponseWriter, r *http.Request) {
	if renewEngine == nil {
		// 501 而不是"200 + 0 成功"：后者会让界面显示「成功 0 / 失败 0」，
		// 用户以为点了没反应 —— 本仓把这类表现叫静默失败（见 AGENTS.md）。
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"ok":    false,
			"error": "续期引擎未接线（renew.go 的 RenewSession 落地后 SetRenewEngine 一次即可）",
		})
		return
	}

	var body struct {
		UIDs  []string `json:"uids"`
		Force bool     `json:"force"`
		Limit int      `json:"limit"`
	}
	// 解不开就按"全部 + 不 force"跑：与 resolveAuth 同款宽容
	//（前端有查询参数与 JSON body 两种调用方式）。
	_ = jsonDecode(r, &body)
	if len(body.UIDs) == 0 {
		if one := strings.TrimSpace(r.URL.Query().Get("uid")); one != "" {
			body.UIDs = []string{one}
		}
	}

	uids := p.renewScopeUIDs(body.UIDs)
	res := make([]renewRow, 0, len(uids))
	now := time.Now()

	// 第一遍：分类。skipped 与失败**都不进候选**，所以这一段要先把它们摘出去
	//（remaining 的口径就靠它，见文件头 ②）。
	var candidates []renewAccountFacts
	var skipped, factsFailed int
	for _, uid := range uids {
		f := renewEngine.facts(p, uid)
		f.UID = uid
		if f.Err != "" {
			// 读事实就失败了 → 记失败，不伪装成 skip。
			res = append(res, renewRow{UID: uid, Error: f.Err})
			factsFailed++
			continue
		}
		if !f.HasCredentials {
			res = append(res, renewRow{UID: uid, Skipped: true, Reason: "no-credentials"})
			skipped++
			continue
		}
		if !body.Force && !f.RenewedAt.IsZero() && now.Sub(f.RenewedAt) < recentRenewWindow {
			res = append(res, renewRow{UID: uid, Skipped: true, Reason: "recently-renewed"})
			skipped++
			continue
		}
		candidates = append(candidates, f)
	}

	// 第二遍：本轮真正要跑的那一段（limit 缺省 = 全跑）。
	round := candidates
	if body.Limit > 0 && body.Limit < len(candidates) {
		round = candidates[:body.Limit]
	}
	remaining := len(candidates) - len(round)

	// 并发跑，每个 goroutine 只写 roundRows 的**自己那一格** ⇒ 不需要锁。
	roundRows := make([]renewRow, len(round))
	sem := make(chan struct{}, renewConcurrency)
	var wg sync.WaitGroup
	for i, f := range round {
		wg.Add(1)
		go func(i int, f renewAccountFacts) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			sec, src, err := renewEngine.run(r.Context(), p, f.UID)
			if err != nil {
				// 单号失败不中断整批：只写自己这一格。
				roundRows[i] = renewRow{UID: f.UID, Error: err.Error()}
				return
			}
			roundRows[i] = renewRow{UID: f.UID, OK: true, ExpiresInSec: sec, TTLSource: src}
		}(i, f)
	}
	wg.Wait()

	var renewed, failed int
	for _, row := range roundRows {
		if row.OK {
			renewed++
		} else {
			failed++
		}
		res = append(res, row)
	}
	// 第一遍里"读事实失败"的那几条也算 failed（它们没进 roundRows）。
	failed += factsFailed

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"results":   res,
		"renewed":   renewed,
		"failed":    failed,
		"skipped":   skipped,
		"remaining": remaining,
	})
}
