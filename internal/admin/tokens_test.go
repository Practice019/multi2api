// tokens_test.go /admin/tokens（Wave B4）的 handler 级守卫。
//
// 钉住四件事：
//
//	① 明文 key **只**从两条敏感端点出来（列表/详情/创建响应都只能有掩码）——
//	   这是本文件最重要的一条：列表是轮询的，漏一次就是每次轮询都漏一遍。
//	② count>1 的语义：名称后缀、43 字节上限、**部分成功如实报告**。
//	③ 校验：名称必填、count 1..1000、manual 至少一组、auto custom 有上限、IP 逐行合法。
//	④ 敏感端点的三道闸：只此两条、no-store、限流（429 + Retry-After）。
//
// 注册方式：**依赖生产那一行**（admin.go 的 New 里调 h.registerTokens()），
// 测试不自己注册 —— 这样「路由到底存不存在」本身也在被测范围内。
// 前提是 store.Open + Migrate 在 New 之前完成（见 tokenTestHandler），与生产装配顺序一致。
//
// ⚠ 请求体一律用 json.Marshal 造（不手写带引号的 JSON 字面量）：手写那种字面量
// 在 Go 里要层层转义，正是本仓"String.raw 里一个裸反引号毁掉整个模板"的同一类坑。
package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/authlimit"
	"workbuddy2api/internal/store"
)

func tokenTestHandler(t *testing.T) *Handler {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// ⚠ 不显式注册：New() 会调 registerTokens()（store.DB 已开）⇒ 下面所有用例都在验
	// 「生产装配真的把路由接上了」这件事本身。
	return New(Config{AdminToken: testAdminToken, ListensLoopback: true})
}

// jsonBody 把 map 编成请求体（所有请求体都从这里过）。
func tokenJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(raw)
}

// createBody 最小可用创建体（manual 模式 + 一个分组），extra 覆盖同名键。
func createBody(t *testing.T, name string, extra map[string]any) string {
	body := map[string]any{"name": name, "automatic": false, "groups": []string{"default"}}
	for k, v := range extra {
		body[k] = v
	}
	return tokenJSON(t, body)
}

func decodeMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v / %s", err, raw)
	}
	return out
}

func tokenRecord(t *testing.T, h *Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withTestAuth(req))
	return rec
}

// ① 列表/详情不得出现明文；但必须出现掩码。
func TestTokensListAndDetailNeverLeakPlaintext(t *testing.T) {
	h := tokenTestHandler(t)
	key, err := store.GenerateTokenKey()
	if err != nil {
		t.Fatalf("GenerateTokenKey: %v", err)
	}
	tok := &store.Token{Name: "明文对照", Key: key, Status: store.TokenStatusEnabled, ExpiredTime: -1,
		Group: store.EncodeTokenGroups([]string{"default"})}
	if err := store.CreateToken(tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	code, listBody := doJSON(t, h, "GET", "/admin/tokens?p=1&page_size=50", "")
	if code != 200 {
		t.Fatalf("列表失败 code=%d body=%s", code, listBody)
	}
	if strings.Contains(listBody, key) {
		t.Fatalf("列表响应里出现了明文 key！")
	}
	if !strings.Contains(listBody, store.MaskTokenKey(key)) {
		t.Fatalf("列表响应里没有掩码 key，前端无法辨识")
	}
	code, detailBody := doJSON(t, h, "GET", fmt.Sprintf("/admin/tokens/%d", tok.ID), "")
	if code != 200 {
		t.Fatalf("详情失败 code=%d body=%s", code, detailBody)
	}
	if strings.Contains(detailBody, key) {
		t.Fatalf("详情响应里出现了明文 key！")
	}
	view := decodeMap(t, detailBody)
	for _, k := range []string{"allow_user_quota_query", "groups", "group_switch_status_codes", "model_limits"} {
		if _, ok := view[k]; !ok {
			t.Errorf("详情缺字段 %s（截图抽屉要靠它回填）", k)
		}
	}
}

// ② count>1 的名称后缀与 43 字节上限。
func TestTokensCreateCountSemantics(t *testing.T) {
	h := tokenTestHandler(t)
	code, body := doJSON(t, h, "POST", "/admin/tokens", createBody(t, "批量建", map[string]any{"count": 3}))
	if code != 200 {
		t.Fatalf("count=3 创建失败 code=%d body=%s", code, body)
	}
	res := decodeMap(t, body)
	if res["created"].(float64) != 3 || res["failed"].(float64) != 0 || res["partial"].(bool) {
		t.Fatalf("count=3 结果不对: %s", body)
	}
	items := res["items"].([]any)
	base := "批量建"
	first := items[0].(map[string]any)["name"].(string)
	if first != base {
		t.Errorf("第 1 条应保持原名，实际 %q", first)
	}
	seen := map[string]bool{first: true}
	for i, raw := range items[1:] {
		name := raw.(map[string]any)["name"].(string)
		if !strings.HasPrefix(name, base+"-") || len(name) != len(base)+7 {
			t.Errorf("第 %d 条应带 - 加 6 位短码，实际 %q", i+2, name)
		}
		if seen[name] {
			t.Errorf("名称重复: %q", name)
		}
		seen[name] = true
	}
	// 上限：单条 50 字节可以；count>1 时同名必须被拒（43 上限）。
	long := strings.Repeat("a", 50)
	if code, body := doJSON(t, h, "POST", "/admin/tokens", createBody(t, long, nil)); code != 200 {
		t.Errorf("单条 50 字节应通过，实际 code=%d body=%s", code, body)
	}
	if code, body := doJSON(t, h, "POST", "/admin/tokens", createBody(t, long, map[string]any{"count": 2})); code != http.StatusBadRequest {
		t.Errorf("count>1 且名称 50 字节应 400，实际 code=%d body=%s", code, body)
	}
}

// ③ 校验逐条。
func TestTokensCreateValidation(t *testing.T) {
	h := tokenTestHandler(t)
	autoCustom := func(groups []any) string {
		return tokenJSON(t, map[string]any{"name": "x", "automatic": true, "auto_group_mode": "custom", "auto_groups": groups})
	}
	tooMany := make([]any, 0, tokenAutoGroupMaxCount+1)
	for i := 0; i <= tokenAutoGroupMaxCount; i++ {
		tooMany = append(tooMany, fmt.Sprintf("g%d", i))
	}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"名称为空", createBody(t, "", nil), 400},
		{"只有空白", createBody(t, "   ", nil), 400},
		{"count 超上限", createBody(t, "x", map[string]any{"count": 1001}), 400},
		{"count 为负", createBody(t, "x", map[string]any{"count": -1}), 400},
		{"manual 无分组", createBody(t, "x", map[string]any{"groups": []string{}}), 400},
		{"auto custom 无分组", autoCustom(nil), 400},
		{"auto custom 超上限", autoCustom(tooMany), 400},
		{"IP 非法", createBody(t, "x", map[string]any{"allow_ips": strings.Join([]string{"10.0.0.1", "not-an-ip"}, "\n")}), 400},
		{"IP 合法含网段", createBody(t, "x", map[string]any{"allow_ips": strings.Join([]string{"10.0.0.1", "192.168.0.0/24"}, "\n")}), 200},
		{"count 缺省视为 1", createBody(t, "单条", nil), 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := doJSON(t, h, "POST", "/admin/tokens", c.body)
			if code != c.want {
				t.Fatalf("code=%d want=%d body=%s", code, c.want, body)
			}
		})
	}
}

// ④ 敏感端点：只此两条回明文、带 no-store、有限流。
func TestTokensRevealGuards(t *testing.T) {
	h := tokenTestHandler(t)
	code, body := doJSON(t, h, "POST", "/admin/tokens", createBody(t, "取明", nil))
	if code != 200 {
		t.Fatalf("创建失败 code=%d body=%s", code, body)
	}
	id := int(decodeMap(t, body)["items"].([]any)[0].(map[string]any)["id"].(float64))

	rec := tokenRecord(t, h, "POST", fmt.Sprintf("/admin/tokens/%d/key", id), "")
	if rec.Code != 200 {
		t.Fatalf("取明文失败 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("取明文响应缺 no-store，实际 %q", cc)
	}
	plain, _ := decodeMap(t, rec.Body.String())["key"].(string)
	if len(plain) != store.TokenKeyLength {
		t.Fatalf("明文长度应为 %d，实际 %d", store.TokenKeyLength, len(plain))
	}
	rec = tokenRecord(t, h, "POST", "/admin/tokens/batch/keys", tokenJSON(t, map[string]any{"ids": []int{id}}))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), plain) {
		t.Fatalf("批量取明文失败 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("批量取明文响应缺 no-store，实际 %q", cc)
	}
	// 批量上限 100。
	ids := make([]int, 0, tokenRevealBatchMax+1)
	for i := 0; i < tokenRevealBatchMax+1; i++ {
		ids = append(ids, i+1)
	}
	if rec := tokenRecord(t, h, "POST", "/admin/tokens/batch/keys", tokenJSON(t, map[string]any{"ids": ids})); rec.Code != 400 {
		t.Errorf("批量取明文 %d 条应 400，实际 %d", len(ids), rec.Code)
	}

	// 限流：换一个只放行 2 次的限速器，第 3 次必须 429 且带 Retry-After。
	old := tokenRevealLimiter
	tokenRevealLimiter = authlimit.NewLimiter(time.Minute, 2)
	defer func() { tokenRevealLimiter = old }()
	for i := 0; i < 2; i++ {
		if rec := tokenRecord(t, h, "POST", fmt.Sprintf("/admin/tokens/%d/key", id), ""); rec.Code != 200 {
			t.Fatalf("第 %d 次应放行，实际 %d", i+1, rec.Code)
		}
	}
	rec = tokenRecord(t, h, "POST", fmt.Sprintf("/admin/tokens/%d/key", id), "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 3 次应 429，实际 %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("429 响应缺 Retry-After")
	}
}

// ⑤ 更新（全字段 + status_only）、删除（单条 + 批量上限）、自查询量形状。
func TestTokensUpdateDeleteAndUsage(t *testing.T) {
	h := tokenTestHandler(t)
	code, body := doJSON(t, h, "POST", "/admin/tokens", createBody(t, "可改", map[string]any{"count": 2}))
	if code != 200 {
		t.Fatalf("创建失败 code=%d body=%s", code, body)
	}
	items := decodeMap(t, body)["items"].([]any)
	id := int(items[0].(map[string]any)["id"].(float64))
	second := int(items[1].(map[string]any)["id"].(float64))

	// status_only：只切禁用。
	code, body = doJSON(t, h, "POST", "/admin/tokens/update",
		tokenJSON(t, map[string]any{"id": id, "status_only": true, "status": store.TokenStatusDisabled}))
	if code != 200 {
		t.Fatalf("status_only 失败 code=%d body=%s", code, body)
	}
	if st := decodeMap(t, body)["status"].(float64); st != float64(store.TokenStatusDisabled) {
		t.Errorf("status 应为 %d，实际 %v", store.TokenStatusDisabled, st)
	}
	// 全字段更新：不限额度（强制 remain=0）+ 永不（-1）+ 允许查余额 + 模型限制逗号串 + 分组有序。
	code, body = doJSON(t, h, "POST", "/admin/tokens/update", tokenJSON(t, map[string]any{
		"id": id, "name": "改过", "automatic": false, "groups": []string{"b", "a"},
		"unlimited_quota": true, "remain_quota": 999, "allow_user_quota_query": true,
		"expired_time": -1, "model_limits_enabled": true, "model_limits": "m1, m2 ,m1",
	}))
	if code != 200 {
		t.Fatalf("更新失败 code=%d body=%s", code, body)
	}
	view := decodeMap(t, body)
	if view["remain_quota"].(float64) != 0 {
		t.Errorf("不限额度时 remain_quota 必须强制 0，实际 %v", view["remain_quota"])
	}
	groups := view["groups"].([]any)
	if len(groups) != 2 || groups[0].(string) != "b" {
		t.Errorf("分组顺序即优先级，应保持 [b a]，实际 %v", groups)
	}
	if got := view["model_limits"].(string); got != "m1,m2" {
		t.Errorf("model_limits 应去空白去重成 m1,m2，实际 %q", got)
	}
	// 自查询量：expires_at 把 -1 映射成 0；允许查余额时带 user_quota。
	code, body = doJSON(t, h, "GET", fmt.Sprintf("/admin/tokens/%d/usage", id), "")
	if code != 200 {
		t.Fatalf("usage 失败 code=%d body=%s", code, body)
	}
	usage := decodeMap(t, body)
	if usage["expires_at"].(float64) != 0 {
		t.Errorf("expires_at 应把 -1 映射为 0，实际 %v", usage["expires_at"])
	}
	if _, ok := usage["user_quota"]; !ok {
		t.Errorf("allow_user_quota_query=true 时应带 user_quota")
	}
	if _, ok := usage["total_granted"]; !ok {
		t.Errorf("usage 缺 total_granted")
	}
	// 关掉开关后不得再带 user_quota。
	if code, _ := doJSON(t, h, "POST", "/admin/tokens/update",
		tokenJSON(t, map[string]any{"id": id, "allow_user_quota_query": false, "groups": []string{"b"}})); code != 200 {
		t.Fatalf("二次更新失败 code=%d", code)
	}
	code, body = doJSON(t, h, "GET", fmt.Sprintf("/admin/tokens/%d/usage", id), "")
	if code != 200 {
		t.Fatalf("usage 失败 code=%d body=%s", code, body)
	}
	if _, ok := decodeMap(t, body)["user_quota"]; ok {
		t.Errorf("allow_user_quota_query=false 时不得带 user_quota")
	}

	// 删除：批量 + 上限 + 删后 404。
	code, body = doJSON(t, h, "POST", "/admin/tokens/delete", tokenJSON(t, map[string]any{"ids": []int{id, second}}))
	if code != 200 {
		t.Fatalf("批量删除失败 code=%d body=%s", code, body)
	}
	if deleted := decodeMap(t, body)["deleted"].(float64); deleted != 2 {
		t.Errorf("应删 2 条，实际 %v", deleted)
	}
	huge := make([]int, 0, tokenDeleteMaxCount+1)
	for i := 0; i < tokenDeleteMaxCount+1; i++ {
		huge = append(huge, i+1)
	}
	code, body = doJSON(t, h, "POST", "/admin/tokens/delete", tokenJSON(t, map[string]any{"ids": huge}))
	if code != 400 {
		t.Errorf("批量删除 %d 条应 400，实际 code=%d body=%s", len(huge), code, body)
	}
	if code, _ := doJSON(t, h, "GET", fmt.Sprintf("/admin/tokens/%d", id), ""); code != 404 {
		t.Errorf("已删除的令牌取详情应 404，实际 %d", code)
	}
}
