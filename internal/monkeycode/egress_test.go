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
	if !strings.Contains(info.Note, "国际站必须走代理") {
		t.Errorf("note 应说明两站前提相反，实际 %q", info.Note)
	}

	// 没配代理：proxies 为空（前端据此只显示「直连」）。
	p2 := &Provider{client: NewClient()}
	if info2 := p2.Egress(); len(info2.Proxies) != 0 {
		t.Errorf("没配代理时 proxies 应为空，实际 %v", info2.Proxies)
	}
}

// TestProxylessIntlProbeTellsUserWhatToDo 没配代理时那一条要**直接给出结论**。
//
// 用户的动作只有两个：去设置里配代理，或改用国内站账号 —— 文案必须给到他。
func TestProxylessIntlProbeTellsUserWhatToDo(t *testing.T) {
	p := proxylessIntlProbe()
	if p.OK {
		t.Fatal("没配代理却报通 —— 这条是静态结论，不该发网络")
	}
	if p.Kind != gateway.EgressKindRefused {
		t.Errorf("类别 = %q，want %q", p.Kind, gateway.EgressKindRefused)
	}
	for _, want := range []string{"配", "代理"} {
		if !strings.Contains(p.Hint, want) {
			t.Errorf("提示里缺 %q：%q", want, p.Hint)
		}
	}
	if !strings.HasPrefix(p.Target, "https://monkeycode-ai.net") {
		t.Errorf("target 应指向国际站（排障要能抄进 curl），实际 %q", p.Target)
	}
}
