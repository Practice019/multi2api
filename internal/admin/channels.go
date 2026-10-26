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
	"log"
	"net/http"
	"sort"
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

// channelsList GET /admin/channels
func (h *Handler) channelsList(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		// 与 apikeys 同款：未启用就回空集 + enabled=false（前端据此不渲染操作区）
		writeJSON(w, http.StatusOK, map[string]any{"channels": []channelView{}, "enabled": false})
		return
	}
	list := s.List()
	out := make([]channelView, 0, len(list))
	for _, ch := range list {
		out = append(out, viewOf(ch, ch.Models))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channels": out,
		"enabled":  true,
		// 能力索引的规模（"对外可服务的 (渠道,模型) 对数"）—— 一眼看出渠道是否真被展开成能力
		"ability_count": len(s.Abilities()),
	})
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
}

// channelsUpsert POST /admin/channels —— 新建（无 id）或更新（带 id）。
func (h *Handler) channelsUpsert(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req channelReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
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

	ch := &channelstore.Channel{
		ID: req.ID, Name: req.Name, Type: req.Type, BaseURL: strings.TrimRight(req.BaseURL, "/"),
		Models: req.Models, ModelMap: req.ModelMapping, Group: req.Group, Groups: req.Groups,
		Priority: req.Priority, Weight: req.Weight, Status: req.Status,
	}
	if ch.Status == 0 {
		ch.Status = channelstore.StatusEnabled
	}
	created := false
	if ch.ID == "" {
		ch.ID = newChannelID()
		created = true
	} else if old, ok := s.Get(ch.ID); ok {
		// 更新：保留原 key（表单不回显明文 ⇒ 不回传 keys 就表示"不改 key"）
		// 以及原 ChannelInfo（key 选择模式是既有设置，不能因为这次没传就被清掉）。
		if len(req.Keys) == 0 {
			ch.Keys = old.Keys
		}
		ch.ChannelInfo = old.ChannelInfo
	}
	// 追加 key，**按内容去重**：同一把 key 填两遍会让轮询连续选中同一个条目
	// （界面上看是"轮询没生效"，实际是数据里有两份相同的 key）。
	seen := make(map[string]bool, len(ch.Keys)+len(req.Keys))
	for _, k := range ch.Keys {
		seen[k.Key] = true
	}
	for _, raw := range req.Keys {
		k := strings.TrimSpace(raw)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		ch.Keys = append(ch.Keys, channelstore.ChannelKey{Key: k, Status: channelstore.StatusEnabled})
	}
	// 选择模式：空 = 保持原值；显式给了才改（并且只认两个合法值，
	// 因为第三个值会让 PickKeyIdx 静默落回随机 —— 用户以为设了轮询其实没设）。
	if req.KeySelectionMode != "" {
		switch req.KeySelectionMode {
		case "random", "round_robin":
			ch.ChannelInfo.KeySelectionMode = req.KeySelectionMode
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "key_selection_mode 只能是 random / round_robin（空 = 保持原值）",
			})
			return
		}
	}
	ch.ChannelInfo.MultiKey = len(ch.Keys) > 1
	if err := s.Upsert(ch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "created": created, "channel": viewOf(ch, ch.Models)})
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
	if err := s.SetStatus(req.ID, req.Status); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
