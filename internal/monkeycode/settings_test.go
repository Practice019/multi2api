package monkeycode

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSettingsFieldsReportCurrentEgress 自报的两个出口就是 client 当前用的。
//
// 判据不能只看"字段存在"：设置页显示的值必须与**实际在用的**一致，
// 否则用户看到「代理 http://…」而请求其实直连 —— 那比没有这个设置更糟。
func TestSettingsFieldsReportCurrentEgress(t *testing.T) {
	p := &Provider{client: NewClientWithProxy("http://127.0.0.1:10808")}
	fields := p.SettingsFields()
	if len(fields) != 2 {
		t.Fatalf("字段数 = %d，want 2（国际站代理 + 国内站强制代理）", len(fields))
	}
	if fields[0].Key != KeyProxy || fields[0].Value != "http://127.0.0.1:10808" {
		t.Errorf("第一个字段 = %+v，want key=%s value=http://127.0.0.1:10808", fields[0], KeyProxy)
	}
	if fields[1].Key != KeyDirectProxy || fields[1].Value != "" {
		t.Errorf("第二个字段 = %+v，want key=%s value=空", fields[1], KeyDirectProxy)
	}
	// 热生效 ⇒ 不需要重启（要求重启会让用户改完不知道生效没）。
	for _, f := range fields {
		if f.RestartRequired {
			t.Errorf("%s 标成了需重启 —— 出口是 transport 级的，换掉即可生效", f.Key)
		}
	}
}

// TestApplySettingsSwapsTransportForever 保存后**立刻**换掉 transport，且下一次读回是新值。
func TestApplySettingsSwapsTransportForever(t *testing.T) {
	p := &Provider{client: NewClientWithProxy("")}
	applied, err := p.ApplySettings(json.RawMessage(`{"monkeycode_proxy":"127.0.0.1:10808"}`))
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if len(applied) != 1 || applied[0] != KeyProxy {
		t.Errorf("applied = %v，want [%s]", applied, KeyProxy)
	}
	// ① 值归一（省略 scheme 也认 —— 多数人从别处复制来的就是 host:port）
	if got := p.SettingsFields()[0].Value; got != "http://127.0.0.1:10808" {
		t.Errorf("保存后的值 = %q，want http://127.0.0.1:10808", got)
	}
	// ② 真的换了 transport（不是只改了字符串）
	tr, ok := p.client.proxied.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("国际站 client 的 transport 上没有 Proxy 函数 —— 只改了显示、没改行为")
	}
	// ③ 直连那一侧不受影响
	dtr, _ := p.client.direct.Transport.(*http.Transport)
	if dtr != nil && dtr.Proxy != nil {
		t.Error("国内站 client 不该被装上代理（它与国际站的网络前提相反）")
	}
}

// TestApplySettingsValidatesProxyURL 填错要在**保存那一刻**就报错。
//
// # 为什么这不是"过度校验"
//
// 不校验的话，写错一个字符的表现是「所有国际站请求都超时」，而错误文案里那句
// 「代理 http://…」看起来完全正常 —— 用户会去查上游，而不是查自己刚填的值。
func TestApplySettingsValidatesProxyURL(t *testing.T) {
	for _, c := range []struct{ in, wantMsg string }{
		{`{"monkeycode_proxy":"ftp://1.2.3.4:21"}`, "协议"},
		{`{"monkeycode_proxy":"http://"}`, "缺少主机名"},
		{`{"monkeycode_proxy":123}`, "字符串"},
		{`{"monkeycode_direct_proxy":"gopher://x:1"}`, "协议"},
	} {
		p := &Provider{client: NewClientWithProxy("")}
		_, err := p.ApplySettings(json.RawMessage(c.in))
		if err == nil {
			t.Errorf("%s 应当被拒（%s），实际通过了", c.in, c.wantMsg)
			continue
		}
		if !strings.Contains(err.Error(), c.wantMsg) {
			t.Errorf("错误信息 %q 里没有 %q —— 用户看不出该怎么改", err.Error(), c.wantMsg)
		}
	}
}

// TestApplySettingsEmptyClearsProxy 空串 = 直连（用户关掉代理的合法途径）。
func TestApplySettingsEmptyClearsProxy(t *testing.T) {
	p := &Provider{client: NewClientWithProxy("http://127.0.0.1:10808")}
	if _, err := p.ApplySettings(json.RawMessage(`{"monkeycode_proxy":""}`)); err != nil {
		t.Fatalf("清空代理应当合法: %v", err)
	}
	if got := p.SettingsFields()[0].Value; got != "" {
		t.Errorf("清空后 value = %q，want 空", got)
	}
	tr, _ := p.client.proxied.Transport.(*http.Transport)
	if tr != nil && tr.Proxy != nil {
		t.Error("清空后国际站仍挂着代理函数 —— 用户会以为改了没生效")
	}
}

// TestApplySettingsIgnoresForeignKeys 别人的键不认（也不报错）。
//
// 核心会把**整份补丁**交给每个上游，忽略不认得的键是契约的一部分。
func TestApplySettingsIgnoresForeignKeys(t *testing.T) {
	p := &Provider{client: NewClientWithProxy("")}
	applied, err := p.ApplySettings(json.RawMessage(`{"growth_auto_open":true,"checkin_enabled":false}`))
	if err != nil {
		t.Fatalf("不认得的键不该报错: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %v，want 空（没有任何键属于本上游）", applied)
	}
}
