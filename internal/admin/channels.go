// channels.go 管理端「渠道」CRUD（对标 new-api 的渠道设置）。
//
// # 三件事按仓库既有约定做
//
//  1. **未启用就不注册路由**（与 /admin/apikeys 同一条判据）：没注入 Channels 存储时
//     路由不存在 ⇒ 前端按 manifest 的 admin_routes 不渲染操作区，而不是渲染出一个点了会 501 的按钮。
//  2. **key 列表只回掩码**：列表接口回 `sk-abc…xyz` 形状（复用 apikey.MaskID，一个事实来源），
//     要复制明文走**显式**的 /admin/channels/reveal —— 那是一个"我知道我在做什么"的动作，
//     而不是"顺手把明文塞进列表"。列表接口是轮询的，明文塞进去等于每次轮询都把 key 发一遍。
//  3. **保存前校验映射环**：modelmap.Validate 在这里拦下 A→B→A。
//     环在请求时才炸的话，用户看到的是一个莫名其妙的上游 400，排查方向直接跑偏。
package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/modelmap"
	"workbuddy2api/internal/store"
)

// channelsStore 取渠道存储；nil = 未启用该功能。
func (h *Handler) channelsStore() *channelstore.Store { return h.cfg.Channels }

// registerChannels 注册 /admin/channels 系列端点（New 里调用）。
func (h *Handler) registerChannels() {
	h.register("GET /admin/channels", h.channelsList)
	h.register("POST /admin/channels/discover", h.channelsDiscover) // 「模型自动发现」：编辑时一键回填上游模型名
	h.register("POST /admin/channels", h.channelsUpsert)
	h.register("POST /admin/channels/delete", h.channelsDelete)
	h.register("POST /admin/channels/status", h.channelsStatus)
	h.register("POST /admin/channels/reveal", h.channelsReveal)
	// 多 key 运维（对标 new-api 的「Key 管理」）：加/删/启停都按**下标**动，不动别人。
	// ⚠ 不能只靠 POST /admin/channels 整体覆盖 —— 表单不回显明文，覆盖会把前端不知道的 key 抹掉。
	h.register("GET /admin/channels/keys", h.channelsKeysList)
	h.register("POST /admin/channels/keys/add", h.channelsKeysAdd)
	h.register("POST /admin/channels/keys/status", h.channelsKeysStatus)
	h.register("POST /admin/channels/keys/delete", h.channelsKeysDelete)

	// ── B3：对齐 hyxapi 渠道管理的其余行为（逐个端点对应 store 的现成仓储）──
	//
	// 路由全部注册在**本文件**（而不是 admin.go 的中央表）：渠道系列是一个内聚的
	// 功能面，加一条渠道端点不该去动装配层的表（B3 的任务约束也是这么定的）。
	// 未注入 Channels 存储时 registerChannels 整个不会被调用 ⇒ 未注册 = 前端不渲染。
	h.register("GET /admin/channels/search", h.channelsSearch)
	h.register("GET /admin/channels/tags", h.channelsTags)
	h.register("GET /admin/channels/tag/models", h.channelsTagModels)
	h.register("POST /admin/channels/tag/batch", h.channelsTagBatch)
	h.register("POST /admin/channels/tag/status", h.channelsTagStatus)
	h.register("POST /admin/channels/tag/edit", h.channelsTagEdit)
	h.register("POST /admin/channels/status-batch", h.channelsStatusBatch)
	h.register("POST /admin/channels/batch-delete", h.channelsBatchDelete)
	h.register("POST /admin/channels/clear-disabled", h.channelsClearDisabled)
	h.register("POST /admin/channels/copy", h.channelsCopy)
	h.register("POST /admin/channels/fix-abilities", h.channelsFixAbilities)
	h.register("GET /admin/channels/order", h.channelsOrder)
	h.register("POST /admin/channels/reorder", h.channelsReorder)
	h.register("POST /admin/channels/move", h.channelsMove)
	// 多 Key 的批量动作（查状态汇总 / 全启全禁 / 删所有已禁用 / 选择模式）。
	h.register("GET /admin/channels/keys/summary", h.channelsKeysSummary)
	h.register("POST /admin/channels/keys/status-all", h.channelsKeysStatusAll)
	h.register("POST /admin/channels/keys/delete-disabled", h.channelsKeysDeleteDisabled)
	h.register("POST /admin/channels/keys/mode", h.channelsKeysMode)
	// 上游侧：测试渠道 / 余额 / 模型变更（全部经 cfg.ChannelProvider，核心不认识具体上游）。
	h.register("POST /admin/channels/test", h.channelsTest)
	h.register("POST /admin/channels/balance", h.channelsBalance)
	h.register("POST /admin/channels/models/check", h.channelsModelsCheck)
	h.register("POST /admin/channels/models/apply", h.channelsModelsApply)
}

// ── 字段分类表（fail-closed）───────────────────────────────────────────────
//
// 口径来源：hyxapi channel_authz.go 的**三张分类表**做法（只借判断，实现自写）：
// 把请求体字段按「能改 / 敏感 / 服务端维护」分类，**未登记的字段一律拒绝**。
//
// # 为什么必须 fail-closed（而不是"忽略不认识的字段"）
//
// 忽略未知字段有一个很难查的坏形态：前端加了一个新字段（比如 `balance` /
// `external_id`），后端悄悄忽略 ⇒ 界面上填了、保存返回 200、值却没进去。
// 用户会以为是"上游没生效"，而不是"这个字段后端不认"。反过来拒绝它，
// 错误信息直接点名 —— 一次就查到。
//
// ⚠ 新增 channelReq 字段时**必须**同步登记在这里；忘了会由
// channels_parity_test.go 的 TestChannelRequestFieldsAreAllRegistered 判红
// （反射比对：靠测试，不靠自觉）。
const (
	// channelFieldEditable 普通可改字段。
	channelFieldEditable = "editable"
	// channelFieldSensitive 敏感字段：接受，但**不回显值**，只在响应里报"改了哪些"。
	// channelReq 里的 keys / base_url / model_mapping 属于这类：它们是凭据或上游地址，
	// 不该随每次保存出现在响应体与日志里。
	channelFieldSensitive = "sensitive"
	// channelFieldServerManaged 服务端维护字段：客户端显式写它 ⇒ 400。
	// 登记它们是为了让报错说的是"这不该你写"而不是"未登记"，两者处置完全不同。
	channelFieldServerManaged = "server-managed"
)

// channelFieldRegistry 见上面的分类表说明。
var channelFieldRegistry = map[string]string{
	"id":                              channelFieldEditable,
	"name":                            channelFieldEditable,
	"type":                            channelFieldEditable,
	"base_url":                        channelFieldSensitive,
	"keys":                            channelFieldSensitive,
	"models":                          channelFieldEditable,
	"model_mapping":                   channelFieldSensitive,
	"group":                           channelFieldEditable,
	"groups":                          channelFieldEditable,
	"priority":                        channelFieldEditable,
	"weight":                          channelFieldEditable,
	"status":                          channelFieldEditable,
	"key_selection_mode":              channelFieldEditable,
	"mode":                            channelFieldEditable,
	"multi_key_mode":                  channelFieldEditable,
	"batch_add_set_key_prefix_2_name": channelFieldEditable,
	"key_mode":                        channelFieldEditable,

	// ── T69：hyxapi 渠道页的其余可编辑字段（C2 的截图里它们都是表单字段）──────
	//
	// 分级判据（照 hyxapi channel_authz.go 的三档思路，落到本仓能执行的三档）：
	//
	//	editable        普通可写：写进去只影响这条渠道自己的展示与调度参数。
	//	sensitive       能**改写请求体 / 响应 / 上游路由**的字段。它们仍接受写入
	//	                （见文件里"敏感三字段的判据"那段），但：① 值永不回显、不入日志；
	//	                ② 写前必须能解析成 JSON 对象；③ 单列一条测试钉住分级。
	//	server-managed  服务端维护，客户端直写 ⇒ 400。
	//
	// 组织 / 测试模型 / 备注 / 自动禁用 / 标签 —— 普通可写。
	"openai_organization": channelFieldEditable,
	"test_model":          channelFieldEditable,
	"remark":              channelFieldEditable,
	"auto_ban":            channelFieldEditable,
	"tag":                 channelFieldEditable,
	// 参数覆盖 / 请求头覆盖 / 状态码复写 —— 敏感（它们能改出去与回来的东西）。
	"param_override":      channelFieldSensitive,
	"header_override":     channelFieldSensitive,
	"status_code_mapping": channelFieldSensitive,

	// 服务端维护：渠道行里有这些列，但只能由测试 / 余额 / 迁移路径写，不接受客户端直写。
	"balance":              channelFieldServerManaged,
	"used_quota":           channelFieldServerManaged,
	"test_time":            channelFieldServerManaged,
	"response_time":        channelFieldServerManaged,
	"created_time":         channelFieldServerManaged,
	"external_id":          channelFieldServerManaged,
	"balance_updated_time": channelFieldServerManaged,
}

// webuiChannelsSubmitFields 是**前端渠道表单会提交的键**的权威清单（T69 的防漂移锚点）。
//
// 来源（改这个切片前先读那两处代码）：
//
//	internal/server/webui-channels.html  saveDrawer() 里的 var body = { … }
//	  —— 无条件字段：type / name / base_url / models / groups / priority / weight / model_mapping
//	  —— 条件字段：keys + key_mode（有 key 时）、key_selection_mode + multi_key_mode（选了模式时）、
//	     mode + status（新建时）、id（编辑时）
//	  —— T69 新增的 8 个可编辑字段：openai_organization / test_model / remark / auto_ban /
//	     tag / param_override / header_override / status_code_mapping
//
// ⚠ 它与 channelFieldRegistry 的关系是**子集**：前端提交的键必须全部已登记，
//
//	否则用户一保存就吃 fail-closed 的 400（而界面上看不出为什么）。
//	这条由 channels_parity_test.go 的 TestWebuiChannelFormFieldsAreRegistered 双向钉住 ——
//	那条测试**直接解析 webui-channels.html 的 saveDrawer**（不只是比对这个切片），
//	所以"前端加了字段、忘了登记"会在 CI 就红，而不是等用户来报"保存失败"。
var webuiChannelsSubmitFields = []string{
	"id", "name", "type", "base_url", "models", "model_mapping", "groups",
	"priority", "weight", "status",
	"keys", "key_mode", "key_selection_mode", "multi_key_mode", "mode",
	// T69：表单里新增的 8 个字段
	"openai_organization", "test_model", "remark", "auto_ban", "tag",
	"param_override", "header_override", "status_code_mapping",
}

// channelView 是对外的渠道视图：**key 只出掩码**（明文走 reveal）。
type channelView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         int               `json:"type"`
	BaseURL      string            `json:"base_url"`
	KeyMasks     []string          `json:"key_masks"`
	KeyCount     int               `json:"key_count"`
	Models       []string          `json:"models"`
	ModelMapping map[string]string `json:"model_mapping"`
	Group        string            `json:"group"`
	Groups       []string          `json:"groups"`
	Priority     int               `json:"priority"`
	Weight       int               `json:"weight"`
	Status       int               `json:"status"`
	Enabled      bool              `json:"enabled"`

	// ── B3：hyxapi 渠道管理页需要的其余列 ────────────────────────────────
	// 这些字段由**列表路径**填充（viewOfChannelRow）；旧的 viewOf（写路径的响应）
	// 留零值 —— 保存的响应里回这几个统计列没有意义，且它们随时会被测/余额改写。
	Tag     string  `json:"tag,omitempty"`
	Balance float64 `json:"balance"`
	// T69 的四个普通可写字段要能**读回**（保存后表单要回填；列表也要显示）。
	// ⚠ 敏感三字段（param_override / header_override / status_code_mapping）**刻意不在这里** ——
	//   它们的值按分级判据永不回显；要看它们请直接查库（或将来另开显式 reveal 入口）。
	OpenAIOrganization string `json:"openai_organization,omitempty"`
	TestModel          string `json:"test_model,omitempty"`
	Remark             string `json:"remark,omitempty"`
	AutoBan            int    `json:"auto_ban"`
	UsedQuota          int64  `json:"used_quota"`
	TestTime           int64  `json:"test_time"`
	ResponseTime       int    `json:"response_time"`
	BalanceUpdatedTime int64  `json:"balance_updated_time"`
}

func viewOf(ch *channelstore.Channel, models []string) channelView {
	v := channelView{
		ID: ch.ID, Name: ch.Name, Type: ch.Type, BaseURL: ch.BaseURL,
		Models: models, ModelMapping: ch.ModelMap, Group: ch.Group,
		// Groups 用 EffectiveGroups()：一条只写了 Group 的存量渠道也要回 ["default"]，
		// 否则前端的分组多选器回填成空，用户一保存就把渠道从所有组里摘掉了。
		Groups:   ch.EffectiveGroups(),
		Priority: ch.Priority, Weight: ch.Weight, Status: ch.Status,
		Enabled:  ch.Status == channelstore.StatusEnabled,
		KeyMasks: make([]string, 0, len(ch.Keys)), KeyCount: len(ch.Keys),
	}
	for _, k := range ch.Keys {
		v.KeyMasks = append(v.KeyMasks, apikey.MaskID(k.Key))
	}
	return v
}

// ---- 分页 / 筛选（T13，对齐 hyxapi 的列表行为）----
//
// 口径来源（只借判断，实现自写）：
//
//	分页 common/page_info.go:37-77   p（缺省 1）、page_size（依次回落 ps → size）、
//	                                 **上限 100**、page_size 缺省 ItemsPerPage
//	状态字典 controller/channel.go:52-65（parseStatusFilter）
//	type_counts controller/channel.go:156-176（一条 GROUP BY 塞进同响应）
//	"停用"合并口径 controller/channel.go:301（statusFilter==0 ⇒ 只排掉 status==1）
//
// # 为什么"停用"是依次序判断而不是查表
//
// hyxapi 的字典是 switch：enabled|1 → 1；disabled|0 → **0**；"2" → 2；"3" → 3；
// 其余 → -1（不过滤）。其中 0 不是"某一条渠道的状态值"（渠道只有 1/2/3），
// 而是"所有未启用"的合并口径 —— 见 :295-307 那段：statusFilter==0 时
// ch.Status == ChannelStatusEnabled 就跳过。所以 0 单独处理，不能与 1/2/3 混在一张表里。

const (
	// channelsPageDefault 缺省页大小。与 hyxapi SearchChannels 的缺省 20 一致（:334）。
	channelsPageDefault = 20
	// channelsPageMax 页大小上限。hyxapi common/page_info.go:73-75 写死 100。
	// 它同时是"前端一次最多能加载多少条"的硬边界（内存搜索模式依赖它）。
	channelsPageMax = 100
)

// channelsPageQuery 是本端点的分页/筛选参数（零值不是合法状态，必须走 parseChannelsPageQuery）。
type channelsPageQuery struct {
	Page     int
	PageSize int
	// Status -1 = 不过滤；0 = "未启用"（status != 1，即 2/3 合并）；1/2/3 = 精确匹配。
	Status int
	// Type -1 = 不过滤；否则精确匹配渠道类型。
	Type int
	// Keyword 内存搜索词（大小写不敏感子串，见 channelMatchesKeyword）。
	Keyword string

	// ── B3（对齐 hyxapi 列表的其余筛选维度）──────────────────────────────
	// Group 分组精确匹配（channels.group 列，走 SQL）。
	Group string
	// TagMode 非空 = "按标签精确匹配"：把 Keyword 当作 tag 全等，而不是模糊搜索。
	// hyxapi 的 SearchChannels 里 tag_mode 就是这个意思（tag 是"一批渠道"的名字，
	// 模糊匹配它会把同前缀的别的标签也带进来）。
	TagMode string
	// SortBy / SortOrder 排序维度与方向（priority|id|name|balance|response_time|test_time）。
	// ⚠ 排序必须在**分页之前**做（否则排的是"这一页"而不是"整个集合"），
	// 而 store.ListChannelPage 只按 priority/id 排序 ⇒ 指定 sort_by 时走内存路径。
	SortBy    string
	SortOrder string
	// Model 按模型名过滤（models 列的 LIKE 子串），与 Keyword 是**并列**的两个条件。
	Model string
}

// parseChannelsPageQuery 解析 p / page_size（回落 ps、size）/ status / type / keyword。
//
// 非法值一律**回落缺省**而不是 400：分页参数是 GET 上的可选参数，把
// "手抖打了个 p=abc" 变成 400 只会让页面白屏；回落是 hyxapi 的做法（strconv 失败即忽略），
// 也与本仓 GET 端点的既有口味一致。
func parseChannelsPageQuery(r *http.Request) channelsPageQuery {
	q := channelsPageQuery{Page: 1, PageSize: channelsPageDefault, Status: -1, Type: -1}
	qs := r.URL.Query()
	if p, err := strconv.Atoi(qs.Get("p")); err == nil && p > 0 {
		q.Page = p
	}
	// 回落顺序照抄 hyxapi：page_size → ps → size（size 是 token 页的历史拼法）。
	for _, key := range []string{"page_size", "ps", "size"} {
		if v, err := strconv.Atoi(qs.Get(key)); err == nil && v > 0 {
			q.PageSize = v
			break
		}
	}
	if q.PageSize > channelsPageMax {
		q.PageSize = channelsPageMax
	}
	q.Status = parseChannelStatusFilter(qs.Get("status"))
	if v, err := strconv.Atoi(qs.Get("type")); err == nil {
		q.Type = v
	}
	// keyword 是 hyxapi 的拼法；q 是本仓前端搜索框的短拼法（两者取先给的那个）。
	q.Keyword = strings.TrimSpace(qs.Get("keyword"))
	if q.Keyword == "" {
		q.Keyword = strings.TrimSpace(qs.Get("q"))
	}
	q.Group = strings.TrimSpace(qs.Get("group"))
	q.TagMode = strings.TrimSpace(qs.Get("tag_mode"))
	q.Model = strings.TrimSpace(qs.Get("model"))
	q.SortBy = normalizeChannelSortBy(qs.Get("sort_by"))
	q.SortOrder = normalizeChannelSortOrder(qs.Get("sort_order"), q.SortBy)
	return q
}

// channelSortFields 是允许的排序维度（白名单）。
//
// 为什么是白名单而不是把参数直接拼进 SQL：那是注入面，而且排序键写错时的
// 表现是"静默按别的顺序排" —— 用户只会觉得"排序坏了"。不认识的键回落缺省。
var channelSortFields = map[string]string{
	"priority":      "priority",
	"id":            "id",
	"name":          "name",
	"balance":       "balance",
	"response_time": "response_time",
	"test_time":     "test_time",
}

// normalizeChannelSortBy 归一化排序维度；不认识 ⇒ ""（回落 store 的默认序）。
func normalizeChannelSortBy(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if _, ok := channelSortFields[v]; ok {
		return v
	}
	return ""
}

// normalizeChannelSortOrder 归一化方向。缺省：name 升序（人名/渠道名按字典序才自然），
// 其余降序（priority 大者先、余额大者先、越快越好 —— hyxapi 的默认也是降序）。
func normalizeChannelSortOrder(v, sortBy string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "asc", "ascend", "1":
		return "asc"
	case "desc", "descend", "-1":
		return "desc"
	}
	if sortBy == "name" {
		return "asc"
	}
	return "desc"
}

// channelTagModeOn 报告 tag_mode 是否为"开"。hyxapi 用 bool，HTTP 上是 1/true/on。
func channelTagModeOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes", "only":
		return true
	}
	return false
}

// parseChannelStatusFilter 复刻 hyxapi 的状态筛选字典（controller/channel.go:52-65）。
func parseChannelStatusFilter(v string) int {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "enabled", "1":
		return channelstore.StatusEnabled
	case "disabled", "0":
		// 0 = "未启用"的合并口径（2 手动停用 + 3 自动停用），见文件头。
		return 0
	case "2":
		return channelstore.StatusManuallyDisabled
	case "3":
		return channelstore.StatusAutoDisabled
	default:
		return -1
	}
}

// channelMatchesStatus want = -1 恒真；0 表示"未启用"（!= 启用）；其余精确匹配。
func channelMatchesStatus(status, want int) bool {
	if want < 0 {
		return true
	}
	if want == 0 {
		return status != channelstore.StatusEnabled
	}
	return status == want
}

// channelMatchesKeyword 内存搜索：名称 / Base URL / id / 声明的模型名，大小写不敏感子串
// （与 hyxapi 的 search 端点同一类判据：它按 name/key/models 搜）。
func channelMatchesKeyword(ch *channelstore.Channel, keyword string) bool {
	if keyword == "" {
		return true
	}
	k := strings.ToLower(keyword)
	if strings.Contains(strings.ToLower(ch.Name), k) ||
		strings.Contains(strings.ToLower(ch.BaseURL), k) ||
		strings.Contains(strings.ToLower(ch.ID), k) {
		return true
	}
	for _, m := range ch.Models {
		if strings.Contains(strings.ToLower(m), k) {
			return true
		}
	}
	return false
}

// channelsPageRange 把 (page, pageSize, total) 夹成 [start,end)。
//
// 越界页给空区间而不是报错（hyxapi 的 search 路径也是 min(...) 夹一下）：
// 删完最后一页的最后一条之后，前端还停在旧页码是很常见的，那不是错误。
func channelsPageRange(page, pageSize, total int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = channelsPageDefault
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return start, end
}

// channelsList GET /admin/channels
//
// 响应同时给两份键名，是刻意的过渡形态：
//
//	channels —— 本仓既有键（前端与 verify_channels_save.js 都在读它）
//	items    —— hyxapi 的键（分页响应的标准形状）
//
// 两者是**同一个数组**（本页条目），不是两份数据 —— 所以不存在"哪个是真相"的问题。
// total / page / page_size / type_counts 是新增的；旧调用方忽略它们即可。
func (h *Handler) channelsList(w http.ResponseWriter, r *http.Request) {
	q := parseChannelsPageQuery(r)
	s := h.channelsStore()
	if s == nil {
		// 与 apikeys 同款：未启用就回空集 + enabled=false（前端据此不渲染操作区）。
		// 分页元数据照给：前端不必为"未启用"写一条分支去猜页大小。
		writeJSON(w, http.StatusOK, map[string]any{
			"channels": []channelView{}, "items": []channelView{},
			"total": 0, "page": q.Page, "page_size": q.PageSize,
			"type_counts": map[string]int{}, "status_counts": map[string]int{},
			"enabled": false,
		})
		return
	}

	// 无库 ⇒ 明确 503：store 未接线与"没有渠道"是两件事，不能混成一个空列表。
	if store.DB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "数据库未打开（store.Open 未接线）"})
		return
	}

	// status_counts 是本仓扩展（给筛选器徽章用）。口径**刻意不同**：
	// 只受 keyword / group / tag 影响、**不受** status / type 过滤影响 —— 否则选中
	// "启用"之后"手动停用 3 / 自动停用 1"会全塌成 0，用户没法从徽章上看出还有多少停用的。
	// 渠道数在 13~25 量级，读一次全表比三条 COUNT 更省（也是 B1 的既有取舍）。
	all, err := store.ListChannels()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读渠道失败: " + err.Error()})
		return
	}
	statusCounts := channelStatusCounts(all, q)

	typeCounts := map[string]int{}
	var out []channelView
	var total int

	if q.Keyword == "" && q.TagMode == "" && q.SortBy == "" {
		// ① 纯 SQL 路径：分页 / status・type・group・model 过滤 / type_counts 全部下推。
		page, perr := store.ListChannelPage(store.ChannelQuery{
			Group: q.Group, Model: q.Model, Status: q.Status, Type: q.Type,
			Page: q.Page, PageSize: q.PageSize,
		})
		if perr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读渠道失败: " + perr.Error()})
			return
		}
		for t, n := range page.TypeCounts {
			typeCounts[strconv.Itoa(t)] = int(n)
		}
		out = h.viewsFromChannelRows(page.Items)
		total = int(page.Total)
	} else {
		// ② 内存路径：keyword / tag_mode / sort_by 需要**过滤后的全集**。
		//
		// ⚠ Type 传 ChannelTypeAny：type 过滤必须在 type_counts **之后**做
		//（type_counts 的用途正是"按当前其它条件看类型分布"）。
		rows, rerr := store.SearchChannelsByKeyword(store.ChannelQuery{
			Group: q.Group, Model: q.Model, Status: q.Status, Type: store.ChannelTypeAny,
		})
		if rerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "搜索渠道失败: " + rerr.Error()})
			return
		}
		// tag_mode 开时 keyword 是**标签**而不是搜索词 ⇒ 跳过文本匹配。否则"按标签筛"
		// 会被文本条件再筛一次而得到空集，用户看到的是"这个标签下没有渠道"。
		textKeyword := !channelTagModeOn(q.TagMode)
		kept := make([]store.Channel, 0, len(rows))
		for i := range rows {
			row := &rows[i]
			if textKeyword && !channelRowMatchesListKeyword(row, q.Keyword) {
				continue
			}
			if !channelRowMatchesTag(row, q) {
				continue
			}
			kept = append(kept, *row)
		}
		for i := range kept {
			typeCounts[strconv.Itoa(kept[i].Type)]++
		}
		if q.Type >= 0 {
			byType := make([]store.Channel, 0, len(kept))
			for i := range kept {
				if kept[i].Type == q.Type {
					byType = append(byType, kept[i])
				}
			}
			kept = byType
		}
		sortChannelRows(kept, q.SortBy, q.SortOrder)
		total = len(kept)
		start, end := channelsPageRange(q.Page, q.PageSize, total)
		out = h.viewsFromChannelRows(kept[start:end])
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"channels":      out,
		"items":         out,
		"total":         total,
		"page":          q.Page,
		"page_size":     q.PageSize,
		"type_counts":   typeCounts,
		"status_counts": statusCounts,
		"enabled":       true,
		// 能力索引的规模（"对外可服务的 (渠道,模型) 对数"）—— 一眼看出渠道是否真被展开成能力
		"ability_count": len(s.Abilities()),
	})
}

// channelStatusCounts 见 channelsList 里的口径说明（全量口径，不受 status/type 过滤影响）。
func channelStatusCounts(rows []store.Channel, q channelsPageQuery) map[string]int {
	out := map[string]int{}
	for i := range rows {
		row := &rows[i]
		// group 用**列相等**（与 store.applyChannelQuery 的 Where(&Channel{Group: g}) 同一口径）——
		// 多分组渠道的 group 列是逗号分隔，相等匹配只命中"完全一致"的那种写法；
		// 这里刻意与 SQL 路径保持一致，不另造一套语义。
		if q.Group != "" && row.Group != q.Group {
			continue
		}
		// 与列表同一条口径：tag_mode 开时 keyword 是标签，不参与文本匹配。
		if !channelTagModeOn(q.TagMode) && !channelRowMatchesListKeyword(row, q.Keyword) {
			continue
		}
		if !channelRowMatchesTag(row, q) {
			continue
		}
		out[strconv.Itoa(row.Status)]++
	}
	return out
}

// channelRowMatchesListKeyword 列表的 keyword 判据：id 精确 / name 模糊 / base_url 模糊 /
// models 模糊（一律大小写不敏感）。
//
// ⚠ 这里**刻意不匹配 key**（store.ChannelQuery.Keyword 也是这么定的）：列表是轮询接口，
// 让 keyword 命中 key 等于开一条"拿搜索框当 key 存在性探针"的侧信道。
// hyxapi 的 `key = ?` 那条语义只在 /admin/channels/search 上开（见 channelsSearch 的说明）。
func channelRowMatchesListKeyword(row *store.Channel, keyword string) bool {
	if keyword == "" {
		return true
	}
	if strconv.FormatInt(row.ID, 10) == keyword {
		return true
	}
	k := strings.ToLower(keyword)
	if strings.Contains(strings.ToLower(row.Name), k) {
		return true
	}
	if strings.Contains(strings.ToLower(derefString(row.BaseURL)), k) {
		return true
	}
	for _, m := range splitChannelCSV(row.Models) {
		if strings.Contains(strings.ToLower(m), k) {
			return true
		}
	}
	return false
}

// channelRowMatchesTag 报告一行是否命中 tag_mode 过滤。
//
// tag_mode 的语义（hyxapi）：把 keyword 当作**标签全等**，而不是模糊搜索 ——
// 标签是"一批渠道"的名字，模糊匹配会把同前缀的别的标签也带进来。
// keyword 为空时不启用（否则"开了 tag_mode 但没填词"会退化成"只显示没有标签的"）。
func channelRowMatchesTag(row *store.Channel, q channelsPageQuery) bool {
	if !channelTagModeOn(q.TagMode) || strings.TrimSpace(q.Keyword) == "" {
		return true
	}
	return derefString(row.Tag) == strings.TrimSpace(q.Keyword)
}

// sortChannelRows 在内存里按白名单维度排序（见 channelsList 的路径说明）。
//
// by 为空时用 priority DESC + id DESC —— 与 store.ListChannelPage 的默认序一致，
// 这样"带 sort_by"与"不带"两条路径的**默认**结果不会漂。
func sortChannelRows(rows []store.Channel, by, order string) {
	if by == "" {
		by = "priority"
	}
	desc := order != "asc"
	less := func(a, b store.Channel) bool {
		switch by {
		case "id":
			if a.ID != b.ID {
				return a.ID < b.ID
			}
		case "name":
			if a.Name != b.Name {
				return a.Name < b.Name
			}
		case "balance":
			if a.Balance != b.Balance {
				return a.Balance < b.Balance
			}
		case "response_time":
			if a.ResponseTime != b.ResponseTime {
				return a.ResponseTime < b.ResponseTime
			}
		case "test_time":
			if a.TestTime != b.TestTime {
				return a.TestTime < b.TestTime
			}
		default: // priority
			ap, bp := derefInt64(a.Priority), derefInt64(b.Priority)
			if ap != bp {
				return ap < bp
			}
		}
		// 同键时用 id 兜底：没有全序的比较函数在不同输入顺序下会给出不同结果，
		// 而那会让"同一页刷新两次顺序不一样"这类 bug 无法复现。
		return a.ID < b.ID
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if desc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})
}

// viewsFromChannelRows 把 SQL 行批量投影成对外视图。
func (h *Handler) viewsFromChannelRows(rows []store.Channel) []channelView {
	out := make([]channelView, 0, len(rows))
	for i := range rows {
		out = append(out, h.viewOfChannelRow(&rows[i]))
	}
	return out
}

// viewOfChannelRow 把一行 SQL 渠道投影成 channelView。
//
// ⚠ 行是 Omit("key") 读出来的 ⇒ 掩码要**另取明文再 MaskID**（与 channelstore.List 同一条
// 路径、同一个取舍：一次列表 = N+1 次查询；渠道数 13~25 量级可接受，上百条时要改成
// 一次 IN 批量取）。明文绝不进视图 —— 只有 reveal 与 keys?reveal=1 两个显式入口带明文。
func (h *Handler) viewOfChannelRow(row *store.Channel) channelView {
	v := channelView{
		ID: row.ExternalID, Name: row.Name, Type: row.Type, BaseURL: derefString(row.BaseURL),
		Models: splitChannelCSV(row.Models), ModelMapping: strMapFromJSONPtr(row.ModelMapping),
		Group: row.Group, Groups: row.GroupsOf(),
		Priority: int(derefInt64(row.Priority)), Weight: int(derefUint(row.Weight)),
		Status: row.Status, Enabled: row.Status == store.ChannelStatusEnabled,
		Tag: derefString(row.Tag), Balance: row.Balance, UsedQuota: row.UsedQuota,
		// T69 的四个可读字段（敏感三字段刻意不回显，见 channelView 的注释）。
		OpenAIOrganization: derefString(row.OpenAIOrganization),
		TestModel:          derefString(row.TestModel),
		Remark:             derefString(row.Remark),
		AutoBan:            derefInt(row.AutoBan),
		TestTime:           row.TestTime, ResponseTime: row.ResponseTime,
		BalanceUpdatedTime: row.BalanceUpdatedTime,
		KeyMasks:           []string{},
	}
	if keys, err := store.GetChannelKeys(row.ID); err == nil {
		for _, k := range keys {
			v.KeyMasks = append(v.KeyMasks, apikey.MaskID(k))
		}
	}
	v.KeyCount = len(v.KeyMasks)
	return v
}

// derefString / derefInt64 / derefUint 处理 store 里那些"可空语义用指针"的列。
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// derefInt 处理 *int 列（如 auto_ban）。
func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func derefUint(v *uint) uint {
	if v == nil {
		return 0
	}
	return *v
}

// splitChannelCSV 拆 channels.models / group 这类逗号分隔列（去空、去重、保序）。
//
// 与 channelstore 的 splitCSV 是同一条规则；本处只读不写，所以不做归一化副作用。
func splitChannelCSV(s string) []string {
	out := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

// strMapFromJSONPtr 解析 store 里的 JSON 映射列（nil / 坏 JSON ⇒ 空表，绝不返回 nil）。
//
// 返回空表而不是 nil：JSON 里 nil map 会序列化成 null，而前端读 `channel.model_mapping`
// 时 null 与 {} 的处置不同（null 常常直接抛）。
func strMapFromJSONPtr(raw *string) map[string]string {
	out := map[string]string{}
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return out
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(*raw), &m); err != nil {
		return out
	}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// channelReq 是保存渠道的入参。keys 用 []string 而不是结构化 []ChannelKey：
// 前端表单填的就是"一行一个 key"，每 key 状态由后端补成 Enabled（要停某个 key 走 status 接口，
// 那是运维动作、不是新建表单的事 —— 表单里塞每 key 状态只会让人填错）。
type channelReq struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         int               `json:"type"`
	BaseURL      string            `json:"base_url"`
	Keys         []string          `json:"keys"`
	Models       []string          `json:"models"`
	ModelMapping map[string]string `json:"model_mapping"`
	Group        string            `json:"group"`
	// Groups 多分组（P4）。空 = 沿用 Group（再空则 default）。
	// 与 Group 同时给时**以 Groups 为准**（Group 由存储层同步成 Groups[0]）。
	Groups   []string `json:"groups"`
	Priority int      `json:"priority"`
	Weight   int      `json:"weight"`
	Status   int      `json:"status"`
	// KeySelectionMode 多 key 的选法（""/"random"/"round_robin"）。空 = 保持原值（新建时 = 随机）。
	KeySelectionMode string `json:"key_selection_mode"`

	// ── B3：新增/更新的三种模式（对齐 hyxapi 的渠道批量新增与 Key 覆盖语义）────
	//
	// Mode 决定"这次新增要建几条渠道"：
	//
	//	""/"single"       一条渠道（keys 是它的多把 key）—— 既有行为
	//	"batch"           每把 key 各建一条渠道（hyxapi 的批量新增）
	//	"multi_to_single" 所有 key 合成**一条**多 Key 渠道（合一的那个方向）
	Mode string `json:"mode"`
	// MultiKeyMode 多 Key 选法（hyxapi 的拼法）。与 KeySelectionMode 同义；
	// 同时给时以 KeySelectionMode 为准（本仓的既有字段优先，避免两个来源打架）。
	MultiKeyMode string `json:"multi_key_mode"`
	// BatchAddSetKeyPrefix2Name 批量新增时把 key 的**掩码前缀**追加到渠道名后。
	//
	// ⚠ 与 hyxapi 的差异（刻意）：它拼的是 key 的**明文字面前缀**，这里拼 apikey.MaskID 的
	// 结果（形如 sk-abc…xyz）。理由：渠道名是长期可见文本（列表/日志/导出都有它），
	// 而本仓有一条硬口径「明文 key 不落列表与日志」；掩码前缀已经足够让运维分辨是哪一条。
	BatchAddSetKeyPrefix2Name bool `json:"batch_add_set_key_prefix_2_name"`
	// KeyMode 更新时对既有 key 的处置：
	//
	//	""/"append"  去重合并（既有行为：表单不回显明文 ⇒ 不传 keys 就表示不改）
	//	"replace"    用请求里的 keys **完全替换**（同字符串的 key 保留其原状态与原因）
	KeyMode string `json:"key_mode"`

	// ── T69：hyxapi 渠道页的其余可编辑字段 ─────────────────────────────────
	//
	// 这 8 个字段由 channelstore **不**管的列承载（channelstore.Channel 的冻结契约里没有它们），
	// 所以写入走 store.UpdateChannel（见 validateChannelExtras / applyChannelExtras）。
	// 两个写路径的分工是**互不重叠**的：channelstore 管 keys/models/groups/优先级/能力行，
	// 这里只写这 8 列 —— 不存在"两条路径抢同一个字段"。
	OpenAIOrganization string `json:"openai_organization"`
	TestModel          string `json:"test_model"`
	Remark             string `json:"remark"`
	// AutoBan 指针：nil = 这次没提这个字段（保持原值）；0/1 = 明确要关/要开。
	// ⚠ 不能用 0 兼任"没传"—— 那会让"取消勾选自动禁用"永远写不进去。
	AutoBan *int `json:"auto_ban"`
	// Tag 由**是否出现在请求体里**决定写不写（见 applyChannelExtras）：
	// 表单没带这个键 ⇒ 保持原标签；带了空串 ⇒ 明确清除。
	Tag string `json:"tag"`
	// 三个覆盖项允许两种形态：JSON 对象，或"装着 JSON 文本的字符串"（表单常用 textarea）。
	// 两种都会被归一化成对象文本落库（见 normalizeJSONObjectField）。
	ParamOverride     json.RawMessage `json:"param_override"`
	HeaderOverride    json.RawMessage `json:"header_override"`
	StatusCodeMapping json.RawMessage `json:"status_code_mapping"`
}

// channelsUpsert POST /admin/channels —— 新建（无 id）或更新（带 id）。
func (h *Handler) channelsUpsert(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	// ① 先把请求体原样读进来：分类要看"请求里**出现了**哪些键"，而结构体解完
	// 之后分不清"字段没传"与"传了零值" —— 未登记字段恰恰只能在这一层看见。
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读请求体失败: " + err.Error()})
		return
	}
	fields := decodeRequestFields(body)
	sensitive, rejected := classifyChannelRequestFields(fields)
	if len(rejected) > 0 {
		// fail-closed：未登记 / 服务端维护字段一律拒绝（理由见 channelFieldRegistry 的注释）。
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":           "拒绝的字段（未登记 fail-closed / 服务端维护）: " + strings.Join(rejected, ", "),
			"rejected_fields": rejected,
			"hint":            "未登记字段一律拒绝：静默忽略会造成『填了、保存 200、值却没进去』。新增字段请先在 channelFieldRegistry 登记。",
		})
		return
	}
	var req channelReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name 必填"})
		return
	}
	if req.BaseURL == "" && strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "base_url 必填（Key 型渠道只填 Base URL + Key）"})
		return
	}
	// 映射**保存前**校验：环在请求时才炸会让用户看到莫名其妙的上游 400
	if err := modelmap.Mapping(req.ModelMapping).Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "模型映射非法: " + err.Error()})
		return
	}
	// T69：附加字段**也在保存前**校验（坏 JSON 写进去会静默破坏这条渠道的每一次请求，
	// 而那要到用户发一次对话请求才会暴露）。校验完后 applyChannelExtras 才写库。
	extras, extErr := normalizeChannelExtras(&req, fields)
	if extErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": extErr.Error()})
		return
	}

	mode, modeErr := normalizeChannelCreateMode(req.Mode)
	if modeErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": modeErr.Error()})
		return
	}
	keyMode, kmErr := normalizeChannelKeyMode(req.KeyMode)
	if kmErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": kmErr.Error()})
		return
	}
	// key_selection_mode 与 multi_key_mode 同义（hyxapi 的拼法）；两个都给时以本仓的既有字段为准。
	selection := req.KeySelectionMode
	if selection == "" {
		selection = req.MultiKeyMode
	}
	if selection != "" && selection != "random" && selection != "round_robin" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "key_selection_mode / multi_key_mode 只能是 random / round_robin（空 = 保持原值）",
		})
		return
	}
	incoming := flattenChannelKeys(req.Keys)

	// ② batch：每把 key 各建一条渠道（hyxapi 的批量新增）。
	if mode == "batch" {
		h.channelsCreateBatch(w, s, &req, incoming, selection, sensitive, extras)
		return
	}

	ch := &channelstore.Channel{
		ID: req.ID, Name: req.Name, Type: req.Type, BaseURL: strings.TrimRight(req.BaseURL, "/"),
		Models: req.Models, ModelMap: req.ModelMapping, Group: req.Group, Groups: req.Groups,
		Priority: req.Priority, Weight: req.Weight, Status: req.Status,
	}
	if ch.Status == 0 {
		ch.Status = channelstore.StatusEnabled
	}
	created := false
	var old *channelstore.Channel
	if ch.ID == "" {
		ch.ID = newChannelID()
		created = true
	} else if got, ok := s.Get(ch.ID); ok {
		old = got
		// 更新：保留原 ChannelInfo（key 选择模式是既有设置，不能因为这次没传就被清掉）。
		ch.ChannelInfo = got.ChannelInfo
	}

	// ③ key 的处置（key_mode）：
	//
	//	append（默认）  在旧 key 上**去重合并**新 key —— 表单不回显明文 ⇒ 不回传 keys
	//	                就表示"不改 key"，回传则是"再加几把"。
	//	replace         用请求里的 keys **完全替换**（同一字符串的 key 保留它原来的
	//	                状态与停用原因：否则"保存一次"会把一把被上游判废的 key 重新启用，
	//	                而它下一次请求还会失败）。
	if keyMode == "replace" {
		ch.Keys = replaceChannelKeys(old, incoming)
	} else {
		ch.Keys = mergeChannelKeys(old, incoming)
	}
	if selection != "" {
		ch.ChannelInfo.KeySelectionMode = selection
	}
	ch.ChannelInfo.MultiKey = len(ch.Keys) > 1
	if err := s.Upsert(ch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// ④ T69：附加字段（组织/测试模型/备注/自动禁用/标签/三个覆盖项）。
	//
	// 它们落在 channelstore 契约**没有**的列上 ⇒ 走 store.UpdateChannel 的第二条写路径。
	// 两条路径字段互不重叠（channelstore 管 keys/models/groups/优先级/能力行），
	// 所以不存在"两条路径抢同一个字段"；顺序上先建行、再补列，失败时**渠道已经存在**，
	// 返回里如实说明（不回滚：回滚会让用户看到"保存了又没了"）。
	applied, aerr := h.applyChannelExtras(ch.ID, extras)
	resp := map[string]any{"ok": true, "created": created, "mode": orDefault(mode, "single")}
	if aerr != nil {
		resp["extras_error"] = aerr.Error()
	}
	if len(applied) > 0 {
		resp["applied_extras"] = applied
	}
	// 用**读回**的那一行构造响应（而不是内存里的 ch）：前端拿到的是落库后的真值，
	// 顺带把 T69 的四个可读字段带回（T69 的字段读回一致性就靠它）。
	if row, ok := h.channelRowByExternalID(ch.ID); ok && row != nil {
		resp["channel"] = h.viewOfChannelRow(row)
	} else {
		resp["channel"] = viewOf(ch, ch.Models)
	}
	out := resp
	// 敏感字段只报**名字**，不回显值：keys / base_url / model_mapping 不该随每次保存
	// 出现在响应体与日志里（值本身在库里，要看走 reveal）。
	if len(sensitive) > 0 {
		out["sensitive_fields"] = sensitive
	}
	writeJSON(w, http.StatusOK, out)
}

// normalizeChannelCreateMode 归一化 mode（"" ⇒ single）。
func normalizeChannelCreateMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "single":
		return "single", nil
	case "batch":
		return "batch", nil
	case "multi_to_single":
		return "multi_to_single", nil
	}
	return "", fmt.Errorf("mode 只能是 single / batch / multi_to_single")
}

// normalizeChannelKeyMode 归一化 key_mode（"" ⇒ append，保持既有语义）。
func normalizeChannelKeyMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "append":
		return "append", nil
	case "replace":
		return "replace", nil
	}
	return "", fmt.Errorf("key_mode 只能是 append / replace")
}

// flattenChannelKeys 把"一行一个 key"的输入摊平：既支持数组的每个元素是一把 key，
// 也支持一个元素里用换行塞了多把（用户从表格里整列复制过来就是这个形态）。
func flattenChannelKeys(in []string) []string {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		// 两种换行都要拆：Windows 上复制出来的是 \r\n，拆不掉的话整列会被当成**一把** key。
		norm := strings.ReplaceAll(raw, "\r\n", "\n")
		norm = strings.ReplaceAll(norm, "\r", "\n")
		for _, line := range strings.Split(norm, "\n") {
			if k := strings.TrimSpace(line); k != "" {
				out = append(out, k)
			}
		}
	}
	return out
}

// mergeChannelKeys append 语义：旧 key 原样保留，新 key 按内容去重后追加。
//
// 去重是必需的：同一把 key 填两遍会让轮询连续选中同一个条目
// （界面上看是"轮询没生效"，实际是数据里有两份相同的 key）。
func mergeChannelKeys(old *channelstore.Channel, incoming []string) []channelstore.ChannelKey {
	out := make([]channelstore.ChannelKey, 0, len(incoming)+4)
	seen := map[string]bool{}
	if old != nil {
		for _, k := range old.Keys {
			if k.Key == "" || seen[k.Key] {
				continue
			}
			seen[k.Key] = true
			out = append(out, k)
		}
	}
	for _, k := range incoming {
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, channelstore.ChannelKey{Key: k, Status: channelstore.StatusEnabled})
	}
	return out
}

// replaceChannelKeys replace 语义：以请求里的 keys 为准；同一字符串的旧 key
// **保留它原来的状态与原因**（见 channelsUpsert 里 ③ 的说明）。
func replaceChannelKeys(old *channelstore.Channel, incoming []string) []channelstore.ChannelKey {
	prev := map[string]channelstore.ChannelKey{}
	if old != nil {
		for _, k := range old.Keys {
			if k.Key != "" {
				prev[k.Key] = k
			}
		}
	}
	out := make([]channelstore.ChannelKey, 0, len(incoming))
	seen := map[string]bool{}
	for _, k := range incoming {
		if seen[k] {
			continue
		}
		seen[k] = true
		if got, ok := prev[k]; ok {
			out = append(out, got)
			continue
		}
		out = append(out, channelstore.ChannelKey{Key: k, Status: channelstore.StatusEnabled})
	}
	return out
}

// channelsCreateBatch 批量新增：每把 key 一条渠道（mode=batch）。
//
// 命名：默认在名称后加序号（-1/-2…）或（batch_add_set_key_prefix_2_name 时）加
// **key 的掩码前缀** —— 两条都是为了让列表里 N 条同名渠道可分辨。
// ⚠ 拼的是掩码（apikey.MaskID），不是明文前缀：渠道名是长期可见文本，
//
//	而本仓硬口径是"明文 key 不落列表/日志"（与 hyxapi 的刻意差异，已写在 channelReq 上）。
func (h *Handler) channelsCreateBatch(w http.ResponseWriter, s *channelstore.Store, req *channelReq,
	keys []string, selection string, sensitive []string, extras channelExtras) {
	if strings.TrimSpace(req.ID) != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode=batch 是**新增**语义，不接受 id"})
		return
	}
	if len(keys) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode=batch 需要 keys（一行一个 key）"})
		return
	}
	created := make([]channelView, 0, len(keys))
	for i, key := range keys {
		name := req.Name
		switch {
		case req.BatchAddSetKeyPrefix2Name:
			name = req.Name + " " + apikey.MaskID(key)
		case len(keys) > 1:
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}
		ch := &channelstore.Channel{
			ID: newChannelID(), Name: name, Type: req.Type, BaseURL: strings.TrimRight(req.BaseURL, "/"),
			Models: req.Models, ModelMap: req.ModelMapping, Group: req.Group, Groups: req.Groups,
			Priority: req.Priority, Weight: req.Weight, Status: req.Status,
			Keys: []channelstore.ChannelKey{{Key: key, Status: channelstore.StatusEnabled}},
		}
		if ch.Status == 0 {
			ch.Status = channelstore.StatusEnabled
		}
		if selection != "" {
			ch.ChannelInfo.KeySelectionMode = selection
		}
		if err := s.Upsert(ch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":   "已建 " + strconv.Itoa(len(created)) + " 条后失败: " + err.Error(),
				"created": len(created), "mode": "batch",
				"channels": created,
			})
			return
		}
		// 附加字段对**每一条**新建渠道都生效（validate 已在外层做过一次，这里只写库）。
		if _, aerr := h.applyChannelExtras(ch.ID, extras); aerr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":   "已建 " + strconv.Itoa(len(created)) + " 条，但第 " + strconv.Itoa(len(created)+1) + " 条的附加字段写入失败: " + aerr.Error(),
				"created": len(created), "mode": "batch", "channels": created,
			})
			return
		}
		if row, ok := h.channelRowByExternalID(ch.ID); ok && row != nil {
			created = append(created, h.viewOfChannelRow(row))
			continue
		}
		created = append(created, viewOf(ch, ch.Models))
	}
	out := map[string]any{
		"ok": true, "created": len(created), "mode": "batch", "channels": created,
	}
	if len(sensitive) > 0 {
		out["sensitive_fields"] = sensitive
	}
	writeJSON(w, http.StatusOK, out)
}

// decodeRequestFields 解析"字段名 → 原始 JSON"，只用于字段分类。
//
// 解析失败返回 nil：真正的错误由随后的结构体解码报出来（那份报错信息更准确）。
func decodeRequestFields(body []byte) map[string]json.RawMessage {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	return raw
}

// ── T69：附加字段（channelstore 契约之外的 8 列）───────────────────────────
//
// # 为什么必须有 presence（"这个键到底出现没有"）
//
// 这 8 个字段里三种语义都真实存在：
//
//	openai_organization / test_model / remark —— 空串是**合法值**（"没填"）
//	auto_ban（指针）                          —— 0 是合法值（"关闭自动禁用"）
//	tag                                      —— 空串 = **清除标签**，而"没提这个字段" = 保持原值
//
// 用零值兼作"没传"会让"取消勾选自动禁用""清空备注""清除标签"永远写不进去，
// 而那种故障在界面上表现为"保存成功但值没变" —— 最难查的一类。所以一律按
// **请求体里出现过哪些键**（fields）决定写不写。
type channelExtras struct {
	present map[string]bool

	openaiOrganization *string
	testModel          *string
	remark             *string
	autoBan            *int
	tag                *string

	paramOverride     *string
	headerOverride    *string
	statusCodeMapping *string
}

// has 报告请求体里是否出现过某个键。
func (e channelExtras) has(name string) bool { return e.present[name] }

// normalizeChannelExtras 校验并归一化附加字段（**保存前**调用；不写库）。
//
// 三个覆盖项允许两种形态：JSON 对象，或"装着 JSON 文本的字符串"（表单里常用 textarea）。
// 两种都归一化成**对象文本**落库 —— 存进去的永远是能被程序直接解析的那一份。
//
// 判据（敏感三字段为什么这样处理，见 normalizeJSONObjectField 的说明）：
// 值必须是 JSON **对象**；数组/标量/坏 JSON 一律 400。
func normalizeChannelExtras(req *channelReq, fields map[string]json.RawMessage) (channelExtras, error) {
	e := channelExtras{present: map[string]bool{}}
	for name := range fields {
		e.present[name] = true
	}
	if e.has("openai_organization") {
		v := req.OpenAIOrganization
		e.openaiOrganization = &v
	}
	if e.has("test_model") {
		v := req.TestModel
		e.testModel = &v
	}
	if e.has("remark") {
		v := req.Remark
		e.remark = &v
	}
	if e.has("auto_ban") {
		if req.AutoBan == nil {
			return e, fmt.Errorf("auto_ban 需要一个 0/1 数字（null 不是合法值）")
		}
		v := *req.AutoBan
		if v != 0 && v != 1 {
			return e, fmt.Errorf("auto_ban 只能是 0（关）或 1（开），实际 %d", v)
		}
		e.autoBan = &v
	}
	if e.has("tag") {
		v := strings.TrimSpace(req.Tag)
		e.tag = &v
	}
	for _, f := range []struct {
		name string
		raw  json.RawMessage
		dst  **string
	}{
		{"param_override", req.ParamOverride, &e.paramOverride},
		{"header_override", req.HeaderOverride, &e.headerOverride},
		{"status_code_mapping", req.StatusCodeMapping, &e.statusCodeMapping},
	} {
		if !e.has(f.name) {
			continue
		}
		norm, err := normalizeJSONObjectField(f.name, f.raw)
		if err != nil {
			return e, err
		}
		*f.dst = norm
	}
	return e, nil
}

// normalizeJSONObjectField 把一个字段归一化成"JSON 对象文本"。
//
// 接受两种输入：
//
//	{"a":1}          —— 直接就是对象
//	"{\"a\":1}"      —— 字符串里装着 JSON（表单 textarea 的常见形态）
//
// 其余（数组 / 数字 / 坏 JSON / null）⇒ 报错；空对象 `{}` 合法（= 明确清空该覆盖项）。
//
// # 为什么这三个字段要卡 JSON 合法性（这就是"敏感"在本仓的可执行判据）
//
// 它们会被逐字拼进**上游请求体**与**响应改写**：写进去一份坏 JSON 不会当场报错，
// 只会让这条渠道之后的每一次请求都变形 —— 而那看起来像"上游坏了"。
// 所以宁可在这里 400，也不要让它静默落库。
func normalizeJSONObjectField(name string, raw json.RawMessage) (*string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, fmt.Errorf("%s 需要一个 JSON 对象（想清空请传 {}）", name)
	}
	text := trimmed
	if trimmed[0] == '"' {
		// 字符串形态：先解出里面的文本，再当 JSON 解析（两层都要合法）。
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return nil, fmt.Errorf("%s 不是合法 JSON 字符串: %v", name, err)
		}
		text = strings.TrimSpace(inner)
		if text == "" {
			text = "{}"
		}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return nil, fmt.Errorf("%s 必须是 JSON 对象（例如 {\"k\":\"v\"}）: %v", name, err)
	}
	norm, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("%s 归一化失败: %v", name, err)
	}
	s := string(norm)
	return &s, nil
}

// applyChannelExtras 把附加字段写进 SQL 行（**Upsert 之后**调用）。
//
// 为什么走 store.UpdateChannel 而不是 channelstore.Upsert：这 8 列不在 channelstore 的
// 冻结契约里（它的 Channel 结构没有它们）。两条写路径字段互不重叠，所以不会互相覆盖：
//
//	channelstore.Upsert → name/type/base_url/keys/models/model_map/group(s)/priority/weight/status + 能力行
//	store.UpdateChannel → 本文件这 8 列
//
// ⚠ UpdateChannel 对空 Key / 空 ExternalID / 空 CreatedTime 的语义是"不传就不改"
//
//	（见它的注释），而我们读回来的行本来就是 Omit("key") ⇒ 明文 key 不会被这次回写抹掉。
func (h *Handler) applyChannelExtras(externalID string, e channelExtras) ([]string, error) {
	if len(e.present) == 0 {
		return nil, nil
	}
	row, ok := h.channelRowByExternalID(externalID)
	if !ok || row == nil {
		return nil, fmt.Errorf("渠道不存在: %s", externalID)
	}
	applied := make([]string, 0, 8)
	if e.has("openai_organization") && e.openaiOrganization != nil {
		row.OpenAIOrganization = strPtrOrNil(*e.openaiOrganization)
		applied = append(applied, "openai_organization")
	}
	if e.has("test_model") && e.testModel != nil {
		row.TestModel = strPtrOrNil(*e.testModel)
		applied = append(applied, "test_model")
	}
	if e.has("remark") && e.remark != nil {
		row.Remark = strPtrOrNil(*e.remark)
		applied = append(applied, "remark")
	}
	if e.has("auto_ban") && e.autoBan != nil {
		v := *e.autoBan
		row.AutoBan = &v
		applied = append(applied, "auto_ban")
	}
	if e.has("tag") && e.tag != nil {
		// 空串 ⇒ 显式清除标签（置 nil），而不是留一个空串标签
		//（空串会让"按标签筛选"多出一个无名字的标签）。
		row.Tag = strPtrOrNil(*e.tag)
		applied = append(applied, "tag")
	}
	if e.has("param_override") && e.paramOverride != nil {
		row.ParamOverride = e.paramOverride
		applied = append(applied, "param_override")
	}
	if e.has("header_override") && e.headerOverride != nil {
		row.HeaderOverride = e.headerOverride
		applied = append(applied, "header_override")
	}
	if e.has("status_code_mapping") && e.statusCodeMapping != nil {
		row.StatusCodeMapping = e.statusCodeMapping
		applied = append(applied, "status_code_mapping")
	}
	if len(applied) == 0 {
		return nil, nil
	}
	if err := store.UpdateChannel(row); err != nil {
		return nil, fmt.Errorf("附加字段写入失败: %w", err)
	}
	return applied, nil
}

// strPtrOrNil 空串 ⇒ nil（"没填"在库里就该是 NULL，而不是空串）。
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// orDefault 空串取缺省（用于响应里的 mode 回显）。
func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// channelsDelete POST /admin/channels/delete  {"id": "..."}
func (h *Handler) channelsDelete(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	// 先记下这条渠道**声明过哪些模型** —— 删除之后 ch 就查不到了，
	// 而这些名字正是要清理的引用。
	//
	// 为什么用「渠道声明的模型」而不是「目录快照」：渠道声明的名字是
	// 删除这一刻的确切事实；目录快照可能因为上游抖动而暂时为空，
	// 拿它做清理会误删用户精心挑的白名单（见 apikey.PruneModels 的注释）。
	var declared []string
	var channelName string
	if ch, ok := s.Get(req.ID); ok && ch != nil {
		declared = append(declared, ch.Models...)
		// 渠道的 model_mapping 值也是内部名，同样要清。
		for _, internal := range ch.ModelMap {
			declared = append(declared, internal)
		}
		channelName = ch.Name
	}

	if err := s.Delete(req.ID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}

	// ── 级联清理：删渠道 ⇒ 该渠道的模型在 API key 里也不该再出现 ──
	//
	// 用户的原话：「删除之后有关这个渠道的都不要了，语义不就是这样么」。
	// 对 —— 留着一个指向已不存在模型的别名，比没有更糟：用户看到它、
	// 复制它发请求，得到的却是 unknown_provider。
	out := map[string]any{"ok": true}
	if ks := h.apiKeysStore(); ks != nil && len(declared) > 0 {
		if res, perr := ks.PruneModels(declared); perr != nil {
			// 清理失败**不**回滚渠道删除：渠道已经删掉了，回滚会让用户
			// 看到"删了又回来"。如实报告哪一步没做干净即可。
			log.Printf("admin: 渠道 %s 已删除，但清理 API key 引用失败: %v", channelName, perr)
			out["prune_error"] = perr.Error()
		} else if res.KeysTouched > 0 {
			log.Printf("admin: 渠道 %s 已删除，连带清理 %d 把 API key 里的 %d 个模型引用: %v",
				channelName, res.KeysTouched, len(res.ModelsGone), res.ModelsGone)
			out["pruned_keys"] = res.KeysTouched
			out["pruned_models"] = res.ModelsGone
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// channelsStatus POST /admin/channels/status  {"id": "...", "status": 1|2|3}
func (h *Handler) channelsStatus(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID     string `json:"id"`
		Status int    `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	switch req.Status {
	case channelstore.StatusEnabled, channelstore.StatusManuallyDisabled, channelstore.StatusAutoDisabled:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status 只能是 1(启用)/2(手动停用)/3(自动停用)"})
		return
	}
	// 先读改前的状态：响应要能区分"真的改了"与"本来就是那个状态"
	//（与批量端点同一条口径 —— 用户点一次按钮，"没反应"与"无需反应"必须分得开）。
	before := 0
	if row, ok := h.channelRowByExternalID(req.ID); ok && row != nil {
		before = row.Status
	} else {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道不存在: " + req.ID})
		return
	}
	if err := s.SetStatus(req.ID, req.Status); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "changed": before != req.Status, "status": req.Status, "previous_status": before,
	})
}

// channelsReveal POST /admin/channels/reveal  {"id": "..."} —— 回**明文** key，供复制。
//
// 为什么单独一个端点：列表接口是轮询的（前端每次刷新都会打），把明文塞进列表等于
// 每次轮询都把全部渠道的 key 发一遍。而这个端点只在用户主动点「显示/复制」时调用。
// 它已经不要求额外口令 —— 会话门禁就是门槛（与"登录后凭据可看可复制"的既有口径一致）。
func (h *Handler) channelsReveal(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	ch, ok := s.Get(req.ID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道不存在: " + req.ID})
		return
	}
	keys := make([]string, 0, len(ch.Keys))
	for _, k := range ch.Keys {
		keys = append(keys, k.Key)
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": ch.ID, "keys": keys})
}

// newChannelID 造渠道 id。用 crypto/rand 而不是自增序号：序号在"删一条再建一条"时会重号，
// 而那些 id 会被能力索引与日志引用 ⇒ 重号会让历史日志指向另一条渠道。
func newChannelID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ch-fallback"
	}
	return "ch-" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// T08：channels + abilities 的反向 seed（PLAN-hyxapi-alignment §4 T08）
// ---------------------------------------------------------------------------

// RegistryChannelSeeds 是 cmd/server/main.go **13 处 registry.Register** 的反向清单。
//
// # 一个条目 = 一个注册点，不是一个上游
//
// ⚠ 这里**绝不能**按 provider ID / 上游实现去重。main.go:275 的 wb 与 :347 的
// wbIntl 是同一个上游（WorkBuddy）的两个渠道实例（:347 注释原文「海外版
// WorkBuddy AI（workbuddy-intl 渠道实例）」），两者只有 ID 与授权站点不同：
//
//	workbuddy       copilot.tencent.com（国内版）
//	workbuddy-intl  www.workbuddy.ai   （海外版）
//
// 若把清单写成 map[上游]=一条（例如按 internal/workbuddy 的 ProviderID 常量，
// 两个实例都来自那个包），13 个注册点会塌成 12 条甚至 1 条，而"上游在跑、
// channels 表里只有一半渠道"是那种**没有任何报错**的故障。
// 所以这里是**切片**（注册顺序），身份由 store 侧的 (Provider, BaseURL) 判定。
//
// # 为什么是 13 条（而不是 12 个上游包 + openaicompat）
//
// main.go 里的 registry.Register 调用点共 13 处：
//
//	:275 workbuddy  :347 workbuddy-intl  :433 codearts   :523 loomy
//	:609 trae       :664 cline           :711 raccoon    :781 lobsterai
//	:861 qoder      :890 qodercn         :979 zcode      :1012 minimax
//	:1058 monkeycode
//
// ⚠ internal/openaicompat 是**第 12 个上游包但不进 registry**：它作为「渠道工厂」
// 注入 main.go:1535-1537（admin.Config.ChannelProvider）与 :1666-1668
// （server.Config.ChannelProvider），在用户新建 Key 型渠道时按 (id, base_url)
// 现造一个 Provider。所以它**没有**注册点、也**不该**在渠道表里占一行 ——
// 它的行由渠道页的 CRUD 产生（走 channelstore）。本清单因此是 13 而不是 14。
//
// # BaseURL 的取值口径
//
// 账号型上游的推理基址常常是**按凭证/账号**推导的（workbuddy 的域来自凭证的
// channel 字段、zcode 按 platform 选 origin），这里填的是该注册点**实例身份**
// 的那个站点 —— 也就是"两个同上游实例靠什么区分"。取值来源已逐个核对：
//
//	copilot.tencent.com          config.go 的 admin.oauth_base_url 默认值（:1196/:1435）
//	www.workbuddy.ai             config.go 的 workbuddy_intl 默认值（:1568）
//	codearts.huaweicloud.com     codearts.DefaultPortalBase
//	loomyad.xunfei.cn/api/v1     loomy.DefaultBaseURL
//	trae-api-cn.mchost.guru      trae.AgentHost（对话/模型端点）
//	api.cline.bot                cline.DefaultAPIBase
//	xiaohuanxiong.com            raccoon.DefaultAPIBase
//	lobsterai-server.youdao.com  lobsterai.DefaultAPIBase
//	api2.qoder.sh                qoder 国际版 EncryptedInferBase 默认（config.go:858 注释）
//	gateway.qoder.com.cn         qoder 中国版 InferBase（qoder.go:147）
//	zcode.z.ai/api/v1            zcode OAuthBase 官方线上（zcode/client.go:46）
//	agent.minimax.cn             minimax apiHost
//	monkeycode-ai.com            monkeycode hostCN（client.go:35）
//
// # Models 为什么一律留空
//
// 隔一条：模型目录是**运行时事实**（账号型上游要活凭证打上游接口才拿得到），
// 而启动期没有、也不该发上游请求（W1 硬约束：不探测、不发请求）。
// 把一张过期的静态模型表写死进 seed，等于制造一个"界面说有、请求却 404"的
// 第二真相源 —— 那是渠道页历史上修过的一类事故。能力行改由两条路展开：
//
//	渠道自己声明了模型（ChannelSeed.Models，非默认清单的调用方）
//	目录就绪后 store.ExpandAbilitiesFromCatalog（T09 的 models/vendors）
func RegistryChannelSeeds() []store.ChannelSeed {
	return []store.ChannelSeed{
		{Provider: "workbuddy", BaseURL: "https://copilot.tencent.com", Source: "cmd/server/main.go:275"},
		{Provider: "workbuddy-intl", BaseURL: "https://www.workbuddy.ai", Source: "cmd/server/main.go:347"},
		{Provider: "codearts", BaseURL: "https://codearts.huaweicloud.com", Source: "cmd/server/main.go:433"},
		{Provider: "loomy", BaseURL: "https://loomyad.xunfei.cn/api/v1", Source: "cmd/server/main.go:523"},
		{Provider: "trae", BaseURL: "https://trae-api-cn.mchost.guru", Source: "cmd/server/main.go:609"},
		{Provider: "cline", BaseURL: "https://api.cline.bot", Source: "cmd/server/main.go:664"},
		{Provider: "raccoon", BaseURL: "https://xiaohuanxiong.com", Source: "cmd/server/main.go:711"},
		{Provider: "lobsterai", BaseURL: "https://lobsterai-server.youdao.com", Source: "cmd/server/main.go:781"},
		{Provider: "qoder", BaseURL: "https://api2.qoder.sh", Source: "cmd/server/main.go:861"},
		{Provider: "qodercn", BaseURL: "https://gateway.qoder.com.cn", Source: "cmd/server/main.go:890"},
		{Provider: "zcode", BaseURL: "https://zcode.z.ai/api/v1", Source: "cmd/server/main.go:979"},
		{Provider: "minimax", BaseURL: "https://agent.minimax.cn", Source: "cmd/server/main.go:1012"},
		{Provider: "monkeycode", BaseURL: "https://monkeycode-ai.com", Source: "cmd/server/main.go:1058"},
	}
}

// SeedSQLChannels 把 13 个注册点反向 seed 进 channels/abilities 两张表。
//
// # 调用时机与调用方
//
// 由 cmd/server 在 `store.Open` + `store.Migrate` 成功之后调用（T11 的注入点）。
// 可重复调用：第二次命中已有行（Created=0、Matched=13），不需要另写"是否首启"的判据。
//
// # 为什么 seed 的是"注册点"而不是"本次启用的上游"
//
// 清单来自代码事实（13 处调用）而不是配置事实（哪些 enabled=true）。于是：
//
//	配置关闭某上游 → 该行仍然存在（渠道是"有这条渠道、只是现在没流量"）
//	代码新增注册点 → 下一次启动多一行（并靠 ChannelSeedGaps 提醒清单是否漏登记）
//
// 反过来（只 seed 启用的上游）会让"表里有几行"随配置抖动，计数判据就失去意义。
//
// # 能力行
//
// 先展开清单里显式声明的模型（默认清单为空，见 RegistryChannelSeeds），
// 再按 T09 的两级目录（vendor = provider id）做一次「模型↔渠道」展开。
// 目录为空时这一步是 0，不是错误 —— 账号型上游的目录要活凭证才有。
func (h *Handler) SeedSQLChannels() (store.ChannelSeedStats, error) {
	var st store.ChannelSeedStats
	if store.DB == nil {
		return st, store.ErrNotOpen
	}
	st, err := store.SeedChannels(RegistryChannelSeeds())
	if err != nil {
		return st, err
	}
	n, aerr := store.ExpandAbilitiesFromCatalog(store.AbilityDefaultGroup)
	if aerr != nil {
		// 能力展开失败**不**让渠道 seed 失败：渠道行已经落库且可用，
		// 而目录/能力是下一层的事（T09）。如实记一行日志即可。
		log.Printf("admin: 渠道 seed 完成，但按目录展开能力行失败: %v", aerr)
	} else {
		st.Abilities += n
	}
	if gaps := h.ChannelSeedGaps(); len(gaps) > 0 {
		log.Printf("admin: 注册表里有渠道 seed 清单没有的 provider: %v —— "+
			"main.go 加了 registry.Register 但忘了登记 RegistryChannelSeeds？", gaps)
	}
	return st, nil
}

// ChannelSeedGaps 返回「注册表里有、渠道 seed 清单里没有」的 provider ID（升序）。
//
// 这是把清单与真实注册表**对起来**的那条守卫：清单是手写的，而
// registry.Register 是真发生的。两者一旦漂移，症状是"某个上游在跑，
// 渠道页/渠道表里却没有它" —— 没有任何报错。
// 它只报告、不阻断启动（注册表为 nil 的测试路径返回空）。
func (h *Handler) ChannelSeedGaps() []string {
	if h.cfg.Registry == nil {
		return nil
	}
	known := make(map[string]bool, 13)
	for _, s := range RegistryChannelSeeds() {
		known[s.Provider] = true
	}
	var gaps []string
	for _, id := range h.cfg.Registry.IDs() {
		if !known[id] {
			gaps = append(gaps, id)
		}
	}
	sort.Strings(gaps)
	return gaps
}

// ── B3：请求体字段分类（fail-closed）的执行部分 ─────────────────────────────
//
// 分类表（channelFieldRegistry）在上面；这里只做两件事：
//  1. 把请求体里出现的字段分到 sensitive / rejected；
//  2. 供守卫测试用反射把 channelReq 的字段逐个对表。
//
// ⚠ 未登记字段进 rejected（fail-closed），不是忽略 —— 理由见分类表的注释。

// channelRequestJSONFields 用反射列出 channelReq 的 JSON 字段名（升序）。
//
// 为什么用反射而不是手写清单：手写清单会**跟着代码一起忘**（加了字段忘了加清单，
// 守卫照样绿）。反射拿的是编译期事实，忘登记就必然被 TestChannelRequestFieldsAreAllRegistered 抓到。
func channelRequestJSONFields() []string {
	t := reflect.TypeOf(channelReq{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			name = f.Name
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// classifyChannelRequestFields 按分类表切分请求体里出现的字段名。
//
//	sensitive —— 已登记的敏感字段（接受，但响应里只报名字、不回显值）
//	rejected  —— 未登记（fail-closed）或服务端维护字段（客户端不该写）
//
// 两个返回值都排过序：报错信息稳定，测试可以逐字断言。
func classifyChannelRequestFields(raw map[string]json.RawMessage) (sensitive, rejected []string) {
	for name := range raw {
		class, ok := channelFieldRegistry[name]
		if !ok {
			rejected = append(rejected, name)
			continue
		}
		switch class {
		case channelFieldServerManaged:
			rejected = append(rejected, name)
		case channelFieldSensitive:
			sensitive = append(sensitive, name)
		}
	}
	sort.Strings(sensitive)
	sort.Strings(rejected)
	return sensitive, rejected
}

// ── 搜索（hyxapi /search 的语义）────────────────────────────────────────────

// channelsSearch GET /admin/channels/search?keyword=...
//
// # 五条匹配规则（hyxapi 的 where，逐条对齐）
//
//	id 精确 或 name 模糊 或 key **全等** 或 base_url 模糊 或 models 模糊。
//
// # 为什么单独一个端点，而不是复用列表的 keyword
//
// 列表是 30 秒轮询的接口，那里**刻意**不匹配 key（见 channelRowMatchesListKeyword 的注释）。
// 而 hyxapi 的搜索框要支持"粘一把 key 找渠道"这个运维动作 —— 所以它落在一个
// 显式调用的端点上：谁来调这一步就已经是管理员了（他本来就能 reveal），
// 而且匹配是**全等**、不是子串：拿不到"这把 key 是不是以 sk-abc 开头"的信息。
//
// ⚠ key 比对要读明文 ⇒ 逐行 GetChannelKeys（N+1 次查询）。渠道数 13~25 量级可接受；
// 上百条时要改成一次 IN 批量取（与 channelstore.List 记的是同一条取舍）。
func (h *Handler) channelsSearch(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	if store.DB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "数据库未打开（store.Open 未接线）"})
		return
	}
	q := parseChannelsPageQuery(r)
	rows, err := store.ListChannels()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读渠道失败: " + err.Error()})
		return
	}
	kw := strings.TrimSpace(q.Keyword)
	matched := make([]store.Channel, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		if !channelMatchesStatus(row.Status, q.Status) {
			continue
		}
		if q.Type >= 0 && row.Type != q.Type {
			continue
		}
		if q.Group != "" && row.Group != q.Group {
			continue
		}
		if kw != "" && !channelRowMatchesSearchKeyword(row, kw) {
			continue
		}
		matched = append(matched, *row)
	}
	sortChannelRows(matched, q.SortBy, q.SortOrder)
	total := len(matched)
	start, end := channelsPageRange(q.Page, q.PageSize, total)
	out := h.viewsFromChannelRows(matched[start:end])
	writeJSON(w, http.StatusOK, map[string]any{
		"channels":  out,
		"items":     out,
		"total":     total,
		"page":      q.Page,
		"page_size": q.PageSize,
		// ⚠ 刻意**不回显** keyword：本端点支持"key 全等"匹配 ⇒ keyword 可能是一把
		// **明文 key**，回显它等于把凭据写进响应体（以及任何访问日志）。调用方自己
		// 知道它搜了什么，不需要我们复述。
		"keyword_echoed": false,
		"enabled":        true,
		// 匹配口径写进响应：前端/排障一眼看出"为什么这条命中了"
		"match": "id 精确 / name 模糊 / key 全等 / base_url 模糊 / models 模糊",
	})
}

// channelRowMatchesSearchKeyword 搜索端点的五条规则（见 channelsSearch 的说明）。
func channelRowMatchesSearchKeyword(row *store.Channel, keyword string) bool {
	k := strings.ToLower(keyword)
	if strconv.FormatInt(row.ID, 10) == keyword {
		return true
	}
	if strings.Contains(strings.ToLower(row.Name), k) {
		return true
	}
	if strings.Contains(strings.ToLower(derefString(row.BaseURL)), k) {
		return true
	}
	for _, m := range splitChannelCSV(row.Models) {
		if strings.Contains(strings.ToLower(m), k) {
			return true
		}
	}
	// key **全等**（不是子串）：见端点注释里的侧信道说明。
	if keys, err := store.GetChannelKeys(row.ID); err == nil {
		for _, key := range keys {
			if key == keyword {
				return true
			}
		}
	}
	return false
}

// ── 标签三连 + 标签清单 / 标签下的模型 ─────────────────────────────────────

// channelsTags GET /admin/channels/tags
func (h *Handler) channelsTags(w http.ResponseWriter, r *http.Request) {
	if h.channelsStore() == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	tags, err := store.ListChannelTags()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读标签失败: " + err.Error()})
		return
	}
	if tags == nil {
		tags = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags, "count": len(tags)})
}

// channelsTagModels GET /admin/channels/tag/models?tag=xxx
//
// 某个标签下所有渠道**声明的模型**并集。用途：给"按标签分组"的模型选择器回填。
func (h *Handler) channelsTagModels(w http.ResponseWriter, r *http.Request) {
	if h.channelsStore() == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	if tag == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tag 必填"})
		return
	}
	rows, err := store.ListChannels()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读渠道失败: " + err.Error()})
		return
	}
	seen := map[string]bool{}
	models := []string{}
	channels := 0
	for i := range rows {
		if derefString(rows[i].Tag) != tag {
			continue
		}
		channels++
		for _, m := range splitChannelCSV(rows[i].Models) {
			if !seen[m] {
				seen[m] = true
				models = append(models, m)
			}
		}
	}
	sort.Strings(models)
	writeJSON(w, http.StatusOK, map[string]any{
		"tag": tag, "channels": channels, "models": models, "count": len(models),
	})
}

// channelsTagBatch POST /admin/channels/tag/batch  {"ids":["ch-x"],"tag":"prod"}
//
// 批量打标签（tag 传空串 = 清除标签）。
func (h *Handler) channelsTagBatch(w http.ResponseWriter, r *http.Request) {
	if h.channelsStore() == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		IDs []string `json:"ids"`
		Tag string   `json:"tag"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ids 必填"})
		return
	}
	ids, missing := channelIntIDs(req.IDs)
	if len(ids) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "没有一条渠道存在", "missing": missing})
		return
	}
	tag := strings.TrimSpace(req.Tag)
	var ptr *string
	if tag != "" {
		ptr = &tag
	}
	n, err := store.SetChannelsTag(ids, ptr)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "打标签失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": n, "tag": tag, "missing": missing})
}

// channelsTagStatus POST /admin/channels/tag/status  {"tag":"prod","status":2}
//
// 按标签批量启停（hyxapi 的 tag/status）。返回**真正变化**的条数 ——
// "点了没反应"与"点了但本来就是那个状态"必须能区分。
func (h *Handler) channelsTagStatus(w http.ResponseWriter, r *http.Request) {
	if h.channelsStore() == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		Tag    string `json:"tag"`
		Status int    `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	tag := strings.TrimSpace(req.Tag)
	if tag == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tag 必填"})
		return
	}
	if !store.IsValidChannelStatus(req.Status) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status 只能是 1(启用)/2(手动停用)/3(自动停用)"})
		return
	}
	rows, err := h.channelRowsByTag(tag)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	changed, err := store.SetChannelsStatus(ids, req.Status, channelStatusReason(req.Status))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "改状态失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tag": tag, "matched": len(ids), "changed": changed})
}

// channelsTagEdit POST /admin/channels/tag/edit
//
//	{"tag":"prod","priority":10,"weight":1,"models":["a"],"model_mapping":{"x":"y"},
//	 "groups":["default"],"group":"default"}
//
// 按标签批量改**可以批量共有的**字段：优先级 / 权重 / 模型 / 映射 / 分组。
// 刻意**不含** base_url / keys：那两者是每条渠道各自的凭据与地址，
// 批量把它们改成同一个值不是运维动作，是事故。
func (h *Handler) channelsTagEdit(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		Tag          string            `json:"tag"`
		Priority     *int              `json:"priority"`
		Weight       *int              `json:"weight"`
		Models       []string          `json:"models"`
		ModelMapping map[string]string `json:"model_mapping"`
		Groups       []string          `json:"groups"`
		Group        string            `json:"group"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	tag := strings.TrimSpace(req.Tag)
	if tag == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tag 必填"})
		return
	}
	if err := modelmap.Mapping(req.ModelMapping).Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "模型映射非法: " + err.Error()})
		return
	}
	rows, err := h.channelRowsByTag(tag)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	updated := 0
	for i := range rows {
		ch, ok := s.Get(rows[i].ExternalID)
		if !ok || ch == nil {
			continue
		}
		if req.Priority != nil {
			ch.Priority = *req.Priority
		}
		if req.Weight != nil {
			ch.Weight = *req.Weight
		}
		if req.Models != nil {
			ch.Models = req.Models
		}
		if req.ModelMapping != nil {
			ch.ModelMap = req.ModelMapping
		}
		if len(req.Groups) > 0 {
			ch.Groups = req.Groups
			ch.Group = req.Groups[0]
		} else if strings.TrimSpace(req.Group) != "" {
			ch.Group = strings.TrimSpace(req.Group)
		}
		if err := s.Upsert(ch); err != nil {
			writeJSON(w, http.StatusInternalServerError,
				map[string]any{"error": "已改 " + strconv.Itoa(updated) + " 条后失败: " + err.Error()})
			return
		}
		updated++
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tag": tag, "matched": len(rows), "updated": updated})
}

// ── 删除三连 ───────────────────────────────────────────────────────────────

// channelsBatchDelete POST /admin/channels/batch-delete
//
//	{"ids":["ch-a","ch-b"]}  或  {"tag":"temp"}
//
// 与单条删除**同一条级联**：先记下这些渠道声明过的模型，删完再清理 API key 里的引用
// （留着一个指向已删渠道模型的别名，比没有更糟 —— 见 channelsDelete 的长注释）。
func (h *Handler) channelsBatchDelete(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		IDs []string `json:"ids"`
		Tag string   `json:"tag"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	tag := strings.TrimSpace(req.Tag)
	var rows []store.Channel
	var missing []string
	var err error
	switch {
	case tag != "":
		rows, err = h.channelRowsByTag(tag)
	default:
		if len(req.IDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ids 或 tag 至少给一个"})
			return
		}
		rows, missing, err = h.channelRowsByExternalIDs(req.IDs)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	ids := make([]int64, 0, len(rows))
	var declared []string
	for i := range rows {
		ids = append(ids, rows[i].ID)
		declared = append(declared, splitChannelCSV(rows[i].Models)...)
	}
	n, err := store.DeleteChannels(ids)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "删除失败: " + err.Error()})
		return
	}
	out := map[string]any{"ok": true, "deleted": n, "missing": missing}
	h.pruneAPIKeyRefs(out, declared)
	writeJSON(w, http.StatusOK, out)
}

// channelsClearDisabled POST /admin/channels/clear-disabled
//
// 清空**已禁用**渠道（status ∈ {2,3}）。store.DeleteDisabledChannels 在同一个
// 事务里连能力行一起删（这是它与 hyxapi 的一处刻意差异，见那边的注释）。
func (h *Handler) channelsClearDisabled(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	rows, err := store.ListChannels()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读渠道失败: " + err.Error()})
		return
	}
	var declared []string
	for i := range rows {
		if rows[i].Status == store.ChannelStatusManuallyDisabled || rows[i].Status == store.ChannelStatusAutoDisabled {
			declared = append(declared, splitChannelCSV(rows[i].Models)...)
		}
	}
	n, err := store.DeleteDisabledChannels()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "清空失败: " + err.Error()})
		return
	}
	out := map[string]any{"ok": true, "deleted": n}
	h.pruneAPIKeyRefs(out, declared)
	writeJSON(w, http.StatusOK, out)
}

// pruneAPIKeyRefs 把"渠道声明过的模型"从 API key 的别名/白名单里清掉。
//
// 失败**不**回滚删除（渠道已经没了，回滚会让用户看到"删了又回来"），只如实报告。
func (h *Handler) pruneAPIKeyRefs(out map[string]any, declared []string) {
	if len(declared) == 0 {
		return
	}
	ks := h.apiKeysStore()
	if ks == nil {
		return
	}
	res, err := ks.PruneModels(declared)
	if err != nil {
		out["prune_error"] = err.Error()
		return
	}
	if res.KeysTouched > 0 {
		out["pruned_keys"] = res.KeysTouched
		out["pruned_models"] = res.ModelsGone
	}
}

// ── 状态（批量）────────────────────────────────────────────────────────────

// channelsStatusBatch POST /admin/channels/status-batch  {"ids":[...],"status":2} 或 {"tag":"x","status":2}
//
// 返回**真正变化**的条数（store.SetChannelsStatus 的返回值）：批量操作最容易
// 被误读成"点了没反应"，而这个数字就是"有反应"的证据。
func (h *Handler) channelsStatusBatch(w http.ResponseWriter, r *http.Request) {
	if h.channelsStore() == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		IDs    []string `json:"ids"`
		Tag    string   `json:"tag"`
		Status int      `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	if !store.IsValidChannelStatus(req.Status) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status 只能是 1(启用)/2(手动停用)/3(自动停用)"})
		return
	}
	tag := strings.TrimSpace(req.Tag)
	var rows []store.Channel
	var missing []string
	var err error
	if tag != "" {
		rows, err = h.channelRowsByTag(tag)
	} else {
		if len(req.IDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ids 或 tag 至少给一个"})
			return
		}
		rows, missing, err = h.channelRowsByExternalIDs(req.IDs)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	changed, err := store.SetChannelsStatus(ids, req.Status, channelStatusReason(req.Status))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "改状态失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "matched": len(ids), "changed": changed, "missing": missing})
}

// channelStatusReason 批量改状态时写进 status_reason 的原因。
//
// 启用（1）必须**清空**原因：留着"手动停用"的原因会让界面显示一条已启用的渠道
// 带着停用理由，用户分不清它到底是不是好的。
func channelStatusReason(status int) string {
	if status == store.ChannelStatusEnabled {
		return ""
	}
	return channelstore.ReasonManual
}

// ── 复制 ───────────────────────────────────────────────────────────────────

// channelsCopy POST /admin/channels/copy  {"id":"ch-x","suffix":"_复制","reset_balance":true}
//
// 复制出来的行**不继承 external_id**（唯一约束 + 歧义，见 store.CopyChannel 的注释），
// 所以这里给它分配一个新的 ch-xxxx：不分配的话，前端拿到的新渠道没法被寻址
// （列表里 id 为空 ⇒ 编辑/停用/删都点不动）。
func (h *Handler) channelsCopy(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID           string `json:"id"`
		Suffix       string `json:"suffix"`
		ResetBalance *bool  `json:"reset_balance"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	row, ok := h.channelRowByExternalID(req.ID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道不存在: " + req.ID})
		return
	}
	suffix := req.Suffix
	if suffix == "" {
		suffix = "_复制"
	}
	// reset_balance 缺省 true：复制是"再开一条"，继承旧渠道的用量会让统计与额度判断失真。
	resetBalance := true
	if req.ResetBalance != nil {
		resetBalance = *req.ResetBalance
	}
	newID, err := store.CopyChannel(row.ID, suffix, resetBalance)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "复制失败: " + err.Error()})
		return
	}
	fresh, err := store.GetChannel(newID)
	if err != nil || fresh == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "复制成功但读不回新行: " + errText(err)})
		return
	}
	ext := fresh.ExternalID
	if ext == "" {
		ext = newChannelID()
		if _, err := store.SetChannelExternalID(newID, ext); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "分配外部 id 失败: " + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": ext, "source": req.ID, "name": fresh.Name,
		"reset_balance": resetBalance, "suffix": suffix,
	})
}

// ── 修复能力 ───────────────────────────────────────────────────────────────

// channelsFixAbilities POST /admin/channels/fix-abilities
//
// 修"渠道声明了模型、能力行却缺/多"的不一致（store.FixAllAbilities 内部有 fixLock 防并发）。
// 返回成功/失败条数 —— 只说"ok"而不给数字的话，用户没法判断它到底修没修东西。
func (h *Handler) channelsFixAbilities(w http.ResponseWriter, r *http.Request) {
	if h.channelsStore() == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	fixed, failed, err := store.FixAllAbilities()
	out := map[string]any{"ok": err == nil, "fixed": fixed, "failed": failed}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// ── 优先级排序三件套（hyxapi 的 order / reorder / move）─────────────────────

// channelsOrder GET /admin/channels/order —— 读当前顺序（priority DESC, id DESC，与列表一致）。
func (h *Handler) channelsOrder(w http.ResponseWriter, r *http.Request) {
	if h.channelsStore() == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	rows, err := store.ListChannels()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读渠道失败: " + err.Error()})
		return
	}
	sortChannelRows(rows, "priority", "desc")
	writeJSON(w, http.StatusOK, map[string]any{"order": channelOrderItems(rows), "count": len(rows)})
}

// channelOrderItem 排序列表的一项（只给排序需要的三个字段）。
type channelOrderItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Priority int64  `json:"priority"`
}

func channelOrderItems(rows []store.Channel) []channelOrderItem {
	out := make([]channelOrderItem, 0, len(rows))
	for i := range rows {
		out = append(out, channelOrderItem{
			ID: rows[i].ExternalID, Name: rows[i].Name, Priority: derefInt64(rows[i].Priority),
		})
	}
	return out
}

// channelsReorder POST /admin/channels/reorder  {"ids":["ch-a","ch-b",...]}
//
// ids 的顺序就是**目标展示顺序**（第一个最优先）。优先级按 N..1 重排，
// 全量重排而不是"只动这几条"：只动一部分时，剩下的渠道优先级不变，
// 结果就是"排完序之后顺序又变了"——用户看到的是排序没生效。
func (h *Handler) channelsReorder(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ids 必填（顺序即目标顺序）"})
		return
	}
	updated, missing, err := h.renumberChannels(s, req.IDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": updated, "missing": missing})
}

// channelsMove POST /admin/channels/move —— **两种形态都接受**（T72）。
//
// # 形态 A：单条相对移动（拖拽排序用的那个）
//
//	{"id":"ch-a","target_id":"ch-b","position":"before"|"same"|"after"}
//
// 把 id 落到 target_id 的对应位置，**其余保持相对次序**（只动一条）。
// position 的三档照前端提示框的字面语义（webui-channels.html:2197-2198）：
//
//	before  插到目标**之前**
//	same    与目标同位置（同级）—— 前端注释原文：「等价于插到目标之前但不改它的层级」。
//	        本仓的渠道**没有"层级"这一维**（只有一条 priority 序），所以这里 same 与
//	        before 同效，这是**照前端语义实现**，不是遗漏。
//	after   插到目标**之后**
//
// # 形态 B：上移/下移一位（既有形态，保留兼容）
//
//	{"id":"ch-x","direction":"up"|"down"}
//
// 与相邻一条交换。形态 B 早于 T72 存在（按钮/脚本在用），所以不能拿掉。
//
// # 边界处置（明确择一，不静默）
//
//	target_id 不存在        ⇒ 404 + 点名（与"渠道不存在"同一口径）
//	position 非法/缺省      ⇒ 400 + 三档取值
//	id == target_id         ⇒ **明确幂等**：200 + noop:true + reason（见下）
//	direction 与 target_id 同时给 ⇒ 400（两种形态二选一，避免"到底按哪个执行"的歧义）
//	两种形态都不给          ⇒ 400
//
// 为什么 id==target_id 选"幂等"而不是"400"：拖到自己身上在界面上就是"没动"，
// 报 400 会让前端弹一个用户无法理解的错误框；但**静默 200 也不行**（调用方分不清
// "成功移动了"与"根本没动"）。所以返回 200 + noop:true + reason，把事实说清楚。
// （前端自己也会在 drop 时短路 dragFrom === to，这里兜的是直接调接口的场景。）
//
// # 与 reorder 的分工（三条端点别打架）
//
//	reorder  按**给定的整份 ids 顺序**重排（撤销排序、导入顺序用它；调用方掌握全序）
//	move A   单条**相对**移动（拖拽落点语义：我要在谁的前/后）
//	move B   单条**邻位**移动（点一次上移/下移）
//
// 三者的持久化口径**统一**：都把整表优先级重写成 N..1（见 renumberChannels 的注释）。
// 局部改写优先级会产生并列值与空洞，而并列会让"同样的操作两次结果不同"。
func (h *Handler) channelsMove(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID        string `json:"id"`
		Direction string `json:"direction"`
		TargetID  string `json:"target_id"`
		Position  string `json:"position"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	dir := strings.ToLower(strings.TrimSpace(req.Direction))
	target := strings.TrimSpace(req.TargetID)
	pos := strings.ToLower(strings.TrimSpace(req.Position))

	// 形态判定：direction 与 target_id/position **二选一**（同时给 ⇒ 400，避免歧义）。
	relative := target != "" || pos != ""
	switch {
	case relative && dir != "":
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "direction 与 target_id/position 是两种形态，只能给一种",
		})
		return
	case !relative && dir == "":
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "要么给 direction（up/down），要么给 target_id + position（before/same/after）",
		})
		return
	}

	rows, err := store.ListChannels()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读渠道失败: " + err.Error()})
		return
	}
	sortChannelRows(rows, "priority", "desc")
	order := make([]string, 0, len(rows))
	for i := range rows {
		order = append(order, rows[i].ExternalID)
	}
	idx := indexOfString(order, req.ID)
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道不存在: " + req.ID})
		return
	}

	// ── 形态 B：邻位交换 ───────────────────────────────────────────────────
	if !relative {
		if dir != "up" && dir != "down" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "direction 只能是 up / down"})
			return
		}
		swap := idx - 1
		if dir == "down" {
			swap = idx + 1
		}
		if swap < 0 || swap >= len(order) {
			// 已经在顶端/底端：不是错误（前端会把按钮置灰，但接口被直接调用时也不该 400）。
			h.respondChannelMove(w, "step", rows, true)
			return
		}
		order[idx], order[swap] = order[swap], order[idx]
		after, uerr := h.applyChannelOrder(s, order)
		if uerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": uerr.Error()})
			return
		}
		h.respondChannelMove(w, "step", after, false)
		return
	}

	// ── 形态 A：相对移动 ───────────────────────────────────────────────────
	if pos != "before" && pos != "same" && pos != "after" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "position 只能是 before / same / after（收到 " + req.Position + "）",
		})
		return
	}
	if target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "给了 position 就必须给 target_id"})
		return
	}
	if target == req.ID {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "mode": "relative", "updated": 0, "noop": true,
			"reason": "id 与 target_id 相同 —— 拖到自己身上等于没动，按幂等处理（不是错误，也不是静默成功）",
			"order":  channelOrderItems(rows),
		})
		return
	}
	if indexOfString(order, target) < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "目标渠道不存在: " + target})
		return
	}
	next := moveRelativeInOrder(order, req.ID, target, pos)
	after, uerr := h.applyChannelOrder(s, next)
	if uerr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": uerr.Error()})
		return
	}
	h.respondChannelMove(w, "relative", after, false)
}

// moveRelativeInOrder 把 id 移到 target 的前/后（same 与 before 同效，见调用方注释）。
//
// 纯函数：输入当前全序，输出新全序（长度不变、元素不增不减）。
// 其余元素的**相对次序**保持不变 —— 这正是"只动一条"的含义。
func moveRelativeInOrder(order []string, id, target, pos string) []string {
	rest := make([]string, 0, len(order))
	for _, x := range order {
		if x != id {
			rest = append(rest, x)
		}
	}
	at := indexOfString(rest, target)
	if at < 0 {
		return order // 调用方已经校验过，兜底：原样返回（绝不丢元素）
	}
	if pos == "after" {
		at++
	}
	out := make([]string, 0, len(rest)+1)
	out = append(out, rest[:at]...)
	out = append(out, id)
	out = append(out, rest[at:]...)
	return out
}

// indexOfString 找下标（找不到 -1）。
func indexOfString(list []string, want string) int {
	for i, x := range list {
		if x == want {
			return i
		}
	}
	return -1
}

// applyChannelOrder 把整份顺序落成优先级 N..1，并**读回**重排后的行（以库为准）。
//
// 为什么读回而不是直接用入参顺序：renumberChannels 是逐条 Upsert，读回才能证明
// 它真的生效了（也顺带给出 name/priority，响应里那条 order 才是可核对的事实）。
func (h *Handler) applyChannelOrder(s *channelstore.Store, order []string) ([]store.Channel, error) {
	if _, _, err := h.renumberChannels(s, order); err != nil {
		return nil, err
	}
	rows, err := store.ListChannels()
	if err != nil {
		return nil, fmt.Errorf("重排后读回失败: %w", err)
	}
	sortChannelRows(rows, "priority", "desc")
	return rows, nil
}

// respondChannelMove 统一 move 的成功响应（三种结局共用同一形状，前端不必分支）。
func (h *Handler) respondChannelMove(w http.ResponseWriter, mode string, rows []store.Channel, atEdge bool) {
	resp := map[string]any{
		"ok": true, "mode": mode, "noop": false, "updated": len(rows), "order": channelOrderItems(rows),
	}
	if atEdge {
		resp["at_edge"] = true
		resp["updated"] = 0
	}
	writeJSON(w, http.StatusOK, resp)
}

// renumberChannels 把 ids 的顺序写成优先级 N..1（第一个最高）。
//
// ⚠ 走 channelstore 的 Get+Upsert（而不是直接写 priority 列）：Upsert 会顺带
// 重建能力行，直接改列会让"渠道优先级变了、能力行的 priority 还是旧的"。
func (h *Handler) renumberChannels(s *channelstore.Store, ids []string) (updated int, missing []string, err error) {
	n := len(ids)
	for i, ext := range ids {
		ch, ok := s.Get(ext)
		if !ok || ch == nil {
			missing = append(missing, ext)
			continue
		}
		want := n - i
		if ch.Priority == want {
			updated++
			continue
		}
		ch.Priority = want
		if uerr := s.Upsert(ch); uerr != nil {
			return updated, missing, uerr
		}
		updated++
	}
	return updated, missing, nil
}

// ── 小工具 ─────────────────────────────────────────────────────────────────

// channelRowByExternalID 按外部 id 取一行（空 id / 不存在 ⇒ false）。
// 这里不返回 error：调用方要的是"在不在"，DB 故障会由随后的写操作自己报出来。
func (h *Handler) channelRowByExternalID(id string) (*store.Channel, bool) {
	row, ok, err := store.GetChannelByExternalID(strings.TrimSpace(id))
	if err != nil {
		log.Printf("admin: 按外部 id %q 取渠道失败: %v", id, err)
		return nil, false
	}
	return row, ok && row != nil
}

// channelRowsByTag 取某个标签下的全部渠道行。
//
// store 的 ChannelQuery 没有 Tag 字段（B3 不改 store）⇒ 读全表后在内存里按 tag 过滤。
// 渠道数 13~25 量级，这条取舍与 channelstore.List 记的是同一条。
func (h *Handler) channelRowsByTag(tag string) ([]store.Channel, error) {
	rows, err := store.ListChannels()
	if err != nil {
		return nil, fmt.Errorf("读渠道失败: %w", err)
	}
	out := make([]store.Channel, 0, len(rows))
	for i := range rows {
		if derefString(rows[i].Tag) == tag {
			out = append(out, rows[i])
		}
	}
	return out, nil
}

// channelRowsByExternalIDs 批量解析外部 id ⇒ (存在的行, 没找到的 id)。
//
// "没找到"要如实回报：批量操作里静默跳过不存在的 id，会让用户以为"全删了"
// （实际只删了一部分）。
func (h *Handler) channelRowsByExternalIDs(ids []string) ([]store.Channel, []string, error) {
	out := make([]store.Channel, 0, len(ids))
	var missing []string
	for _, ext := range ids {
		row, ok := h.channelRowByExternalID(ext)
		if !ok {
			missing = append(missing, ext)
			continue
		}
		out = append(out, *row)
	}
	return out, missing, nil
}

// channelIntIDs 把外部 id 列表翻成 SQL 主键列表（供只吃 []int64 的 store 函数用）。
func channelIntIDs(ids []string) ([]int64, []string) {
	out := make([]int64, 0, len(ids))
	var missing []string
	for _, ext := range ids {
		row, ok, err := store.GetChannelByExternalID(strings.TrimSpace(ext))
		if err != nil || !ok || row == nil {
			missing = append(missing, ext)
			continue
		}
		out = append(out, row.ID)
	}
	return out, missing
}

// errText 把可能为 nil 的 error 转成字符串（用于"读不回新行"这类组合错误）。
//
// ⚠ 刻意**不叫** errString：同包既有的 login_incognito_test.go 里已经有一个包级
// errString（测试里既当函数又当类型用），同名会让 `go test` 的目标整体编不过。
func errText(err error) string {
	if err == nil {
		return "未知原因"
	}
	return err.Error()
}
