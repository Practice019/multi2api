package admin

// apikeys_credential_guard_test.go —— T-459：完整凭据响应的安全守卫。
//
// # 为什么需要这一组
//
// `/admin/apikeys` 会回吐 **完整的管理令牌**（`admin_token_full`）与
// **完整的对话密钥**（`admin_key_full`，config.api_key，不限额、全模型）。
// 拿到这个响应 = 拿到网关的钥匙（对话密钥泄露只是白用额度，性质不同）。
//
// 代码里已有一条注释承诺（apikeys.go L79-83）：
//
//	「做法是在**会话鉴权之后**回吐完整值…页面 HTML 里不留凭据 ⇒ 截图、另存为、
//	  **浏览器缓存**、扩展都带不出去。」
//
// ⇒ 但"浏览器缓存带不出去"这句话，只有配上 `Cache-Control: no-store` 才成立 ✗
//    （浏览器 bfcache 会把响应留到下次前进/后退；中间代理可能落盘）。
//    本组测试把这句话变成**可执行的契约**。

import (
	"encoding/json"
	"strings"
	"testing"
)

// listRaw 打一次 /admin/apikeys 并原样返回响应（头 + 体都在）。
func listRaw(t *testing.T) (cc string, d map[string]any) {
	t.Helper()
	f := newAdminKeyFixture(t)
	rec := f.call(t, "GET", "/admin/apikeys", "")
	if rec.Code != 200 {
		t.Fatalf("GET /admin/apikeys = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	return rec.Header().Get("Cache-Control"), d
}

// TestAPIKeysCredentialResponseNeverCached 完整凭据**不得进任何缓存**。
func TestAPIKeysCredentialResponseNeverCached(t *testing.T) {
	cc, d := listRaw(t)
	if cc != credentialCacheControl {
		t.Errorf("Cache-Control = %q，want %q ——"+
			"这个响应带着完整管理令牌，进了 bfcache/代理就是把它留在磁盘上"+
			"（apikeys.go 的注释已经承诺「浏览器缓存带不出去」，但没有这个头就不成立）", cc, credentialCacheControl)
	}
	// 掩码必须**短于**完整值（否则前端拿到的"完整值"其实还是掩码）
	mask, _ := d["admin_token"].(string)
	full, _ := d["admin_token_full"].(string)
	if mask == "" || full == "" {
		t.Fatalf("掩码/完整值应当都非空：mask=%q full=%q（fixture 配了 AdminToken）", mask, full)
	}
	if len(full) <= len(mask) {
		t.Errorf("len(admin_token_full)=%d 必须 > len(admin_token)=%d（否则前端「显示完整值」显示的还是掩码）",
			len(full), len(mask))
	}
	if !strings.HasPrefix(full, mask[:min(4, len(mask))]) {
		t.Errorf("掩码 %q 应当是完整值 %q 的前缀（可辨认「我用的是哪一把」）", mask, full[:8]+"…")
	}
	t.Logf("Cache-Control=%q  admin_token=%q(len=%d)  admin_token_full=len%d",
		cc, mask, len(mask), len(full))
}

// TestAPIKeysMaskedAndFullAlwaysBothPresent 掩码与完整值**要么都给、要么都不给**。
//
// 前端 T-458 按 `admin_token_full` 是否存在决定渲不渲染「显示/复制」按钮
// ⇒ 后端若某天只给掩码（比如有人"出于安全"删掉 _full），前端会**静默降级**
//
//	⇒ 用户刚为此报过障 ✗ ⇒ 这条是前后端契约的守卫。
func TestAPIKeysMaskedAndFullAlwaysBothPresent(t *testing.T) {
	_, d := listRaw(t)
	for _, pair := range [][2]string{{"admin_token", "admin_token_full"}, {"admin_key", "admin_key_full"}} {
		mask, _ := d[pair[0]].(string)
		full, hasFull := d[pair[1]]
		_, hasMask := d[pair[0]]
		if hasMask != hasFull {
			t.Errorf("%s 与 %s 必须同时出现（存在性：mask=%v full=%v）"+
				"—— 只给一个会让前端静默降级成「看不到完整值」", pair[0], pair[1], hasMask, hasFull)
		}
		// 空值边界：不能出现"掩码有值、完整值是空串"这种怪组合
		if mask != "" {
			if s, _ := full.(string); s == "" {
				t.Errorf("%s=%q 非空但 %s 是空串 ⇒ 前端会渲染按钮却复制到空", pair[0], mask, pair[1])
			}
		}
	}
	t.Logf("成对存在 ✓  admin_key=%q / admin_token=%q",
		d["admin_key"], d["admin_token"])
}

// TestAPIKeysEmptyKeyStaysConsistentlyEmpty 未配置 api_key ⇒ 两个字段**一致地**为空。
func TestAPIKeysEmptyKeyStaysConsistentlyEmpty(t *testing.T) {
	f := newAdminKeyFixture(t)
	f.h.cfg.APIKey = "" // 模拟"未配置 api_key"（实测当前就是空）
	rec := f.call(t, "GET", "/admin/apikeys", "")
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d["admin_key"] != "" || d["admin_key_full"] != "" {
		t.Errorf("api_key 未配置时两者都应为空串，得到 admin_key=%q admin_key_full=%q",
			d["admin_key"], d["admin_key_full"])
	}
	// 而 admin_token 那一对仍然有值（它与管理钥匙互相独立）
	if s, _ := d["admin_token_full"].(string); s == "" {
		t.Error("admin_token_full 不该因为 api_key 为空而变空（两者互相独立）")
	}
}
