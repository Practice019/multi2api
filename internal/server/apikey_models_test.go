// apikey_models_test.go 模型白名单在**出口层**的守卫。
//
// # 用户的需求与它的三条边界
//
//	「API管理里面，我们设置那个API，我觉得需要设置这个API可以用哪些模型？」
//
// 一句话的需求，但有三个方向都危险的边界：
//
//	① 空白名单必须是「不限制」而不是「一个都不许」
//	   后者会把所有既有 key 与"没想过要限制"的新 key 一次性打死。
//
//	② 模型名必须按**裸名**比（剥掉上游前缀）
//	   客户端发 `glm-5.2` 与 `workbuddy/glm-5.2` 是同一个模型；逐字比会让
//	   用户在界面里填对了名字却被 403，而他完全看不出为什么。
//
//	③ /v1/models 必须**同时**裁剪
//	   否则用户看见几十个模型、调哪个都 403，会读成"网关坏了"。
//
// 存储层（规整、三态、持久化）在 internal/apikey 侧另有一份。
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/apikey"
)

// newKeyStore 造一个临时 apikey 存储。
func newKeyStore(t *testing.T) *apikey.Store {
	t.Helper()
	s, err := apikey.New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// reqAs 造一个"已通过鉴权、命中某把 key"的请求（与 withAuth 放进 ctx 的一致）。
func reqAs(t *testing.T, id string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if id != "" {
		r = r.WithContext(context.WithValue(r.Context(), apikeyCtxKey, id))
	}
	return r
}

// 空白名单 = 不限制（**不是**一个都不许）。
//
// ⚠ 这里只断言 nil 与空切片。**带空串的** `["", "  "]` 不在此列 ——
// 那种输入在**存储层写入时**就被 `normalizeModels` 丢掉了（见
// internal/apikey/models_test.go 的 TestCreateBlankModelsMeansUnrestricted）。
// 本层拿到的一定是已规整的列表，所以"里面有空白项"这个形态在这里不可达：
// 若真的传进来，它会被当成"受限于一个空名"而拒掉一切 —— 那是对的行为
// （空名匹配不上任何模型），只是不该由这层负责清理。
func TestModelAllowedEmptyMeansUnrestricted(t *testing.T) {
	for _, models := range [][]string{nil, {}} {
		if !modelAllowed(models, "任意模型") {
			t.Errorf("空白名单必须放行一切（%#v）—— "+
				"把它读成「一个都不许」会一次升级打死所有既有 key", models)
		}
	}
}

// 按**裸名**比：几种写法都指同一个模型。
func TestModelAllowedMatchesBareName(t *testing.T) {
	cases := []struct {
		allow []string
		model string
		want  bool
		why   string
	}{
		{[]string{"glm-5.2"}, "glm-5.2", true, "逐字相同"},
		{[]string{"glm-5.2"}, "workbuddy/glm-5.2", true, "客户端带了上游前缀，仍是同一个模型"},
		{[]string{"workbuddy/glm-5.2"}, "glm-5.2", true, "用户在白名单里写了前缀，裸名请求也该放行"},
		{[]string{"workbuddy/glm-5.2"}, "workbuddy/glm-5.2", true, "两边都带前缀"},
		{[]string{"glm-5.2"}, "glm-5.1", false, "不同模型"},
		{[]string{"glm-5.2"}, "codearts/glm-5.2-ultra", false, "裸名不同 ⇒ 拒"},
		{[]string{"glm-5.2", "deepseek-v4-flash"}, "deepseek-v4-flash", true, "多元素白名单"},
		{[]string{"glm-5.2"}, "", false, "空模型名：白名单生效时不放行"},
		{[]string{"glm-5.2"}, "  glm-5.2  ", true, "两侧空白要被容错"},
		// SplitModel 的实际边界（它的注释曾写错，已订正 —— 见 capability.go）：
		{[]string{"/auto"}, "/auto", true, "空前缀不剥，/auto 就是裸名 /auto"},
		{[]string{"auto"}, "/auto", false, "/auto 与 auto 是**不同**的名字（不剥前导斜杠）"},
		{[]string{"a/b/c"}, "a/b/c", true, "只切第一个 '/' ⇒ 两侧同为 (a, b/c)"},
		// ⚠ 已知局限：含 '/' 的白名单项会被解析成 provider/model，
		// 所以**没法**用它表达"裸名里本身带斜杠"（断言这个方向，
		// 是为了让将来想支持它的人先看到这里）。
		{[]string{"b/c"}, "a/b/c", false, "(b,c) 与 (a,b/c) 是不同模型"},
	}
	for _, c := range cases {
		if got := modelAllowed(c.allow, c.model); got != c.want {
			t.Errorf("modelAllowed(%v, %q) = %v，want %v —— %s", c.allow, c.model, got, c.want, c.why)
		}
	}
}

// 被限制的 key 调别的模型 ⇒ 403，且错误体里要**写明允许什么**。
func TestModelAllowedForRequestRejectsWithHelpfulBody(t *testing.T) {
	keys := newKeyStore(t)
	k, _ := keys.Create("app", 0, 0, []string{"glm-5.2", "deepseek-v4-flash"})
	h := &Handler{cfg: Config{APIKeys: keys}}

	r := reqAs(t, k.ID)
	w := httptest.NewRecorder()
	if h.modelAllowedForRequest(w, r, "gpt-5.5") {
		t.Fatal("白名单外的模型必须被拒")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d，want 403", w.Code)
	}
	body := w.Body.String()
	// 这条断言**反过来了**（用户实测的泄露 1）。
	//
	// 原断言要求 403 列出白名单里的模型名。而白名单里存的是内部名
	//（workbuddy/glm-5.2、loomy/...），列出来等于公布上游身份：
	//
	//	这把 API key 不允许调用模型 ；允许的模型：csg/deepseek-v4.1-flash-1017、
	//	loomy/DeepSeek-V4.1-Flash、workbuddy/deepseek-v4.1-flash、...
	//
	// 而且客户端拿到这份清单后可以**直接用真名调用**（真名是全局可用的，
	// 别名只是按 key 的虚拟命名空间），整套脱敏被一步绕过。
	//
	// 现在只回数量，不回名字。要看具体能调什么走 /v1/models（那里回别名）。
	for _, leak := range []string{"glm-5.2", "deepseek-v4-flash"} {
		if strings.Contains(body, leak) {
			t.Errorf("★ 403 泄露了白名单模型名 %q（等于公布上游身份），实际: %s", leak, body)
		}
	}
	if !strings.Contains(body, "model_not_allowed") {
		t.Errorf("应带稳定的错误码 model_not_allowed，实际: %s", body)
	}
	if !strings.Contains(body, "2 个模型") {
		t.Errorf("应当回数量让用户知道是权限问题，实际: %s", body)
	}

	// 白名单内（带前缀写法）放行，且不写错误体。
	w2 := httptest.NewRecorder()
	if !h.modelAllowedForRequest(w2, r, "workbuddy/glm-5.2") {
		t.Error("白名单内（带前缀写法）必须放行")
	}
	if w2.Body.Len() != 0 {
		t.Errorf("放行时不该写任何响应体，实际: %s", w2.Body.String())
	}
}

// 没有普通 key 命中（管理钥匙）⇒ **不受限**。
//
// 管理钥匙是运维/调试用的那把；被白名单挡住会让排障时连模型列表都拿不到。
func TestModelAllowedForRequestUnrestrictedForAdminKey(t *testing.T) {
	keys := newKeyStore(t)
	_, _ = keys.Create("app", 0, 0, []string{"glm-5.2"})
	h := &Handler{cfg: Config{APIKeys: keys}}

	w := httptest.NewRecorder()
	if !h.modelAllowedForRequest(w, reqAs(t, ""), "任何模型") {
		t.Error("没有命中普通 key（管理钥匙）时必须不受限")
	}
	if w.Code != http.StatusOK {
		t.Errorf("不该写任何错误体，实际状态码 %d", w.Code)
	}
}

// 未启用 APIKeys 存储时也不受限（单 key 部署的既有行为）。
func TestModelAllowedForRequestNoStoreIsUnrestricted(t *testing.T) {
	h := &Handler{cfg: Config{}}
	w := httptest.NewRecorder()
	if !h.modelAllowedForRequest(w, reqAs(t, "sk-whatever"), "任何模型") {
		t.Error("未启用 API key 存储时不该限制任何模型")
	}
}

// 模型列表按白名单裁剪（**不**限制时原样返回同一个切片）。
func TestFilterModelsForRequest(t *testing.T) {
	keys := newKeyStore(t)
	k, _ := keys.Create("app", 0, 0, []string{"glm-5.2"})
	h := &Handler{cfg: Config{APIKeys: keys}}

	list := []map[string]any{
		{"id": "workbuddy/glm-5.2"},
		{"id": "workbuddy/glm-5.1"},
		{"id": "glm-5.2"},
		{"id": "codearts/GLM-5.2"},
	}

	// 受限：只剩裸名为 glm-5.2 的那两个。
	got := h.filterModelsForRequest(reqAs(t, k.ID), list)
	if len(got) != 2 {
		t.Fatalf("应剩 2 个（workbuddy/glm-5.2 与 glm-5.2），实际 %v", got)
	}

	// 不受限：原样返回**同一个**切片（不复制）。
	got2 := h.filterModelsForRequest(reqAs(t, ""), list)
	if len(got2) != len(list) {
		t.Errorf("不限制时不该裁剪，实际 %v", got2)
	}
	if &got2[0] != &list[0] {
		t.Error("不限制时应当原样返回同一个切片（这条路径每个客户端都会走，无谓复制是白花钱）")
	}
}

// joinLimited 超长时截断并报总数（错误体不该变成一堵墙）。
func TestJoinLimitedTruncates(t *testing.T) {
	if got := joinLimited([]string{"a", "b", "c", "d"}, 2); !strings.Contains(got, "共 4 个") {
		t.Errorf("超长应截断并报总数，实际 %q", got)
	}
	if got := joinLimited([]string{"a"}, 2); got != "a" {
		t.Errorf("不超长时应原样连接，实际 %q", got)
	}
}

// TestModelAllowedRespectsProviderPrefix 白名单带前缀时必须精确匹配上游。
//
// # 用户报的 bug（实测复现）
//
// 白名单 9 条、别名映射 9 条，但客户的模型列表里出现**第 10 个**：
//
//	workbuddy-intl/deepseek-v4.1-flash     ← 多出来的
//
// 而白名单里只有 workbuddy/deepseek-v4.1-flash。
//
// 根因：旧实现只比剥过前缀的裸名。SplitModel 按**第一个** / 切，
// 于是 workbuddy 与 workbuddy-intl 得到同一个裸名 deepseek-v4.1-flash，
// 被当成「同一个模型」放行 —— 可它们是两个独立上游（不同账号池、
// 不同额度）。用户在列表里看到 -intl 那条还能调通，那是他没授权的上游。
func TestModelAllowedRespectsProviderPrefix(t *testing.T) {
	allowed := []string{"workbuddy/deepseek-v4.1-flash"}

	// 同上游：放行。
	if !modelAllowed(allowed, "workbuddy/deepseek-v4.1-flash") {
		t.Error("同上游同名模型应当放行")
	}
	// ★ 不同上游、同裸名：**必须拒绝**（这就是那个多出来的第 10 个）。
	if modelAllowed(allowed, "workbuddy-intl/deepseek-v4.1-flash") {
		t.Error("★ workbuddy-intl 与 workbuddy 是两个上游，白名单只授权了前者 —— " +
			"放行会让客户的模型列表多出一条他没授权的条目（用户实测报的 bug）")
	}
	// 白名单是裸名时表示不限上游：仍然放行任意前缀。
	if !modelAllowed([]string{"deepseek-v4.1-flash"}, "workbuddy-intl/deepseek-v4.1-flash") {
		t.Error("白名单写裸名时应匹配任意上游（用户明确表达了「哪个上游都行」）")
	}
	// 请求是裸名时也不限定上游。
	if !modelAllowed(allowed, "deepseek-v4.1-flash") {
		t.Error("请求发裸名时应匹配白名单里的同名条目")
	}
}

// TestModelAllowedMultiLevelProvider 两级前缀（monkeycode/monkeycode-basic/x）也要对。
//
// # 为什么单独测
//
// SplitModel 只切**第一个** /，所以 providerID 可能是 `monkeycode`、
// 而模型名是 `monkeycode-basic/deepseek-flash`。前缀比较必须用完整的
// providerID，不能把模型名里的 / 也算进上游。
func TestModelAllowedMultiLevelProvider(t *testing.T) {
	allowed := []string{"monkeycode/monkeycode-basic/deepseek-flash"}
	if !modelAllowed(allowed, "monkeycode/monkeycode-basic/deepseek-flash") {
		t.Error("两级模型名应当精确匹配通过")
	}
	// 同 provider、不同档位：不应当放行。
	if modelAllowed(allowed, "monkeycode/monkeycode-pro/deepseek-flash") {
		t.Error("monkeycode-basic 与 monkeycode-pro 是不同模型，不该互相放行")
	}
}
