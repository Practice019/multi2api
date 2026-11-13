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
		if failInst != "" && strings.Contains(key, ":"+failInst+":") {
			writeJSONTest(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal"}})
			return
		}
		// ★ 体例与签到端点同款：body 必须是**空对象**、幂等键必须是 referral-code:<instID>:<YYYY-MM-DD>。
		if strings.TrimSpace(string(raw)) != "{}" {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code": "bad_body", "message": "body must be an empty JSON object, got: " + string(raw)}})
			return
		}
		if !strings.HasPrefix(key, "referral-code:") || strings.Count(key, ":") != 2 {
			writeJSONTest(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"code": "bad_idem", "message": "idempotency-key 期望 referral-code:<installationId>:<YYYY-MM-DD>，实际 " + key}})
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
		f.mu.Unlock()
		day := f.serverDate
		if day == "" {
			day = time.Now().UTC().Add(8 * time.Hour).Format("2006-01-02") // UTC+8 的日键（服务端口径）
		}
		body := map[string]any{
			"schema_version":               1,
			"server_time":                  time.Now().UTC().Format(time.RFC3339Nano),
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
	if len(strings.Split(canonical, "\n")) != 7 {
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
	if len(strings.Split(canonical, "\n")) != 12 {
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
