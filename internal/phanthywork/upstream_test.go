// upstream_test.go 一个**假上游**（httptest），它替服务端做三件事：
//
//	① 按报告里的段序/分隔符**重新验一遍签名**（这是本文件的核心价值）
//	② 校验那几个"文案撒谎"的坑（UA、幂等键的有无、model 前缀、段数）
//	③ 记录"打了哪些端点、打了几次"，用于并发闸/熔断的断言
//
// # 为什么假上游必须自己验签名
//
// 如果假上游只回 200，那么"签名段序全写错"的实现也能拿到全绿 ——
// 那正是本仓记过的形态（测试与生产走两条不同的路）。所以这里的
// verifyRegister / verifyV1 / verifyInference 用的是**与实现独立**
// 的拼装代码（从请求原文里读段），错了就 403。
package phanthywork

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// fakeUpstream 一个假上游 + 请求记录。
type fakeUpstream struct {
	mu    sync.Mutex
	paths []string

	registerCalls int32
	summaryCalls  int32
	chatCalls     int32
	profileCalls  int32
	modelsCalls   int32
	tokenCalls    int32

	identityPub string
	inferPub    string
	// knownIdentityPubs 已注册过的 identity 公钥集合（多账号用例：一台上游认识多把设备密钥）。
	knownIdentityPubs map[string]bool

	// lastSessionHeader / lastInferTokenIssued 记 chat 请求上的
	// x-phanthy-desktop-session 与最近一次下发的推理令牌 —— 用于钉住坑⑨。
	lastSessionHeader   string
	lastInferTokenValue string
	// lastChatStreamTrue 线上收到的 chat body 里 stream 是否为 true（★ 真机 502 回归）。
	lastChatStreamTrue bool

	// ── 2026-11-14 真机封号/500 三条对照实验所需的夹具开关 ──

	// serverTime 非空时 summary 用它当 server_time（默认 = 夹具此刻的 UTC 时间）。
	//
	// 为什么要它可控：坑① 的判据是"timestamp 必须用**服务端口径**的时间"，
	// 而断言必须落在**独立的真源**上 —— 也就是夹具自己写死的这个字面量，
	// 而不是从实现里再算一遍。
	serverTime string
	// registerReasonCode 非空时注册回 **HTTP 200** + {"reason_code": ...}（★ 封号铁证）。
	//
	// ⚠ 这条**不是** 409：真机上服务端对"该账号已有第一台设备"的二次注册回的是
	// 200 + reason_code=first_device_owner（凭证文件里留着的那一行就是它）。
	registerReasonCode string
	// lastSessionID / lastClaudeSessionID 记推理请求上的
	// x-phanthy-session 与 x-claude-code-session-id（★ 500 空体的判据）。
	lastSessionID       string
	lastClaudeSessionID string
	// lastInferenceCanonical 假上游**独立重算**出来的 12 段 canonical（★ 第 10 段的判据）。
	//
	// ⚠ 它必须是**重算**的结果，不是从实现里抄的 —— 否则断言就成了同义反复。
	lastInferenceCanonical string

	// conflict 置真时注册回 409（坑⑥ 的终态断言）。
	conflict bool
	// summaryStatus 非 0 时用它当 summary 的状态码。
	summaryStatus int
	// omitIDem 置真时 summary **带上**幂等键（模拟"顺手加一个"的错法）。
	omitIDem bool
	// badUA 置真时把 UA 换掉（模拟坑②）。
	badUA bool
	// tokenDelay 拖慢 token 端点（并发用例要让它真的重叠，否则用例会"永远绿"）。
	tokenDelay time.Duration

	// ── 签到 / 额度相关的开关与记录 ──

	// dailyStatus summary 里 daily.status 的取值（"" = 不给 daily 块）。
	dailyStatus string
	// dailyEnabled feature_flags.daily_enabled 的取值。
	dailyEnabled bool
	// creditsAvailable summary 里 credits.available 的取值。
	creditsAvailable int64
	// serverDate 服务端日键（签到幂等键必须用它 —— 用例据此断言"没本地算日期"）。
	serverDate string
	// omitServerDate 置真时 daily 块**不给** server_date（模拟上游漏字段）。
	omitServerDate bool
	// claimStatus claim 端点返回的 status（默认 granted）。
	claimStatus string
	// claimPoints claim 端点返回的积分。
	claimPoints int64
	// claimAuthFail 置真时 claim 回 401（用于额度/签到的"凭据失效"路径）。
	claimAuthFail bool

	// summaryAuthFail 置真时 summary 回 401（额度 AuthFailed 判据用）。
	summaryAuthFail bool
	// summaryAuthFailCode 上面那个 401 的 error.code（空 = "invalid_grant"，= 真·凭据失效）。
	//
	// ⚠ 2026-11-13 加这个开关的原因：原来这里**写死** `desktop_installation_required` ——
	//   那是"这台设备还没注册"的标记，而当时分类器把它判成 Auth ⇒ 夹具这条 401 一直被当成
	//   "凭据废了"用，于是"未注册被误摘号"这个真 bug 在用例里**看不见**。
	//   现在：默认给真·凭据失效的标记；要模拟未注册就显式把它设成那个串。
	summaryAuthFailCode string

	// lastClaimBody / lastClaimKey 记下 claim 收到的 body 与幂等键（供断言）。
	lastClaimBody string
	lastClaimKey  string
	// claimCalls 签到端点被调次数。
	claimCalls int32

	// ── 邀请码面板相关的开关与记录（invite_test.go，2026-11-12 加）──

	// eligibleToInvite GET /referrals/me 的 referral.eligible_to_invite。
	eligibleToInvite bool
	// referralProgramStatus 同上的 program_status（空 = 夹具给 "active"）。
	referralProgramStatus string
	// rewardPoints / totalLimit / totalGranted 同上的三个计数（0 = 夹具给实测默认值 3000/50/0）。
	rewardPoints int64
	totalLimit   int64
	totalGranted int64
	// referralSlotCount POST /referrals/code 回几个槽（0 = 5，实测就是 5）。
	referralSlotCount int
	// referralMeStatus / referralCodeStatus 非 0 时用它们当状态码（错误路径用例）。
	referralMeStatus   int32
	referralCodeStatus int32
	// referralsMeCalls / referralCodeCalls 两个端点各自被调次数（护栏②的判据）。
	referralsMeCalls  int32
	referralCodeCalls int32
	// failReferralCodeInstID 命中这个 installation id 的取码请求回 500（**按账号**隔离失败用）。
	failReferralCodeInstID string
	// lastReferralKey / lastReferralBody 记下取码收到的幂等键与 body（形态断言）。
	lastReferralKey  string
	lastReferralBody string
}

// startFakeUpstream 起假上游并注册清理。
func startFakeUpstream(t *testing.T) (*httptest.Server, *fakeUpstream) {
	t.Helper()
	f := &fakeUpstream{}
	mux := http.NewServeMux()

	mux.HandleFunc(pathDailyLoginClaim, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.claimCalls, 1)
		f.record(r)
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		key := r.Header.Get("idempotency-key")
		f.mu.Lock()
		f.lastClaimBody, f.lastClaimKey = string(raw), key
		statusWanted := f.claimStatus
		points := f.claimPoints
		day := f.serverDate
		authFail := f.claimAuthFail
		f.mu.Unlock()
		if day == "" {
			day = time.Now().UTC().Add(8 * time.Hour).Format("2006-01-02")
		}
		if authFail {
			writeJSONTest(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code":    "desktop_installation_required",
				"message": "Desktop installation required.",
			}})
			return
		}
		// ★ 硬要求①：body 必须是**空对象**（不是空串、不是 {} 之外的东西）。
		if strings.TrimSpace(string(raw)) != "{}" {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code":    "desktop_binding_invalid",
				"message": "claim body must be an empty JSON object, got: " + string(raw),
			}})
			return
		}
		// ★ 硬要求②：幂等键必须是 daily:<installationId>:<server_date>，且日期用**服务端**那天。
		wantKey := DailyLoginIdempotencyKey(r.Header.Get("x-desktop-installation-id"), day)
		if key != wantKey {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code":    "desktop_binding_invalid",
				"message": "idempotency-key 期望 " + wantKey + "，实际 " + key,
			}})
			return
		}
		// v1 签名（**带幂等键**）必须验得过。
		if err := f.verifyV1WithRaw(r, raw, wantKey, ""); err != nil {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code":    "desktop_binding_invalid",
				"message": err.Error(),
			}})
			return
		}
		if statusWanted == "" {
			statusWanted = "granted"
		}
		resp := map[string]any{
			"schema_version": 1,
			"status":         statusWanted,
			"server_date":    day,
			"streak_day":     1,
		}
		if statusWanted != "already_granted" {
			if points == 0 {
				points = DailyCheckinPoints
			}
			resp["points"] = points
		}
		resp["reward"] = map[string]any{"points": points, "status": "approved", "reward_type": "daily_login"}
		writeJSONTest(w, 200, resp)
	})

	// ── 邀请码面板的两个端点（2026-11-12 加）──────────────────────────────
	// 形状逐字照 2026-11-12 的真机取证（结构 22 键 + 5 槽）。
	mux.HandleFunc(pathReferralsMe, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.referralsMeCalls, 1)
		f.record(r)
		f.mu.Lock()
		st := atomic.LoadInt32(&f.referralMeStatus)
		eligible, program := f.eligibleToInvite, f.referralProgramStatus
		rp, tl, tg := f.rewardPoints, f.totalLimit, f.totalGranted
		f.mu.Unlock()
		if st != 0 {
			writeJSONTest(w, int(st), map[string]any{"error": map[string]any{"code": "boom"}})
			return
		}
		if program == "" {
			program = "active"
		}
		if rp == 0 {
			rp = 3000
		}
		if tl == 0 {
			tl = 50
		}
		writeJSONTest(w, 200, map[string]any{
			"schema_version": 1,
			"referral": map[string]any{
				"eligible_to_invite": eligible, "eligibility_reason": nil, "program_status": program,
				"reward_points": rp, "next_reward_points": rp, "invitee_reward_points": rp,
				"reward_tiers": []any{rp}, "invited_progress": nil,
				"pending_count": 0, "total_granted": tg, "total_limit": tl, "total_occupied": 0,
				"remaining_slots": tl, "daily_limit": 50, "daily_granted": 0, "daily_committed": 0,
				"tier_allocated_count": 0, "tier_granted_count": 0, "recent_relations": []any{},
			},
		})
	})
	mux.HandleFunc(pathReferralsCode, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.referralCodeCalls, 1)
		f.record(r)
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		key := r.Header.Get("idempotency-key")
		f.mu.Lock()
		st := atomic.LoadInt32(&f.referralCodeStatus)
		n := f.referralSlotCount
		failInst := f.failReferralCodeInstID
		f.lastReferralKey, f.lastReferralBody = key, string(raw)
		f.mu.Unlock()
		// ⚠ 幂等键现在是 `referral-code:<instID>`（两段，instID 后**没有**冒号）——
		//   所以这里不能再找 ":"+failInst+":"，否则"指定账号失败"这条开关会静默失效。
		if failInst != "" && strings.Contains(key, ":"+failInst) {
			writeJSONTest(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal"}})
			return
		}
		// ★ 体例与签到端点同款：body 必须是**空对象**、幂等键必须是 referral-code:<instID>。
		if strings.TrimSpace(string(raw)) != "{}" {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code": "bad_body", "message": "body must be an empty JSON object, got: " + string(raw)}})
			return
		}
		if !strings.HasPrefix(key, "referral-code:") || strings.Count(key, ":") != 1 {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code": "bad_idem", "message": "idempotency-key 期望 referral-code:<installationId>（不带日期），实际 " + key}})
			return
		}
		if st != 0 {
			writeJSONTest(w, int(st), map[string]any{"error": map[string]any{"code": "boom"}})
			return
		}
		if n <= 0 {
			n = 5
		}
		slots := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			slots = append(slots, map[string]any{
				"slot":        i,
				"code":        fmt.Sprintf("2_GeoAgB0BlMMz_FDydFKO2SJHyXGxA%d", i),
				"invite_code": fmt.Sprintf("46KG-8S4M-2CE%d", i),
				"share_url":   fmt.Sprintf("https://code.phanthy.com/r/slot%d", i),
			})
		}
		writeJSONTest(w, 200, map[string]any{"schema_version": 1, "slots": slots})
	})

	mux.HandleFunc(pathProfile, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.profileCalls, 1)
		f.record(r)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			writeJSONTest(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized"}})
			return
		}
		writeJSONTest(w, 200, map[string]any{
			"account": map[string]any{"uuid": "scope-u-1", "email": "+86 150****3411"},
		})
	})

	mux.HandleFunc(pathRegister, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.registerCalls, 1)
		f.record(r)
		if f.conflict {
			writeJSONTest(w, http.StatusConflict, map[string]any{"error": map[string]any{
				"code":    "desktop_installation_conflict",
				"message": "Desktop installation identifier does not match its public key.",
			}})
			return
		}
		if reason := f.registerReasonCode; reason != "" {
			// ★ 2026-11-14 真机封号铁证：二次注册回的是 **200 + reason_code**，
			//   不是 409 —— 所以"只看状态码"的判据会把它当成注册成功。
			//
			// 夹具在这里**短路**（不验签）：这条用例要钉的是状态机，不是签名。
			writeJSONTest(w, 200, map[string]any{
				"reason_code":   reason,
				"reward_status": "denied",
			})
			return
		}
		if err := f.verifyRegister(r); err != nil {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code":    "desktop_binding_invalid",
				"message": err.Error(),
			}})
			return
		}
		writeJSONTest(w, 200, map[string]any{
			"installation":  map[string]any{"id": "cmwctrcoe259ooy3jvk42mnwc"},
			"reward_status": "approved",
			"reward_reason": "first_device_owner",
		})
	})

	mux.HandleFunc(pathActivitiesSummary, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.summaryCalls, 1)
		f.record(r)
		if f.summaryStatus != 0 {
			writeJSONTest(w, f.summaryStatus, map[string]any{"error": map[string]any{
				"code":    "request_not_authorized",
				"message": "当前请求不可用，请更新或重新打开客户端后重试。",
			}})
			return
		}
		// ⚠ 坑②：UA 必须逐字是 Bun/1.3.14。
		if f.badUA && r.Header.Get("User-Agent") != DefaultUA {
			writeJSONTest(w, http.StatusForbidden, map[string]any{
				"error_code": "TM.00000001",
				"error_msg":  "网络繁忙，请稍后重试",
			})
			return
		}
		if f.summaryAuthFail {
			code := f.summaryAuthFailCode
			if code == "" {
				code = "invalid_grant" // 默认 = **真·凭据失效**（未注册要显式指定）
			}
			writeJSONTest(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code":    code,
				"message": "auth failed for test",
			}})
			return
		}
		wantIdem := ""
		if f.omitIDem {
			wantIdem = "summary" + ":x" // 故意错：这条请求不该有幂等键（坑⑪）
		}
		if err := f.verifyV1(r, wantIdem); err != nil {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code":    "desktop_binding_invalid",
				"message": err.Error(),
			}})
			return
		}
		// 令牌 TTL 180 秒（坑④），payload 带 sub（身份）。
		tok := fakeJWT("u-1", time.Now().Unix()+180)
		f.mu.Lock()
		f.lastInferTokenValue = tok
		serverTime := f.serverTime
		f.mu.Unlock()
		if serverTime == "" {
			serverTime = time.Now().UTC().Format(time.RFC3339Nano)
		}
		day := f.serverDate
		if day == "" {
			day = time.Now().UTC().Add(8 * time.Hour).Format("2006-01-02") // UTC+8 的日键（服务端口径）
		}
		body := map[string]any{
			"schema_version":               1,
			"server_time":                  serverTime,
			"desktop_inference_token":      tok,
			"desktop_inference_expires_at": time.Now().Add(180 * time.Second).UTC().Format(time.RFC3339Nano),
			"credits":                      map[string]any{"available": f.creditsAvailable},
			"feature_flags":                map[string]any{"daily_enabled": f.dailyEnabled},
		}
		if f.dailyStatus != "" {
			daily := map[string]any{
				"status":         f.dailyStatus,
				"points":         DailyCheckinPoints,
				"next_points":    DailyCheckinPoints,
				"streak_day":     1,
				"max_streak_day": 5,
			}
			// omitServerDate：模拟"上游漏了 server_date"（用例断言此时**拒绝**用本地日期）。
			if !f.omitServerDate {
				daily["server_date"] = day
			}
			body["daily"] = daily
		}
		writeJSONTest(w, 200, body)
	})

	mux.HandleFunc(pathModels, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.modelsCalls, 1)
		f.record(r)
		writeJSONTest(w, 200, map[string]any{
			"schema_version": 1,
			"default_model":  DefaultModelID,
			"models": []map[string]any{
				{"id": DefaultModelID, "context_window_tokens": 262144},
				{"id": "glm-5.3-flash", "context_window_tokens": 131072},
			},
		})
	})

	mux.HandleFunc(pathOAuthToken, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.tokenCalls, 1)
		f.record(r)
		if f.tokenDelay > 0 {
			select {
			case <-time.After(f.tokenDelay):
			case <-r.Context().Done():
				return
			}
		}
		writeJSONTest(w, 200, map[string]any{
			"access_token":  fakeJWT("u-1", time.Now().Unix()+1800),
			"refresh_token": "refresh-rotated",
			"expires_in":    1800,
			"scope":         scopeString(),
		})
	})

	mux.HandleFunc(pathMessages, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.chatCalls, 1)
		f.record(r)
		// ⚠ 坑⑨：这个头放的是**令牌本身**（不是 sessionId）。
		f.mu.Lock()
		f.lastSessionHeader = r.Header.Get("x-phanthy-desktop-session")
		// ★ 500 空体的判据：这两个头必须存在、且**同一个值**。
		f.lastSessionID = r.Header.Get("x-phanthy-session")
		f.lastClaudeSessionID = r.Header.Get("x-claude-code-session-id")
		f.mu.Unlock()
		if r.Header.Get("x-phanthy-desktop-session") == "" {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code": "request_not_authorized", "message": "missing desktop session",
			}})
			return
		}
		if err := f.verifyInference(r); err != nil {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code": "request_not_authorized", "message": err.Error(),
			}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start" + "\n"))
		_, _ = w.Write([]byte("data: {\"usage\":{\"output_tokens\":224}}" + "\n\n"))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

// record 记一次请求路径。
func (f *fakeUpstream) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
}

// pathCount 该路径被打了多少次。
func (f *fakeUpstream) pathCount(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, x := range f.paths {
		if x == p {
			n++
		}
	}
	return n
}

// trust 让假上游"已经认识这台设备"（= 该账号注册过了）。
//
// # 为什么必须有它
//
// 假上游的公钥是在**注册**那一步学到的（真实服务端也一样）。而额度/签到这类
// 只读用例不该为了验签而去跑一遍注册 —— 那会把"写操作"混进只读用例里，
// 也会让用例依赖注册的副作用。所以这里直接把账号的两把公钥交给假上游。
//
// ⚠ 它只影响**验签**，不影响任何业务判据（UA/幂等键/body 形状仍逐条校验）。
func (f *fakeUpstream) trust(a *Auth) {
	if f == nil || a == nil {
		return
	}
	idPub, _ := a.identityPublicKey()
	f.mu.Lock()
	f.identityPub = idPub
	f.inferPub = a.Infer.PublicKey
	if f.knownIdentityPubs == nil {
		f.knownIdentityPubs = map[string]bool{}
	}
	f.knownIdentityPubs[idPub] = true
	f.mu.Unlock()
}

// pathsSnapshot 打过的全部路径（副本，供失败信息里展示链路）。
func (f *fakeUpstream) pathsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.paths))
	copy(out, f.paths)
	return out
}

// verifyRegister 独立重算并校验设备注册的签名（device canonical，7 段）。
func (f *fakeUpstream) verifyRegister(r *http.Request) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	var body struct {
		PublicKey       string `json:"public_key"`
		Edition         string `json:"edition"`
		Platform        string `json:"platform"`
		Version         string `json:"version"`
		DevicePublicKey string `json:"device_public_key"`
		DeviceSignature string `json:"device_signature"`
		InferenceKey    string `json:"inference_public_key"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return errFake("register body 不是 JSON: " + err.Error())
	}
	if body.Edition != Edition {
		return errFake("edition 必须是 " + Edition)
	}
	if body.Platform == "" {
		return errFake("platform 不能为空")
	}
	// ⚠ version 必须是**官方客户端**版本号，不是插件版本（报告 §2.4）。
	if body.Version != OfficialClientVersion {
		return errFake("version 必须是官方客户端版本号 " + OfficialClientVersion + "，得到 " + body.Version)
	}
	if body.DevicePublicKey == body.PublicKey {
		return errFake("device 公钥必须与 identity 公钥不同（IDENTITY_POLICY.deviceKeyDistinct）")
	}
	if !isInstallationID(r.Header.Get("x-desktop-installation-id")) {
		return errFake("x-desktop-installation-id 不是 di_ 形态: " + r.Header.Get("x-desktop-installation-id"))
	}
	// 独立重算 device canonical：段序与第 5 段都按报告。
	canonical := DeviceCanonical(
		"scope-u-1",
		r.Header.Get("x-desktop-installation-id"),
		body.PublicKey,
		body.InferenceKey, // ⚠ 第 5 段必须是**推理公钥**（坑⑩）
		r.Header.Get("x-desktop-timestamp"),
		r.Header.Get("x-desktop-nonce"),
	)
	if !verifyEd25519(body.DevicePublicKey, []byte(canonical), body.DeviceSignature) {
		return errFake("device_signature 验不过（段序/第 5 段/哪把私钥）")
	}
	// register 那条**要**带 register:<installationId> 幂等键。
	if err := f.verifyV1WithRaw(r, raw, registerIdempotencyKey(r.Header.Get("x-desktop-installation-id")), body.PublicKey); err != nil {
		return err
	}
	f.mu.Lock()
	f.identityPub, f.inferPub = body.PublicKey, body.InferenceKey
	if f.knownIdentityPubs == nil {
		f.knownIdentityPubs = map[string]bool{}
	}
	f.knownIdentityPubs[body.PublicKey] = true
	f.mu.Unlock()
	return nil
}

// verifyV1 校验 v1 签名（从请求里读 body）。
func (f *fakeUpstream) verifyV1(r *http.Request, wantIdem string) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return f.verifyV1WithRaw(r, raw, wantIdem, "")
}

// verifyV1WithRaw 与 verifyV1 同体，body 由调用方给（identityPub 可覆盖）。
func (f *fakeUpstream) verifyV1WithRaw(r *http.Request, raw []byte, wantIdem, identityPub string) error {
	if identityPub == "" {
		f.mu.Lock()
		identityPub = f.identityPub
		f.mu.Unlock()
	}
	// 多账号：验签时把"所有已注册过的设备公钥"都试一遍（真实服务端也是这样按
	// x-desktop-installation-id 找到对应设备再验的）。
	var candidates []string
	if identityPub != "" {
		candidates = append(candidates, identityPub)
	}
	f.mu.Lock()
	for k := range f.knownIdentityPubs {
		if k != identityPub {
			candidates = append(candidates, k)
		}
	}
	f.mu.Unlock()
	timestamp := r.Header.Get("x-desktop-timestamp")
	nonce := r.Header.Get("x-desktop-nonce")
	if timestamp == "" || nonce == "" {
		return errFake("缺 x-desktop-timestamp / x-desktop-nonce")
	}
	if got := r.Header.Get("idempotency-key"); got != wantIdem {
		return errFake("idempotency-key 应为 " + wantIdem + "，实际 " + got)
	}
	// ⚠ 坑⑫：v1 串里 method **保持原值**（这里用请求原文的 r.Method）。
	canonical := V1Canonical(r.Method, r.URL.Path, timestamp, nonce, raw, wantIdem)
	// ⚠ 用常量而不是 "\n" 字面量：v1 的分隔符是**空格**（2026-11-14 修正）。
	//   写死字面量的话，下次改分隔符这条断言会静默变成"永远失败"或"永远通过"。
	if len(strings.Split(canonical, deviceV1Separator)) != 7 {
		return errFake("v1 canonical 不是 7 段")
	}
	sig := r.Header.Get("x-desktop-signature")
	verified := false
	for _, pub := range candidates {
		if verifyEd25519(pub, []byte(canonical), sig) {
			verified = true
			break
		}
	}
	if !verified {
		return errFake("x-desktop-signature 验不过（v1 段序/分隔符/method 大小写，或设备公钥未知）")
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return errFake("缺 Bearer 授权头")
	}
	return nil
}

// verifyInference 独立重算并校验 12 段 proof 签名。
func (f *fakeUpstream) verifyInference(r *http.Request) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	f.mu.Lock()
	inferPub := f.inferPub
	f.mu.Unlock()
	if inferPub == "" {
		return errFake("服务端还不知道推理公钥（没注册过）")
	}
	// ⚠ 坑⑨：这个头放的是**令牌本身**。
	token := r.Header.Get("x-phanthy-desktop-session")
	if !strings.HasPrefix(token, "eyJ") {
		return errFake("x-phanthy-desktop-session 里应当是推理令牌本身（坑⑨），得到 " + token)
	}
	canonical := InferenceCanonical(
		r.Method, r.URL.Path,
		r.Header.Get("x-phanthy-proof-timestamp"),
		r.Header.Get("x-phanthy-proof-nonce"),
		raw,
		r.Header.Get("Authorization"),
		token,
		r.Header.Get("x-request-id"),
		r.Header.Get("x-claude-code-session-id"),
		r.Header.Get("x-phanthy-expert-id"),
		r.Header.Get("x-phanthy-skill-names"),
	)
	// ★ 存下**独立重算**的 canonical：用例要拿它断言"第 10 段（1-based）
	//   等于收到的 session 头"，而不是从实现里再算一遍（那会是同义反复）。
	f.mu.Lock()
	f.lastInferenceCanonical = canonical
	f.mu.Unlock()
	// ⚠ 用常量而不是 "\n" 字面量（inference 的分隔符确实是 LF，但常量更耐改）。
	if len(strings.Split(canonical, inferenceSeparator)) != 12 {
		return errFake("inference canonical 不是 12 段")
	}
	if !verifyEd25519(inferPub, []byte(canonical), r.Header.Get("x-phanthy-proof-signature")) {
		return errFake("x-phanthy-proof-signature 验不过（段序/分隔符/method 大写/哪把私钥）")
	}
	// ★ 真机 502 的回归判据：线上收到的 body 里 stream 必须是 true。
	var streamProbe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(raw, &streamProbe)
	f.mu.Lock()
	f.lastChatStreamTrue = streamProbe.Stream
	f.mu.Unlock()
	// ⚠ 坑③：请求体里的 model 必须是**裸 id**。
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return errFake("请求体不是 JSON: " + err.Error())
	}
	if strings.HasPrefix(probe.Model, providerID+"/") {
		return errFake("model 带了 " + providerID + " 前缀（坑③ => 403 model_not_allowed）: " + probe.Model)
	}
	if probe.Model == "" {
		return errFake("model 为空")
	}
	// Anthropic Messages 协议要求 max_tokens。
	if !strings.Contains(string(raw), "max_tokens") {
		return errFake("Anthropic 请求体缺 max_tokens")
	}
	if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
		return errFake("anthropic-version 应为 " + anthropicVersion + "，实际 " + got)
	}
	return nil
}

// fakeErr 是假上游用的错误类型（让判据可读）。
type fakeErr string

// Error 实现 error。
func (e fakeErr) Error() string { return string(e) }

// errFake 造一个假上游错误。
func errFake(s string) error { return fakeErr(s) }

// writeJSONTest 写一个 JSON 响应。
func writeJSONTest(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newFakeProvider 建一个指向假上游的 Provider。
//
// 时钟基线给 0：校准由 summary 的 server_time 完成（坑① 的正常路径）。
//
// ⚠ **默认显式设成 ProtocolMessages**（旧版 Anthropic + 12 段签名）。
//
// 本文件与 provider_test.go / preparebody_test.go 里的假上游**全部按
// `/v1/messages` 的签名语义建**（断言 v1 canonical / 推理令牌 TTL /
// 设备注册 / 幂等键）。而生产默认已改成 `chat`（/v1/chat/completions，不签名）。
//
// 不显式指定的话，这些用例会拿 chat 路径去打一个只认 messages 的假上游
// ⇒ 一律 404 / 空流，而那**不是**被测逻辑坏了。协议由用例自己声明：
//
//	· 测 messages 管线 → newFakeProvider（默认 messages）
//	· 测 chat 管线     → newFakeProviderChat
func newFakeProvider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	c := NewWithBase(srv.URL)
	c.UA = DefaultUA
	p := NewWithClient(c, t.TempDir(), 0)
	p.SetProtocol("messages")
	return p
}

// newFakeProviderChat 走**新版内核协议**（/v1/chat/completions，不签名）的假上游 Provider。
//
// 与 newFakeProvider 的唯一差别是协议；Client 构造完全相同 ⇒
// "协议"是唯一变量，排查时不会和 UA / 时钟 / 基址混在一起。
func newFakeProviderChat(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	c := NewWithBase(srv.URL)
	c.UA = DefaultUA
	p := NewWithClient(c, t.TempDir(), 0)
	p.SetProtocol("chat")
	return p
}

// ══════════════════════════════════════════════════════════════════════════
// 2026-11-14 真机三条的回归（每条都对应一次单变量对照实验）
// ══════════════════════════════════════════════════════════════════════════

// TestInferenceRequestCarriesSessionHeaders 推理请求必须同时带
// `x-phanthy-session` 与 `x-claude-code-session-id`，且 canonical **第 10 段**
// 与它们**同一个值**。
//
// 真机判据（同账号、同 key、同 body，唯一变量就是 session 头）：
//
//	两个头都缺 + 第 10 段空 → **500 Internal Error，0 字节**，kong upstream-latency 9~10ms
//	两个头齐全 + 第 10 段同值 → **200**
//
// 官方 sidecar 的拦截器做的是把内核的 `x-phanthy-session` **改写成**
// `x-claude-code-session-id`（原头保留），再用改写后的值去算 canonical。
// 本包在 buildProof 里生成一次、三处共用，所以三者必须逐字相同。
func TestInferenceRequestCarriesSessionHeaders(t *testing.T) {
	srv, f := startFakeUpstream(t)
	defer srv.Close()
	p := newFakeProvider(t, srv)
	a := newTestAuth("u-1", "rt")
	// ⚠ 前置：先真注册一次。
	//
	// 假上游是在 register 里**学到**设备公钥与推理公钥的（verifyRegister 末尾），
	// 没这一步就发推理，它连验签用的公钥都没有 —— 失败原因会变成
	// "服务端还不知道推理公钥（没注册过）"，与"session 头写错了"混在一起。
	if err := p.ensureInstallation(context.Background(), a); err != nil {
		t.Fatalf("前置注册失败: %v", err)
	}
	// 推理令牌必须是 JWT 形态（假上游按 `eyJ` 前缀判"这个头放的是令牌本身"）。
	now := nowMS()
	a.storeInferenceToken(fakeJWT("u-1", time.Now().Unix()+180), now+120_000, now)
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}

	cs, err := p.Chat(context.Background(), cred, []byte(
		`{"model":"phanthywork/glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`))
	if err != nil {
		t.Fatalf("Chat 报错: %v", err)
	}
	defer cs.Body.Close()
	_, _ = io.Copy(io.Discard, cs.Body)

	f.mu.Lock()
	gotSession := f.lastSessionID
	gotClaude := f.lastClaudeSessionID
	canonical := f.lastInferenceCanonical
	f.mu.Unlock()

	if gotSession == "" || gotClaude == "" {
		t.Fatalf("两个 session 头都必须发（缺任意一个上游回 500 空体）: x-phanthy-session=%q x-claude-code-session-id=%q",
			gotSession, gotClaude)
	}
	if gotSession != gotClaude {
		t.Errorf("两个 session 头必须同值: x-phanthy-session=%q x-claude-code-session-id=%q", gotSession, gotClaude)
	}
	// 形态：ses_ + 24 位 cuid2（小写字母/数字），与抓包原文一致
	if !strings.HasPrefix(gotSession, "ses_") || len(gotSession) != 4+24 {
		t.Errorf("session id 形态应为 ses_<24 位 cuid2>，得到 %q（len=%d）", gotSession, len(gotSession))
	}

	segs := strings.Split(canonical, inferenceSeparator)
	if len(segs) != 12 {
		t.Fatalf("inference canonical 段数 = %d, want 12", len(segs))
	}
	if segs[9] != gotSession {
		t.Errorf("canonical 第 10 段必须等于 session 头: 段=%q 头=%q", segs[9], gotSession)
	}
	if segs[9] == "" {
		t.Error("第 10 段在线上路径上不许为空（空 → 上游 500 空体）")
	}
}

// TestUncalibratedAuthForcesTokenRenewal **时钟没校准过时，即使缓存令牌还有效，
// 也必须去换一次令牌**。
//
// 为什么：换令牌（activities/summary）是唯一能拿到 `server_time` 的常规路径，
// 而 proof 的 `x-phanthy-proof-timestamp` 必须用服务端口径的时间。
// 本机时钟实测慢 52 秒（另一次 15 秒，一直在漂）—— 未校准就发推理，
// 上游回 403 `request_not_authorized`，而那句文案完全指不到时钟上。
//
// 这条守的是旧实现的漏洞：`if a.inferenceTokenValid(now) { return 缓存 }`
// 会在"进程刚起来、还没换过令牌"这条路径上**跳过唯一一次校准** ⇒ 必然 403。
func TestUncalibratedAuthForcesTokenRenewal(t *testing.T) {
	srv, f := startFakeUpstream(t)
	defer srv.Close()
	p := newFakeProvider(t, srv)
	a := newTestAuth("u-1", "rt")
	// 前置：先注册一次，让假上游学到设备公钥（否则 summary 的 v1 验签必失败，
	// 而那条失败看起来像"签名写错了"，其实只是夹具没前置）。
	if err := p.ensureInstallation(context.Background(), a); err != nil {
		t.Fatalf("前置注册失败: %v", err)
	}
	now := nowMS()
	a.storeInferenceToken(fakeJWT("u-1", time.Now().Unix()+180), now+120_000, now)

	// 刻意清掉校准标记（夹具默认是已校准的，这里要的正是"没校准"这条路径）。
	a.skewCalibrated.Store(false)
	if a.isClockCalibrated() {
		t.Fatal("前置条件：本项目的前置状态必须是未校准")
	}
	before := atomic.LoadInt32(&f.summaryCalls)

	tok, err := p.ensureInferenceToken(context.Background(), a)
	if err != nil {
		t.Fatalf("未校准时应能通过换令牌完成校准，却报错: %v", err)
	}
	if tok == "" {
		t.Fatal("换令牌后必须拿到非空令牌")
	}
	if n := atomic.LoadInt32(&f.summaryCalls); n != before+1 {
		t.Errorf("未校准时必须打一次 activities/summary（用于取 server_time），实际 %d 次（基线 %d）", n, before)
	}
	if !a.isClockCalibrated() {
		t.Error("换令牌成功后必须已经把时钟标成已校准")
	}

	// 反面：校准过之后再取，**不该**再打一次 summary（省一次往返）
	before2 := atomic.LoadInt32(&f.summaryCalls)
	if _, err := p.ensureInferenceToken(context.Background(), a); err != nil {
		t.Fatalf("已校准后取令牌不该报错: %v", err)
	}
	if after := atomic.LoadInt32(&f.summaryCalls); after != before2 {
		t.Errorf("已校准且令牌有效时不该再换令牌（summary 调用 %d → %d）", before2, after)
	}
}

// TestRegisterReasonCodeIsTerminalConflict **二次注册**是终态冲突，不是成功。
//
// 真机铁证（2026-11-14，凭证文件里留下的原文）：
//
//	{"registered_at_ms":…, "reason_code":"first_device_owner"}
//	09:08:5x 同一账号 /v1/messages → 200
//	09:19:33 用全新 identity seed 注册第二台设备 → 服务端回 **HTTP 200** + reason_code
//	09:20:08 同一账号 /models、/profile → 403 disabled_account（账号被封）
//
// ⇒ 服务端对"该账号已有第一台设备"回的是 **200 + reason_code**，**不是 409**。
// 只看状态码会把它当成功 ⇒ 拿一把服务端不认的 key 去换票/推理（一路 403），
// 并把账号打进 disabled_account。
func TestRegisterReasonCodeIsTerminalConflict(t *testing.T) {
	srv, f := startFakeUpstream(t)
	defer srv.Close()
	f.registerReasonCode = "first_device_owner"
	p := newFakeProvider(t, srv)
	a := newTestAuth("u-rc", "rt")

	err := p.ensureInstallation(context.Background(), a)
	if err == nil {
		t.Fatal("reason_code 非空必须是**终态冲突**，不能当注册成功")
	}
	if !IsRegistrationConflict(err) {
		t.Errorf("必须能被 IsRegistrationConflict 认出来，得到 %v", err)
	}
	if a.Reg.RegisteredAtMS != 0 {
		t.Errorf("冲突时**绝不能**写 RegisteredAtMS（那是「已注册」的信号），实际 %d", a.Reg.RegisteredAtMS)
	}
	if a.Reg.ConflictAtMS == 0 {
		t.Error("冲突时必须写 ConflictAtMS（熔断起点）")
	}
	if a.Reg.ReasonCode != "first_device_owner" {
		t.Errorf("必须记下服务端的 reason_code，得到 %q", a.Reg.ReasonCode)
	}

	// 熔断：再调一次不许再发注册请求（否则持续触发设备风控 → 封号）
	before := atomic.LoadInt32(&f.registerCalls)
	_ = p.ensureInstallation(context.Background(), a)
	if after := atomic.LoadInt32(&f.registerCalls); after != before {
		t.Errorf("终态冲突后不许再发注册请求（register 调用 %d → %d）", before, after)
	}
}
