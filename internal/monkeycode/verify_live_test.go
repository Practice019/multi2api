// verify_live_test.go 真机验证：用真实凭证跑一条完整对话。
//
// ⚠ 这是 **live** 测试：需要 auths/monkeycode/ 里有真实凭证，否则 Skip。
// CI 不该因缺凭证而红（与 codearts 的 live 测试同款约定）。
//
// ★ 2026-11-01 补充约定：Skip 的判据从「凭证**不存在**」扩到「凭证**不可用**」
// （session 已失效 → 上游回 401/403）。理由与实际发生的事：
//
//	本仓 `auths/monkeycode/` 里 26 个凭证**都在，但 session 全过期** —— 而
//	AGENTS.md 的验证三件套是 `go test ./... -count=1`（不带 -short），于是
//	CI 一直红，看起来还像"谁把自己的改动改坏了"。live 测试要回答的问题本来就是
//	「**这台机器上有没有可用的**凭证」，没有就该 Skip。
//
//	⚠ 但判据只认 401/403：上游 5xx / 网关错误 / 网络全断**必须照旧红** ——
//	把真故障也 Skip 掉比一条红更危险（见 credDeadErr 的注释）。
package monkeycode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// ── 共享闸门：live 测试的「凭证可用性」判定 ──────────────────────────────────

// credDeadErr 这个错误是不是「凭证失效」—— live 测试里**唯一**允许 Skip 的情形。
//
// 判据刻意窄：只认上游明确的 401/403（`… 返回 401：Unauthorized` 这种原文）。
// **不认**的东西（遇到就必须红，不许 Skip）：
//
//	上游 5xx、网关 502/504、DNS/拨号失败、TLS 握手超时、响应解析失败。
//
// 后者都是真故障；把它们当"凭证问题"跳过，等于让 CI 对真事故静默。
func credDeadErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// 各条链路的原文形态不一：`… 返回 401：Unauthorized`、
	// `… 续期失败（password-login HTTP 403）：{…}`，两种都要认。
	for _, s := range []string{
		"返回 401", "返回 403", "HTTP 401", "HTTP 403",
		"401：", "403：", "Unauthorized", "Forbidden",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// liveProbeMax 探测多少个号仍全部失效，就下「这台机器没有可用凭证」的结论。
//
// 不把 26 个号都探一遍：结论一样，却要多等十几秒（每次探测都真打上游）。
const liveProbeMax = 5

// liveUsableCreds 返回该机器上**可用**的真实凭证；一个可用的都没有就 Skip 整个测试。
//
// needChat=true 时还要求具备对话材料（api_key + signing_secret）。
//
// 探测用一次最便宜的会话端点（FetchWallet，单请求）。若探测碰到**非凭证问题**
// （网络不通 / 上游 5xx）→ t.Fatalf：那是真故障，必须红（见 credDeadErr）。
func liveUsableCreds(t *testing.T, p *Provider, ctx context.Context, list []*Auth, needChat bool) []*Auth {
	t.Helper()
	alive := make([]*Auth, 0, len(list))
	probed, dead := 0, 0
	var lastErr error
	for _, a := range list {
		if needChat && !a.ChatUsable() {
			continue
		}
		probed++
		if _, err := p.client.FetchWallet(ctx, a); err != nil {
			if !credDeadErr(err) {
				t.Fatalf("凭证可用性探测遇到**非凭证问题**（这不属于可 Skip 的情形，必须红）: [%s] %v",
					a.RegionOrDefault(), err)
			}
			dead++
			lastErr = err
			if dead >= liveProbeMax {
				break
			}
			continue
		}
		alive = append(alive, a)
	}
	if len(alive) == 0 {
		t.Skipf("这台机器上没有可用凭证（探测 %d 个，其中 %d 个凭证已失效；最后一条：%v）",
			probed, dead, lastErr)
	}
	return alive
}

// TestCredDeadErrIsNarrow 判据守卫（**不联网**）：只有 401/403 才算「凭证失效」。
//
// 这条是整套 Skip 机制的**安全阀**：如果判据放宽成"任何错误都算凭证问题"，
// 上游 5xx / 网关 502 / DNS 挂掉都会被 Skip 掉 —— CI 会对真事故静默，
// 比一条红危险得多。所以这里把"必须红"的那几类逐个钉住。
func TestCredDeadErrIsNarrow(t *testing.T) {
	mustSkip := []string{
		"monkeycode: 查签到状态返回 401：Unauthorized",
		"monkeycode: models 返回 401：Unauthorized",
		"monkeycode: 返回 403：Forbidden",
		`monkeycode: 续期失败（password-login HTTP 403）：{"code":403}`,
	}
	for _, s := range mustSkip {
		if !credDeadErr(errors.New(s)) {
			t.Errorf("应判为凭证失效（可 Skip）：%q", s)
		}
	}

	mustNotSkip := []string{
		"monkeycode: 查签到状态返回 500：Internal Server Error",                           // 上游故障 → 必须红
		"monkeycode: models 返回 502：Bad Gateway",                                   // 网关故障 → 必须红
		"monkeycode: GET monkeycode-ai.net 失败 —— 国际站出口（直连）：dial tcp: i/o timeout", // 网络不通 → 必须红
		"monkeycode: GET monkeycode-ai.net 失败 —— 出口：lookup monkeycode-ai.net: no such host",
		"monkeycode: 解析 challenge 失败：unexpected end of JSON input",
		"monkeycode: PoW 求解超出上限",
	}
	for _, s := range mustNotSkip {
		if credDeadErr(errors.New(s)) {
			t.Errorf("★ 不该判为凭证失效（这类必须红，Skip 会藏住真故障）：%q", s)
		}
	}
	if credDeadErr(nil) {
		t.Error("nil 不是凭证失效")
	}
}

// liveSkipIfNothingTested 一个号都没测到（全是凭证失效）→ Skip 整条测试。
//
// 与 liveUsableCreds 的分工：
//
//	· liveUsableCreds 用于"必须先有一个可用号才能开工"的测试（对话 / 双区域），
//	  它自己做一次**廉价探测**（FetchWallet）筛出可用号；
//	· 本函数用于"逐号跑批"的测试（签到），那里用**测试真正要调的那个端点**的结果
//	  判凭证死活更准（实测：同一凭证 FetchWallet 通、CheckinStatus 却 401）。
func liveSkipIfNothingTested(t *testing.T, tested, dead int, lastErr error) {
	t.Helper()
	if tested == 0 {
		t.Skipf("这台机器上没有可用凭证（%d 个凭证已失效；最后一条：%v）", dead, lastErr)
	}
}

// TestLiveChat 真机跑一条对话，验证签名与双协议路由。
func TestLiveChat(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	dir := liveAuthDir(t)
	list, err := LoadDir(dir)
	if err != nil || len(list) == 0 {
		t.Skipf("无真实凭证（%s），跳过", dir)
	}

	p := New(Config{AuthDir: dir})
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 30*time.Second)
	list = liveUsableCreds(t, p, probeCtx, list, true)
	cancelProbe()
	a := list[0]
	cred := gateway.Credential{
		Provider: ProviderID,
		UID:      a.UID(),
		Nickname: a.DisplayName(),
		Secret:   a,
	}

	// 先拉目录，学协议归属。
	models, err := p.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("拉模型目录失败: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("模型目录为空")
	}
	t.Logf("模型目录 %d 条，首条 %s", len(models), models[0].ID)

	// 挑一个 basic 档的模型跑对话。
	model := models[0].ID
	for _, m := range models {
		if m.ID == "monkeycode-basic/qwen3.8-flash" || m.ID == "monkeycode-basic/deepseek-flash" {
			model = m.ID
			break
		}
	}
	t.Logf("用模型 %s（协议 %s）", model, interfaceOf(model))

	body := []byte(`{"model":"` + model + `","stream":true,
		"messages":[{"role":"user","content":"只回答两个字：收到"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	stream, err := p.Chat(ctx, cred, body)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer stream.Body.Close()
	if stream.Status != 200 {
		raw, _ := io.ReadAll(io.LimitReader(stream.Body, 500))
		t.Fatalf("HTTP %d: %s", stream.Status, raw)
	}

	got, err := io.ReadAll(io.LimitReader(stream.Body, 64*1024))
	if err != nil {
		t.Fatalf("读流失败: %v", err)
	}
	if !bytes.Contains(got, []byte("data:")) {
		t.Fatalf("返回里没有 SSE 帧: %s", got)
	}
	if !bytes.Contains(got, []byte("[DONE]")) {
		t.Errorf("流没有以 [DONE] 收尾")
	}
	t.Logf("收到 %d 字节 SSE，片段: %s", len(got), firstN(got, 200))
}

// firstN 取前 n 字节（日志用）。
func firstN(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// liveAuthDir 真机凭证目录（相对仓库根）。
//
// 找不到就是 Skip —— 不能因为"这台机器没放凭证"而让测试红。
func TestLiveRequiresOptIn(t *testing.T) {
	for _, value := range []string{"", "0", "true"} {
		t.Run("disabled="+value, func(t *testing.T) {
			t.Setenv("WB2API_LIVE_TEST", value)
			for name, run := range map[string]func(*testing.T){
				"chat":           TestLiveChat,
				"captcha":        TestLiveCaptcha,
				"regions":        TestLiveBothRegions,
				"checkin":        TestLiveCheckin,
				"checkin-status": TestLiveCheckinStatus,
			} {
				skipped := false
				t.Run(name, func(t *testing.T) {
					t.Cleanup(func() { skipped = t.Skipped() })
					run(t)
				})
				if !skipped {
					t.Errorf("%s did not skip without explicit opt-in", name)
				}
			}
		})
	}
}

func liveAuthDir(t *testing.T) string {
	t.Helper()
	if testing.Short() || os.Getenv("WB2API_LIVE_TEST") != "1" {
		t.Skip("live test requires WB2API_LIVE_TEST=1 and no -short")
	}
	candidates := []string{
		filepath.Join("..", "..", "auths", "monkeycode"),
		filepath.Join("auths", "monkeycode"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return candidates[0]
}
