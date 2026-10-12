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
	"os"
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
	if !strings.Contains(body, "glm-5.2") {
		t.Errorf("403 必须写明**允许的模型**（调用方最需要知道的就是这个），实际: %s", body)
	}
	if !strings.Contains(body, "model_not_allowed") {
		t.Errorf("应带稳定的错误码 model_not_allowed，实际: %s", body)
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

// itoa 的边界（错误文案里用得到，写错会让"共 10 个"变成空）。
func TestItoa(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 10: "10", 123: "123"}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q，want %q", in, got, want)
		}
	}
}

// ── 前端接线守卫 ────────────────────────────────────────────────────────
//
// # 为什么需要它（本会话踩过的形态）
//
// API 管理页的前端是**嵌在二进制里**的（webui.html 走 go:embed），
// 而后端接口与页面之间只靠**约定**连着：
//
//	后端返回 k.models       ←→  前端读 k.models
//	data-act="models"       ←→  事件委托里有 act === 'models' 分支
//
// 任何一处改名，另一处**不会报错**：页面上那一列空着、那个按钮点了没反应。
// 所以这里把"用户点名要的那些界面元素"逐个钉住。
func TestAPIKeyModelsUIIsWired(t *testing.T) {
	raw, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	src := string(raw)

	need := []struct {
		what   string
		needle string
	}{
		{"表头「可用模型」", "<th>可用模型</th>"},
		{"列渲染函数", "function apiKeyModelsCell("},
		{"单元格里调用它", "${apiKeyModelsCell(k)}"},
		{"输入解析函数", "function parseModelList("},
		{"新建时询问模型", "允许调用的模型（逗号或换行分隔；**留空 = 不限制**）"},
		{"行内「改模型」按钮", `data-act="models"`},
		{"事件委托里的改模型分支", "act === 'models'"},
		{"新建请求带上 models", "JSON.stringify({ name, limit, rpm, models })"},
	}
	for _, n := range need {
		if !strings.Contains(src, n.needle) {
			t.Errorf("webui.html 缺少%s（%q）—— 后端会照发，而那一列/那个按钮不会出现，"+
				"且**没有任何报错**", n.what, n.needle)
		}
	}

	// ⚠ 三态：取消（null）与清空（[]）必须分开处理。
	//
	// 混在一起的话，"点一次取消"会被当成"清空白名单" ——
	// 用户什么都没改，模型限制却被静默解开了。
	fn := src[strings.Index(src, "function parseModelList("):]
	if i := strings.Index(fn, "\n  }"); i > 0 {
		fn = fn[:i]
	}
	if !strings.Contains(fn, "=== null") {
		t.Error("parseModelList 必须把「取消」与「清空」分开（判据是 === null）——\n" +
			"否则用户点取消就等于清空了白名单，而他什么都没改")
	}
	if !strings.Contains(src, "models === null") {
		t.Error("「改模型」分支必须显式处理取消（models === null ⇒ 不发请求）")
	}
}
