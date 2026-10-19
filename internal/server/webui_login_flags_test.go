// webui_login_flags_test.go 登录页**标志位**的守卫（从 webui_login_test.go 拆出来）。
//
// 为什么单独一个文件：这条判据测的是"页面上的开关与服务端断定**同源**"，
// 与那个文件里另外那一大堆"密钥不许进 HTML / 302 换 cookie / 令牌轮换"
// 不是同一类东西。同一个文件里混着三四组互不相干的判据时，
// 改动其中一组的人会把另外几组的失败信息当成噪音。
package server

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// 登录页的标志位按真实状态渲染（bootstrap）。
//
// ⚠ 语义变更（2026-10-19 单账号口径）：原先 ③ 是"配置里开放注册 ⇒ register=true"，
// ② 与 ① 里还断言 `register` 的取值。那个开关（Config.RegisterOpen /
// config.console.disables_register）删了，`webui-login.html` 的
// `__WB2API_LOGIN_FLAGS__` 里的 `register` 项也被前端那一侧删了
// ⇒ 判据换成**更强**的一条（③）：标志位对象里**只有 bootstrap**，
// 页面上不存在任何"注册开关/角色开关"这种东西，也就**没有任何配置组合**
// 能再造出「bootstrap=false 而注册入口可见」这种自相矛盾的状态
// （改造前管理员关掉注册就能造出来：页面给"创建第一个管理员"按钮，
// 但 `/admin/auth/register` 会回 403 register_disabled）。
func TestUILoginPageFlags(t *testing.T) {
	boolFlag := func(rec *httptest.ResponseRecorder, key string) string {
		re := regexp.MustCompile(key + `:\s*(true|false)`)
		if m := re.FindStringSubmatch(rec.Body.String()); m != nil {
			return m[1]
		}
		return ""
	}
	// flagsObject 抠出 `window.__WB2API_LOGIN_FLAGS__ = { … }` 这一段的字面量。
	// 只在这个对象里找 register/member/role —— 页面别处当然会出现
	// `/admin/auth/register` 这样的端点路径与解释性注释，那不是开关。
	flagsObject := func(body string) string {
		i := strings.Index(body, "__WB2API_LOGIN_FLAGS__")
		if i < 0 {
			return ""
		}
		seg := body[i:]
		if j := strings.Index(seg, "};"); j >= 0 {
			return seg[:j]
		}
		return seg
	}

	// ① 账号库**有账号**（单账号口径下就是"已经建过那个管理员"）：
	//    bootstrap=false（不该再给创建入口）
	f1 := newUIFixture(t, true)
	rec1 := f1.get(t, "/ui")
	if got := boolFlag(rec1, "bootstrap"); got != "false" {
		t.Errorf("账号库非空时 bootstrap 应为 false，实际 %q", got)
	}

	// ② ★ 账号体系已接线但**一个账号都没有**（全新部署）⇒ bootstrap=true
	f2 := newUIEmptyAccountsFixture(t)
	rec := f2.get(t, "/ui")
	if got := boolFlag(rec, "bootstrap"); got != "true" {
		t.Errorf("★ 账号库为空时 bootstrap 应为 true（页面给出「创建第一个管理员」），实际 %q", got)
	}

	// ③ ★ 标志位对象里**只有 bootstrap** 一个键
	for _, f := range []*uiFixture{f1, f2, newUIFixture(t, false)} {
		obj := flagsObject(f.get(t, "/ui").Body.String())
		if obj == "" {
			t.Fatal("登录页里找不到 __WB2API_LOGIN_FLAGS__ —— 标志位对象的形状变了，本测试已失去守卫作用")
		}
		if !strings.Contains(obj, "bootstrap") {
			t.Errorf("标志位对象里必须有 bootstrap，实际 %s", obj)
		}
		if strings.Contains(obj, "register") || strings.Contains(obj, "member") || strings.Contains(obj, "role") {
			t.Errorf("★ 标志位对象里还有注册/角色相关的开关：%s ——\n"+
				"单账号口径下「能不能建号」只有一个答案（账号库空不空）；多一个开关就多一条\n"+
				"能造出「页面给入口但端点回 403」的路。", obj)
		}
	}

	// ④ 没接账号体系（Accounts=nil）⇒ bootstrap **必须**为 false。
	//
	// 这不是"没账号所以该显示创建入口"：没接线时 `/admin/auth/register`
	// 根本没挂载，页面给一个"创建第一个管理员"的按钮点了必然 404。
	// 宁可不显示入口，也不要显示一个点了就错的按钮。
	f4 := newUIFixture(t, false)
	if got := boolFlag(f4.get(t, "/ui"), "bootstrap"); got != "false" {
		t.Errorf("没接账号体系时 bootstrap 应为 false（创建入口此时不可用），实际 %q", got)
	}

	// ⑤ 占位符不得残留（否则页面上的标志位是字符串而非布尔）
	if strings.Contains(rec.Body.String(), `"`+loginBootstrapPlaceholder+`"`) {
		t.Errorf("登录页里仍残留占位符字面量 %s —— 标志位没被替换成布尔", loginBootstrapPlaceholder)
	}
}
