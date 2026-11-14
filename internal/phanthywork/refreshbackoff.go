package phanthywork

// refreshbackoff.go 续期失败的**退避表** —— 官方 `Pd3` 的 `w` map 的忠实移植。
//
// # 官方原文（`Pd3`，偏移 @99945774）
//
//	let K = q.now ?? Date.now,
//	    z = new Map,   // 成功结果缓存（键 = sha256(url+body)）
//	    w = new Map;   // ★ 失败退避表（键 → 解禁时刻）
//	return (O, $) => {
//	  let Y = JSON.stringify($),
//	      H = Zd3("sha256").update(O).update(Y).digest("hex"),
//	      J = z.get(H);
//	  if (J) return J;                                          // 成功结果复用
//	  if ((w.get(H) ?? 0) > K()) return Promise.reject(new Po); // ★ 退避期内直接拒，不打网络
//	  w.delete(H);                                              // 过窗 ⇒ 清掉自己这条
//	  ...
//	  let M = f?.headers.get("retry-after"), W = M ? Number(M) : NaN,
//	      v = M ? (Number.isFinite(W) ? W * 1000 : Date.parse(M) - K()) : NaN,
//	      Z = Math.max(500 * 2 ** X, Number.isFinite(v) ? v : 0);   // 指数或 Retry-After
//	  if (X === 2 || Z > 2000) {
//	    if (w.size >= 128) w.delete(w.keys().next().value);         // 容量上限 128
//	    throw w.set(H, K() + Math.min(Math.max(Z, 5000), 300000)), new Po;  // clamp 到 5s–300s
//	  }
//	  await _(Z)
//
// # 我们为什么需要它（而且比官方更需要）
//
// 官方的表只用来挡 **429/5xx** 的重复冲击。我们多一层理由：
//
// 本轮把 `invalid_grant` 从"终态"改成了"可重试、不计账"（见
// errors.go 的 ErrRefreshRejected）。**不计账就没有熔断** —— 一个真的废掉的
// 凭据会被每一个进来的对话请求各触发一次上游续期。这是移除计数器的**必然副作用**，
// 必须由退避表兜住，否则就是把"账号被误禁"换成"上游被打"。
//
// 所以本表承载两档，各自的窗口来源不同：
//
//	429/5xx 放弃时  → clamp(max(500·2^attempt, Retry-After), 5s, 300s)（官方原值）
//	被拒 invalid_grant → rejectBackoffMS（**我们加的**，官方那一档不进退避表）
//
// ⚠ 第二条是**有意的偏离**，理由如上：官方没有"网关海拔"这个场景，
// 它的 `tH` 只在桌面端要用令牌时才被调一次；我们是 N 个请求共享一个账号。
//
// 判据：TestRefreshBackoffSuppressesRepeatedUpstreamCalls。

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	// refreshBackoffMinMS / refreshBackoffMaxMS 官方那对 clamp 边界（5 秒 ~ 5 分钟）。
	refreshBackoffMinMS = 5_000
	refreshBackoffMaxMS = 300_000

	// refreshBackoffMaxKeys 表容量上限（官方 `w.size >= 128`）。
	refreshBackoffMaxKeys = 128

	// rejectBackoffMS 被拒（invalid_grant）之后的退避窗口。
	//
	// 取值理由：这一段的主要目的**不是**省上游调用，而是把"重试上游"这件事
	// 推后，让**先重读共享凭证库**（sharedstore.go）成为每次调用都会走的第一步 ——
	// 那个是本地操作、零成本，而且正是官方认为能救活账号的那一步。
	//
	// 60 秒与 GateReplayWindow 同量级：一次成功的续期结果也只回放 60 秒，
	// 两处窗口对齐后，"一分钟内一个账号最多打一次上游"这条心智模型才成立。
	rejectBackoffMS = 60_000
)

// refreshBackoff 键 → 解禁时刻（毫秒）的退避表。
//
// 零值可直接用（Auth 常由 json.Unmarshal 直接产出，不走构造器 —— 同一个
// 教训见 credential.go 的 gateReady）。
type refreshBackoff struct {
	mu    sync.Mutex
	until map[string]int64
}

// refreshBackoffKey 官方那个键：`sha256(url + body)` 的十六进制。
//
// 分开传 url 与 body 而不是先拼好，是为了不可能出现"两个不同请求拼出同一个串"
// （例如 url 结尾正好接上 body 开头的巧合）。
func refreshBackoffKey(url string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(url))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// blocked 报告该键是否还在退避窗口里；是则同时返回解禁时刻。
func (b *refreshBackoff) blocked(key string, nowMS int64) (int64, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.until[key]
	if !ok {
		return 0, false
	}
	if until <= nowMS {
		// 窗口已过 —— 官方在这里是 `w.delete(H)`（清掉自己那条再往下走）。
		delete(b.until, key)
		return 0, false
	}
	return until, true
}

// set 记录一次退避。窗口由调用方按来源算好（官方把算式放在调用点，这里照做）。
func (b *refreshBackoff) set(key string, untilMS int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until == nil {
		b.until = make(map[string]int64, 4)
	}
	if len(b.until) >= refreshBackoffMaxKeys {
		// 官方：`w.delete(w.keys().next().value)` —— 丢最早插入的那条。
		// map 无序，所以这里近似成"丢任意一条"：表只用来限速，
		// 丢错一条的后果仅仅是某个键少挡一次，不影响正确性。
		for k := range b.until {
			delete(b.until, k)
			break
		}
	}
	b.until[key] = untilMS
}

// backoffWindowMS 官方那对 clamp：`Math.min(Math.max(Z, 5000), 300000)`。
func backoffWindowMS(z int64) int64 {
	if z < refreshBackoffMinMS {
		return refreshBackoffMinMS
	}
	if z > refreshBackoffMaxMS {
		return refreshBackoffMaxMS
	}
	return z
}

// parseRetryAfterMS 解 `Retry-After`（秒数或 HTTP 日期），解不出回 0。
//
// 对应官方那几行：
//
//	let M = f?.headers.get("retry-after"), W = M ? Number(M) : NaN,
//	    v = M ? (Number.isFinite(W) ? W * 1000 : Date.parse(M) - K()) : NaN
//
// 即：先当秒数，不是数字就当 HTTP 日期减去当前时刻。
func parseRetryAfterMS(v string, nowMS int64) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0
		}
		return secs * 1000
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.UnixMilli() - nowMS; d > 0 {
			return d
		}
	}
	return 0
}
