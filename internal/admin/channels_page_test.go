// channels_page_test.go —— 渠道列表的分页 / 筛选 / type_counts（T13）。
//
// # 为什么这些判据必须落在 Go 层
//
// 前端的"内存搜索+内存分页"依赖后端先把**过滤后的全集**给出来（一次最多 100 条）。
// 如果后端的 status 字典或上限写错，前端不会报错 —— 它只会安静地少显示几行，
// 而探针 verify_channels_save.js 只覆盖"新建一条能保存"这条路径，看不到这里。
//
// 因此这里逐条钉住 hyxapi 的列表行为（口径见 channels.go 的"分页 / 筛选"段）：
//
//	① p / page_size 真的分页，且 total 是**过滤后总数**（不是本页条数）
//	② page_size 上限 100（超了夹到 100，不是 400、也不是照给）
//	③ page_size 的兼容拼法 ps / size
//	④ 状态字典 enabled|1 / disabled|0 / 2 / 3 / 非法值
//	⑤ type_counts 只在 status+keyword 过滤之后、type 过滤之前分组
//	⑥ keyword 内存搜索（名称 / Base URL / 模型名）
//	⑦ 兼容键：channels 与 items 是同一个数组（旧前端与探针读前者）
package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// chListResp 是 /admin/channels 的响应形状（只声明本文件要断言的字段）。
type chListResp struct {
	Channels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channels"`
	Items    []json.RawMessage `json:"items"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	// type_counts / status_counts 的键是**字符串**（JSON 对象键只能是字符串）。
	TypeCounts   map[string]int `json:"type_counts"`
	StatusCounts map[string]int `json:"status_counts"`
}

// seedChannelsForPageTest 建 5 条渠道：3 启用（type 0/1/1）、1 手动停用(2)、1 自动停用(3)。
//
// ⚠ 列表顺序由 channelstore.List() 决定，所以下面的断言一律看"命中了谁 / 命中几条"，
// 不看"第几行"。
func seedChannelsForPageTest(t *testing.T, h *Handler) {
	t.Helper()
	rows := []struct {
		name   string
		typ    int
		status int
	}{
		{"alpha-up", 0, 1},
		{"beta-cn", 1, 1},
		{"gamma-cn", 1, 1},
		{"delta-off", 0, 2},
		{"epsilon-auto", 1, 3},
	}
	for _, r := range rows {
		payload := map[string]any{
			"name":     r.name,
			"type":     r.typ,
			"base_url": "https://" + r.name + ".example.com/v1",
			"status":   r.status,
		}
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("构造渠道请求体失败: %v", err)
		}
		if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels", string(b)); code != http.StatusOK {
			t.Fatalf("建渠道 %s 应 200，实际 %d: %s", r.name, code, resp)
		}
	}
}

// getChannels 发一次列表请求并解出响应。
func getChannels(t *testing.T, h *Handler, query string) chListResp {
	t.Helper()
	code, body := doJSON(t, h, http.MethodGet, "/admin/channels"+query, "")
	if code != http.StatusOK {
		t.Fatalf("列表 %s 应 200，实际 %d: %s", query, code, body)
	}
	var out chListResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("列表响应不是合法 JSON: %v (%s)", err, body)
	}
	return out
}

// TestChannelsListPagination ① ② ③ ⑦：分页、总数、上限 100、ps/size 兼容、双键一致。
func TestChannelsListPagination(t *testing.T) {
	h := chanHandler(t, chanStore(t))
	seedChannelsForPageTest(t, h)

	// ① 缺省一页装得下 5 条；total 是过滤后的总数。
	all := getChannels(t, h, "")
	if all.Total != 5 || len(all.Channels) != 5 {
		t.Fatalf("空筛选应回 5/5，实际 total=%d len=%d", all.Total, len(all.Channels))
	}
	if all.Page != 1 || all.PageSize != channelsPageDefault {
		t.Fatalf("缺省 page/page_size 应为 1/%d，实际 %d/%d", channelsPageDefault, all.Page, all.PageSize)
	}
	// ⑦ channels 与 items 必须同长（同一个数组的两份键名）。
	if len(all.Items) != len(all.Channels) {
		t.Fatalf("channels 与 items 必须同长，实际 %d vs %d", len(all.Channels), len(all.Items))
	}

	// ① 真分页：每页 2 条 ⇒ 3 页，最后一页 1 条。
	seen := map[string]bool{}
	for page := 1; page <= 3; page++ {
		p := getChannels(t, h, fmt.Sprintf("?p=%d&page_size=2", page))
		if p.Total != 5 {
			t.Fatalf("第 %d 页 total 应为 5（过滤后总数，不是本页条数），实际 %d", page, p.Total)
		}
		want := 2
		if page == 3 {
			want = 1
		}
		if len(p.Channels) != want {
			t.Fatalf("第 %d 页应有 %d 条，实际 %d", page, want, len(p.Channels))
		}
		for _, c := range p.Channels {
			if seen[c.ID] {
				t.Fatalf("渠道 %s 在两页里重复出现", c.ID)
			}
			seen[c.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("三页合计应覆盖 5 条，实际 %d", len(seen))
	}

	// 越界页给空页而不是报错（前端删完最后一条时很正常）。
	if oob := getChannels(t, h, "?p=99&page_size=2"); len(oob.Channels) != 0 || oob.Total != 5 {
		t.Fatalf("越界页应回空页 + total=5，实际 len=%d total=%d", len(oob.Channels), oob.Total)
	}

	// ② 上限 100：照给 1000 必须夹到 100（不是 400，也不是 1000）。
	if cap1 := getChannels(t, h, "?page_size=1000"); cap1.PageSize != 100 {
		t.Fatalf("page_size 上限应为 100，实际 %d", cap1.PageSize)
	}
	if cap2 := getChannels(t, h, "?page_size=100"); cap2.PageSize != 100 {
		t.Fatalf("page_size=100 应原样保留，实际 %d", cap2.PageSize)
	}

	// ③ 兼容拼法：ps 与 size（hyxapi 的回落顺序 page_size → ps → size）。
	if ps := getChannels(t, h, "?ps=3"); ps.PageSize != 3 {
		t.Fatalf("ps=3 应被认作 page_size，实际 %d", ps.PageSize)
	}
	if sz := getChannels(t, h, "?size=4"); sz.PageSize != 4 {
		t.Fatalf("size=4 应被认作 page_size，实际 %d", sz.PageSize)
	}
	// 非法值回落缺省（不是在 GET 上回 400）。
	if bad := getChannels(t, h, "?p=abc&page_size=xyz"); bad.Page != 1 || bad.PageSize != channelsPageDefault {
		t.Fatalf("非法分页参数应回落 1/%d，实际 %d/%d", channelsPageDefault, bad.Page, bad.PageSize)
	}
}

// TestChannelsListStatusFilter ④ ⑤：状态字典与计数口径。
func TestChannelsListStatusFilter(t *testing.T) {
	h := chanHandler(t, chanStore(t))
	seedChannelsForPageTest(t, h)

	// 字典命中：enabled|1 → 3 条；disabled|0 → 2 条（手动+自动）；2 → 1；3 → 1；非法 → 全量。
	cases := []struct {
		q    string
		want int
	}{
		{"", 5}, // 不带 = 全量
		{"?status=enabled", 3},
		{"?status=1", 3},
		{"?status=disabled", 2},
		{"?status=0", 2},
		{"?status=2", 1},
		{"?status=3", 1},
		{"?status=bogus", 5}, // 不认识的值 = 不过滤（hyxapi 的 -1）
	}
	for _, c := range cases {
		got := getChannels(t, h, c.q)
		if got.Total != c.want {
			t.Errorf("筛选 %q 应命中 %d 条，实际 %d", c.q, c.want, got.Total)
		}
	}

	// ⑤ type_counts：status 过滤**之后**分组（停用的两条里 type=0/1 各一）。
	off := getChannels(t, h, "?status=disabled")
	if off.TypeCounts["0"] != 1 || off.TypeCounts["1"] != 1 {
		t.Fatalf("disabled 下 type_counts 应为 {0:1,1:1}，实际 %v", off.TypeCounts)
	}
	// status_counts：**不受** status 过滤影响（否则徽章在选中状态后全塌成 0）。
	if off.StatusCounts["1"] != 3 || off.StatusCounts["2"] != 1 || off.StatusCounts["3"] != 1 {
		t.Fatalf("status_counts 应当是全量口径 {1:3,2:1,3:1}，实际 %v", off.StatusCounts)
	}
	// type 过滤：type=1 命中 3 条，且 type_counts 不变（它在 type 过滤之前算）。
	byType := getChannels(t, h, "?type=1")
	if byType.Total != 3 {
		t.Fatalf("type=1 应命中 3 条，实际 %d", byType.Total)
	}
	if byType.TypeCounts["1"] != 3 {
		t.Fatalf("type_counts 不该受 type 过滤影响，实际 %v", byType.TypeCounts)
	}
}

// TestChannelsListKeywordSearch ⑥：内存搜索（名称 / Base URL / 模型名）。
func TestChannelsListKeywordSearch(t *testing.T) {
	h := chanHandler(t, chanStore(t))
	seedChannelsForPageTest(t, h)
	// 给 gamma-cn 补一个模型，验"按模型名搜"（更新走同一个 upsert，带 id）。
	add := map[string]any{
		"id":       "gamma-cn",
		"name":     "gamma-cn",
		"base_url": "https://gamma-cn.example.com/v1",
		"models":   []string{"glm-5.2-pro"},
	}
	b, _ := json.Marshal(add)
	// 先用列表拿到真实 id（newChannelID 生成的），再按 id 更新。
	ids := map[string]string{}
	for _, c := range getChannels(t, h, "").Channels {
		ids[c.Name] = c.ID
	}
	add["id"] = ids["gamma-cn"]
	b, _ = json.Marshal(add)
	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels", string(b)); code != http.StatusOK {
		t.Fatalf("补模型应 200，实际 %d: %s", code, resp)
	}

	cases := []struct {
		q    string
		want int
	}{
		{"?keyword=alpha", 1},       // 名称
		{"?keyword=ALPHA", 1},       // 大小写不敏感
		{"?keyword=example.com", 5}, // Base URL
		{"?keyword=glm-5.2", 1},     // 模型名
		{"?keyword=zzz-nope", 0},    // 无命中
		{"?q=beta", 1},              // 短拼法（本仓前端搜索框）
		{"?keyword=cn", 2},          // beta-cn / gamma-cn
	}
	for _, c := range cases {
		if got := getChannels(t, h, c.q); got.Total != c.want {
			t.Errorf("搜索 %q 应命中 %d 条，实际 %d", c.q, c.want, got.Total)
		}
	}
}
