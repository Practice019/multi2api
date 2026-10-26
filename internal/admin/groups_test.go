// groups_test.go —— T85 分组引用反查的判据（只读聚合）。
//
// # 为什么这一组测试值得存在
//
// 用户最痛的问题是「我改这个分组会影响谁」。反查答错的两种形态都很坏：
//
//	少报（漏了某类引用）⇒ 用户以为删掉没事，结果一批 key 当场 403；
//	多报（把悬空/别的组算进来）⇒ 用户不敢动任何东西。
//
// 所以下面的判据逐条钉住三处来源，并且把两个**必须可区分**的结果分开测：
// 「分组不存在（404）」与「分组存在但没人引用（200 + total:0）」。
package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// groupsUsageFixture 一套带分组存储 + apikey 存储 + SQL（渠道/令牌）的管理端。
type groupsUsageFixture struct {
	h      *Handler
	groups *groupstore.Store
	keys   *apikey.Store
}

func newGroupsUsageFixture(t *testing.T) *groupsUsageFixture {
	t.Helper()
	// 渠道与令牌都读 SQL（store.DB 是包级单例，openSeedDB 负责开库+建表+收尾关闭）。
	openSeedDB(t)
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatalf("groupstore.New: %v", err)
	}
	ks, err := apikey.New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatalf("apikey.New: %v", err)
	}
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true, Groups: gs, APIKeys: ks})
	return &groupsUsageFixture{h: h, groups: gs, keys: ks}
}

// usage 调反查端点（id 既可以是分组名也可以是分组 ID）。
func (f *groupsUsageFixture) usage(t *testing.T, id string) (int, map[string]any) {
	t.Helper()
	code, body := doJSON(t, f.h, http.MethodGet, "/admin/groups/usage?id="+url.QueryEscape(id), "")
	var out map[string]any
	if strings.TrimSpace(body) != "" {
		_ = json.Unmarshal([]byte(body), &out)
	}
	return code, out
}

// listGroups 取分组列表（转成 name → item 的 map，便于按名字断言）。
func (f *groupsUsageFixture) listGroups(t *testing.T) map[string]map[string]any {
	t.Helper()
	code, body := doJSON(t, f.h, http.MethodGet, "/admin/groups", "")
	if code != http.StatusOK {
		t.Fatalf("GET /admin/groups 应 200，实际 %d: %s", code, body)
	}
	var out struct {
		Groups []map[string]any `json:"groups"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析分组列表失败: %v", err)
	}
	byName := map[string]map[string]any{}
	for _, g := range out.Groups {
		name, _ := g["name"].(string)
		byName[name] = g
	}
	return byName
}

// addKey 建一把 API key 并（可选）绑分组 **ID**（与前端/一致性报告同口径：apikey.Groups 存 ID）。
func (f *groupsUsageFixture) addKey(t *testing.T, name string, groupIDs []string) *apikey.Key {
	t.Helper()
	k, err := f.keys.Create(name, 0, 0, nil)
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	if len(groupIDs) > 0 {
		if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, groupIDs); err != nil {
			t.Fatalf("给 key 绑分组失败: %v", err)
		}
	}
	return k
}

// addChannel 建一条渠道（Group 列写分组**名**；空 = 走 default 兜底）。
func (f *groupsUsageFixture) addChannel(t *testing.T, name, group string) *store.Channel {
	t.Helper()
	base := "https://" + name + ".example.invalid/v1"
	ch := &store.Channel{
		Name: name, BaseURL: &base, Group: group,
		Status: store.ChannelStatusEnabled, ExternalID: "ch-" + name,
	}
	id, err := store.CreateChannel(ch)
	if err != nil {
		t.Fatalf("建渠道 %s 失败: %v", name, err)
	}
	ch.ID = id
	return ch
}

// addToken 建一条**账号令牌**（user_id > 0），分组按**名**编码进 Group 串（hyxapi 形态）。
//
// ⚠ userID 必须非 0：本仓的 API key 与令牌是 tokens 表的同一批行，反查按归属区分
// （见 groups.go 里 groupRefIndex 的注释：user_id == 0 ⇒ keys，> 0 ⇒ tokens）。
// 传 0 会让这条令牌落进 keys 一类，那正是"两处都算一遍"的老毛病。
func (f *groupsUsageFixture) addToken(t *testing.T, name string, groups []string) *store.Token {
	t.Helper()
	tok := &store.Token{Name: name, UserID: 42, Groups: groups}
	if err := store.CreateToken(tok); err != nil {
		t.Fatalf("建令牌 %s 失败: %v", name, err)
	}
	return tok
}

// refNames 从反查响应的某个数组里取 name 集合。
func refNames(t *testing.T, out map[string]any, field string) []string {
	t.Helper()
	items, ok := out[field].([]any)
	if !ok {
		t.Fatalf("响应里 %s 不是数组（可能是 null）: %v", field, out[field])
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		names = append(names, m["name"].(string))
	}
	return names
}

// TestGroupsUsageReverseLookup 三处来源各一真一假：只该命中真的那一条。
func TestGroupsUsageReverseLookup(t *testing.T) {
	f := newGroupsUsageFixture(t)
	vip, err := f.groups.Create("vip", "", []string{"m-a"})
	if err != nil {
		t.Fatalf("建分组: %v", err)
	}
	other, err := f.groups.Create("other", "", []string{"m-b"})
	if err != nil {
		t.Fatalf("建分组: %v", err)
	}

	hitKey := f.addKey(t, "key-vip", []string{vip.ID})
	f.addKey(t, "key-other", []string{other.ID})
	f.addKey(t, "key-unrestricted", nil) // 不限制的 key：不引用任何分组
	f.addChannel(t, "chan-vip", "vip")
	f.addChannel(t, "chan-other", "other")
	f.addToken(t, "tok-vip", []string{"vip"})
	f.addToken(t, "tok-other", []string{"other"})

	code, out := f.usage(t, "vip")
	if code != http.StatusOK {
		t.Fatalf("反查 vip 应 200，实际 %d: %v", code, out)
	}
	if out["group"] != "vip" {
		t.Errorf("响应应回分组名 vip，实际 %v", out["group"])
	}
	if got := refNames(t, out, "keys"); len(got) != 1 || got[0] != "key-vip" {
		t.Errorf("keys 应只命中 key-vip，实际 %v", got)
	}
	if got := refNames(t, out, "channels"); len(got) != 1 || got[0] != "chan-vip" {
		t.Errorf("channels 应只命中 chan-vip，实际 %v", got)
	}
	if got := refNames(t, out, "tokens"); len(got) != 1 || got[0] != "tok-vip" {
		t.Errorf("tokens 应只命中 tok-vip，实际 %v", got)
	}
	if total, _ := out["total"].(float64); int(total) != 3 {
		t.Errorf("total 应为 3（三类各一），实际 %v", out["total"])
	}
	// ★ 两类的**互斥**：同一批 rows 不能既算 keys 又算 tokens（那是 total 翻倍的根因）。
	// 这里 key（user_id=0）进 keys、令牌（user_id=42）进 tokens，各一条，和为 2。
	if keyN, tokN := len(refNames(t, out, "keys")), len(refNames(t, out, "tokens")); keyN+tokN != 2 {
		t.Errorf("凭据两类应互斥且合计 2 条（keys=%d tokens=%d）—— 同一行被算两次就会变 4", keyN, tokN)
	}
	if tr, ok := out["token_scan_truncated"].(bool); !ok || tr {
		t.Errorf("令牌扫描未截断时应明确回 false，实际 %v", out["token_scan_truncated"])
	}

	// ★ 明文 key 不该出现在这份只读报告里（与一致性报告/令牌列表同一条纪律）。
	_, raw := doJSON(t, f.h, http.MethodGet, "/admin/groups/usage?id=vip", "")
	if strings.Contains(raw, hitKey.ID) {
		t.Errorf("★ 反查响应里出现了明文 API key（应只给掩码）: %s", raw)
	}
	if !strings.Contains(raw, "key-vip") {
		t.Errorf("响应里应带上 key 的名字（否则前端没法显示是谁）: %s", raw)
	}

	// 按 **ID** 查也要能查到（前端拿得到 ID，内部调用也常用 ID）。
	code, byID := f.usage(t, vip.ID)
	if code != http.StatusOK {
		t.Fatalf("按分组 ID 反查应 200，实际 %d: %v", code, byID)
	}
	if byID["total"] != out["total"] || byID["group"] != "vip" {
		t.Errorf("按 ID 与按名字的结果应一致，实际 %v vs %v", byID["total"], out["total"])
	}

	// ★ used_by_count 必须与 usage.total 一致（同一份聚合，两处口径不同就是 bug）。
	byName := f.listGroups(t)
	vipItem := byName["vip"]
	if vipItem == nil {
		t.Fatalf("分组列表里找不到 vip: %v", byName)
	}
	if n, _ := vipItem["used_by_count"].(float64); int(n) != 3 {
		t.Errorf("列表里的 used_by_count 应为 3，实际 %v", vipItem["used_by_count"])
	}
	if n, _ := vipItem["used_by_count"].(float64); int(n) != int(out["total"].(float64)) {
		t.Errorf("used_by_count(%v) 与 usage.total(%v) 必须一致", vipItem["used_by_count"], out["total"])
	}
	// other 组各 1 条（key/渠道/令牌），同样要对上
	if n, _ := byName["other"]["used_by_count"].(float64); int(n) != 3 {
		t.Errorf("other 组的 used_by_count 应为 3，实际 %v", byName["other"]["used_by_count"])
	}
}

// TestGroupsUsage404VsEmpty 两个必须可区分的结果：不存在（404） vs 存在但没人引用（total:0）。
func TestGroupsUsage404VsEmpty(t *testing.T) {
	f := newGroupsUsageFixture(t)
	if _, err := f.groups.Create("lonely", "", []string{"m-a"}); err != nil {
		t.Fatalf("建分组: %v", err)
	}

	// ① 不存在 ⇒ 404（**不是** 200 + 空数组：那会让用户永远以为"没人引用"）
	code, out := f.usage(t, "no-such-group")
	if code != http.StatusNotFound {
		t.Fatalf("不存在的分组应 404，实际 %d: %v", code, out)
	}
	if s, _ := out["error"].(string); !strings.Contains(s, "no-such-group") {
		t.Errorf("404 应点名分组，实际 %q", s)
	}

	// ② 存在但无人引用 ⇒ 200 + total:0 + 三个**空数组**（不是 null）
	code, out = f.usage(t, "lonely")
	if code != http.StatusOK {
		t.Fatalf("无人引用的分组应 200，实际 %d: %v", code, out)
	}
	if total, _ := out["total"].(float64); int(total) != 0 {
		t.Errorf("lonely 的 total 应为 0，实际 %v", out["total"])
	}
	for _, field := range []string{"keys", "channels", "tokens"} {
		arr, ok := out[field].([]any)
		if !ok || arr == nil {
			t.Errorf("%s 应是空数组而不是 null/缺失（前端不必为 null 多写分支），实际 %v", field, out[field])
		}
		if len(arr) != 0 {
			t.Errorf("%s 应为空，实际 %v", field, arr)
		}
	}
	// ③ 两者确实可区分：一个 404、一个 200
	if code == http.StatusNotFound {
		t.Fatal("lonely 不该是 404 —— 那样「不存在」与「没人引用」就分不开了")
	}
}

// TestGroupsUsageChannelDefaultFallbackAndDanglingRefs 覆盖两个容易答错的角落。
func TestGroupsUsageChannelDefaultFallbackAndDanglingRefs(t *testing.T) {
	f := newGroupsUsageFixture(t)
	def, err := f.groups.Create("default", "", []string{"m-a"})
	if err != nil {
		t.Fatalf("建 default 分组: %v", err)
	}
	// ① 渠道 Group 列为空 ⇒ 按 default 兜底（与能力行同一口径），必须算进 default 组
	f.addChannel(t, "chan-nogroup", "")
	code, out := f.usage(t, "default")
	if code != http.StatusOK {
		t.Fatalf("default 反查应 200，实际 %d", code)
	}
	if got := refNames(t, out, "channels"); len(got) != 1 || got[0] != "chan-nogroup" {
		t.Errorf("Group 列为空的渠道应算进 default 组，实际 %v", got)
	}

	// ② 悬空引用（指向已删除的分组 ID）不该算进任何分组
	vip, err := f.groups.Create("vip", "", []string{"m-b"})
	if err != nil {
		t.Fatalf("建 vip 分组: %v", err)
	}
	f.addKey(t, "key-dangling", []string{"g-deleted-xyz"})
	code, out = f.usage(t, "vip")
	if code != http.StatusOK {
		t.Fatalf("vip 反查应 200，实际 %d", code)
	}
	if total, _ := out["total"].(float64); int(total) != 0 {
		t.Errorf("悬空引用不该算进 vip（它指向别的 ID），实际 total=%v", out["total"])
	}
	// 而 default 组也不该被那条悬空引用算上
	code, out = f.usage(t, "default")
	if code != http.StatusOK {
		t.Fatalf("default 反查应 200，实际 %d", code)
	}
	if keys, _ := out["keys"].([]any); len(keys) != 0 {
		t.Errorf("悬空引用不该出现在任何分组的 keys 里，实际 %v", keys)
	}
	_ = def
	_ = vip
}

// TestGroupsUsageIsReadOnly 反查是**只读**的：点开看一眼不该改任何状态。
func TestGroupsUsageIsReadOnly(t *testing.T) {
	f := newGroupsUsageFixture(t)
	g, err := f.groups.Create("vip", "", []string{"m-a"})
	if err != nil {
		t.Fatalf("建分组: %v", err)
	}
	k := f.addKey(t, "key-vip", []string{g.ID})
	ch := f.addChannel(t, "chan-vip", "vip")
	tok := f.addToken(t, "tok-vip", []string{"vip"})

	before := map[string]string{
		"group_list":  dumpJSON(t, f.groups.List()),
		"key_groups":  dumpJSON(t, mustKeyGroups(t, f.keys, k.ID)),
		"token_group": mustTokenGroup(t, tok.ID),
		"chan_group":  mustChanGroup(t, ch.ID),
	}
	if _, out := f.usage(t, "vip"); out["total"] == nil {
		t.Fatalf("反查失败: %v", out)
	}
	if f.listGroups(t) == nil {
		t.Fatal("列表失败")
	}
	after := map[string]string{
		"group_list":  dumpJSON(t, f.groups.List()),
		"key_groups":  dumpJSON(t, mustKeyGroups(t, f.keys, k.ID)),
		"token_group": mustTokenGroup(t, tok.ID),
		"chan_group":  mustChanGroup(t, ch.ID),
	}
	for k2, v := range before {
		if after[k2] != v {
			t.Errorf("★ 反查不该改动 %s：反查前 %s，反查后 %s", k2, v, after[k2])
		}
	}
}

// TestGroupsUsageNotRegisteredWithoutStore 未注入分组存储 ⇒ 路由不存在（404），与 groups.go:15 约定一致。
func TestGroupsUsageNotRegisteredWithoutStore(t *testing.T) {
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true})
	code, _ := doJSON(t, h, http.MethodGet, "/admin/groups/usage?id=vip", "")
	if code != http.StatusNotFound {
		t.Errorf("未启用分组功能时 /admin/groups/usage 应 404，实际 %d", code)
	}
	// ⚠ 列表也是 404：groups.go:15 的约定是「未注入就**不注册路由**」（而不是注册一个
	// 恒回 enabled:false 的端点）。前端按 manifest 判断该不该渲染入口 ——「路由不存在」
	// 与「功能在但没启用」在前端是两种不同处理，这里守的是前者。
	code, body := doJSON(t, h, http.MethodGet, "/admin/groups", "")
	if code != http.StatusNotFound {
		t.Errorf("未注入分组存储时 /admin/groups 也应 404（不注册路由），实际 %d %s", code, body)
	}
}

// ── 只读判据用到的小工具 ──────────────────────────────────────────────────

func dumpJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(b)
}

func mustKeyGroups(t *testing.T, ks *apikey.Store, id string) []string {
	t.Helper()
	gs, ok := ks.GroupsOf(id)
	if !ok {
		t.Fatalf("key %s 不存在", id)
	}
	return gs
}

func mustTokenGroup(t *testing.T, id int) string {
	t.Helper()
	tok, err := store.GetTokenByID(id)
	if err != nil || tok == nil {
		t.Fatalf("读令牌失败: %v", err)
	}
	return tok.Group
}

func mustChanGroup(t *testing.T, id int64) string {
	t.Helper()
	ch, err := store.GetChannel(id)
	if err != nil || ch == nil {
		t.Fatalf("读渠道失败: %v", err)
	}
	return ch.Group
}
