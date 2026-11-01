package monkeycode

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestProbeEgressTreatsAnyHTTPResponseAsReachable 出口探测的判据是「能不能拿到 HTTP 响应」。
//
// # 这是整套自检的价值所在
//
// 用**不带凭证**的请求探：401/403 也算通（DNS + TCP + TLS + HTTP 全通了）。
// 若把 401 记成「出口不通」，用户会去修代理；而真正的问题是他的凭证过期。
// 两类动作完全不同 —— 所以这条判据必须有测试钉住。
func TestProbeEgressTreatsAnyHTTPResponseAsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // 无凭证的正常回应
		_, _ = w.Write([]byte(`{"code":401}`))
	}))
	defer srv.Close()

	p := probeEgress(context.Background(), "测试出口", srv.URL, srv.Client(), "直连")
	if !p.OK {
		t.Fatalf("401 应判出口**通**（判据是能不能拿到 HTTP 响应），实际 ok=%v kind=%q detail=%q",
			p.OK, p.Kind, p.Detail)
	}
	if p.Kind != "" || p.Hint != "" {
		t.Errorf("通了就不该带失败类别，实际 kind=%q hint=%q", p.Kind, p.Hint)
	}
	if p.MS < 0 {
		t.Errorf("耗时不该为负：%d", p.MS)
	}
}

// TestProbeEgressClassifiesRefused 端口没人听 ⇒ refused + 一句人话。
//
// 现场就是这条：本机代理进程没起来，127.0.0.1:10808 直接拒绝连接。
func TestProbeEgressClassifiesRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 关掉 ⇒ 端口没人听

	client := &http.Client{Timeout: 3 * time.Second}
	// ⚠ 等一秒再探：部分平台上刚关闭的端口会先给 RST 也可能短暂time_wait
	p := probeEgress(context.Background(), "代理", "http://"+addr+"/x", client, "代理 http://"+addr)
	if p.OK {
		t.Fatal("端口已关闭却报出口通 —— 探测判据失效")
	}
	if p.Kind != gateway.EgressKindRefused {
		t.Errorf("类别 = %q，want %q（detail=%q）", p.Kind, gateway.EgressKindRefused, p.Detail)
	}
	if p.Hint == "" {
		t.Error("失败必须带一句可操作的话")
	}
}

// TestEgressInfoReportsStaticFacts 自报出口是静态事实（不发网络）。
func TestEgressInfoReportsStaticFacts(t *testing.T) {
	// 配了代理：应当报出代理地址 + 直连，并带上「两站前提相反」那句说明。
	p := &Provider{client: NewClientWithProxy("http://127.0.0.1:10808")}
	info := p.Egress()
	if !info.Direct {
		t.Error("国内站是直连的 ⇒ direct 必须为 true")
	}
	if len(info.Proxies) != 1 || info.Proxies[0] != "http://127.0.0.1:10808" {
		t.Errorf("proxies = %v，want [http://127.0.0.1:10808]", info.Proxies)
	}
	// Note 的价值是**说清两站的差异**，所以两个站名都必须在。
	note := info.Note
	for _, want := range []string{"国内站", "国际站"} {
		if !strings.Contains(note, want) {
			t.Errorf("note 应说明两站的差异（缺 %q），实际 %q", want, note)
		}
	}
	// ★ 新口径（2026-11-01 用户实测推翻旧前提）：国际站**可以直连**，代理只是
	//   "本机直连不通时"的可选项。旧文案「国际站必须走代理（否则连不上）」是错的。
	//   具体措辞以 T-023 落地为准，这里接受几种等价说法。
	if !strings.Contains(note, "可直连") && !strings.Contains(note, "也可以直连") &&
		!strings.Contains(note, "直连或走代理") && !strings.Contains(note, "直连亦可") {
		t.Errorf("note 应说明国际站**可直连**（代理只是可选），实际 %q", note)
	}
	// 反向断言：绝对化措辞必须消失（用户原话：「不一定非要走代理」）。
	for _, bad := range []string{"必须走代理", "必须配代理", "否则连不上"} {
		if strings.Contains(note, bad) {
			t.Errorf("note 仍含旧前提 %q（实测：零代理可直连国际站），实际 %q", bad, note)
		}
	}

	// 没配代理：proxies 为空（前端据此只显示「直连」）。
	p2 := &Provider{client: NewClient()}
	if info2 := p2.Egress(); len(info2.Proxies) != 0 {
		t.Errorf("没配代理时 proxies 应为空，实际 %v", info2.Proxies)
	}
}

// TestProxylessIntlProbeTellsUserWhatToDo 没配代理时那一条**不再静态判定失败**。
//
// ★ 新口径（2026-11-01 用户实测）：国际站零代理也能用（401/201/200 都拿到过），
// 所以"没配代理 ⇒ 一定连不上"是**错的结论**。现在这条只给一句「未配置代理（直连）」
// 的待实测说明 + 可操作建议（直连不通再去配代理），由实测决定通不通。
func TestProxylessIntlProbeTellsUserWhatToDo(t *testing.T) {
	p := proxylessIntlProbe()
	if !strings.HasPrefix(p.Target, "https://monkeycode-ai.net") {
		t.Errorf("target 应指向国际站（排障要能抄进 curl），实际 %q", p.Target)
	}
	// Hint 必须给**可操作**建议：既提"直连"这条路，也提"代理"这个备选。
	for _, want := range []string{"直连", "代理"} {
		if !strings.Contains(p.Hint, want) {
			t.Errorf("提示里缺 %q（新口径应同时给「直连」与「配代理」两条路）：%q", want, p.Hint)
		}
	}
	// 不再静态下结论：没配代理不等于"被拒"（那是旧的硬编码判定）。
	if p.Kind == gateway.EgressKindRefused {
		t.Errorf("没配代理不该静态判成 refused（可直连，需实测），实际 kind=%q hint=%q", p.Kind, p.Hint)
	}
	// 反向断言：不得出现绝对化措辞（用户实测推翻了它）。
	for _, bad := range []string{"必须走代理", "一定连不上", "必须配代理"} {
		if strings.Contains(p.Hint, bad) {
			t.Errorf("提示里仍有绝对化措辞 %q：%q", bad, p.Hint)
		}
	}
}
