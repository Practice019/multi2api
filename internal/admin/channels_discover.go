// channels_discover.go 「模型自动发现」：POST /admin/channels/discover
//
// # 它解决什么
//
// Key 型渠道的模型列表要么手抄、要么去上游问。手抄必然抄错（上游改名/加模型时更明显），
// 而"问上游"这件事**只有上游自己知道** —— 所以这里不是让管理台认识某个上游，
// 而是经注入的 `cfg.ChannelProvider`（类型取自 gateway 契约）拿一个 Provider 来问。
//
// ⚠ 为什么不在这里 import 任何具体上游包：见 admin.go 里那段注释（当年的 Upstream 字段
// 就是因为这个被删的）。管理台一旦认识某个具体上游，就会长出"只对某一个上游成立的行为"。
//
// # 三种入参形态（对应三种真实用法）
//
//	{"id":"ch-x"}                      ⇒ 用已存渠道的 BaseURL + 第一个启用 key 去问（最常见：编辑时点"自动发现"）
//	{"base_url":"...","key":"..."}     ⇒ **还没保存**就先试一次（新建表单里"先看它有什么再决定"）
//	{"id":"ch-x","save":true}          ⇒ 问完把结果**写回该渠道的 models**（一键回填）
//
// ⚠ save 的语义刻意是"覆盖 models"而不是"合并"：合并会让上游删掉的模型永远赖在列表里，
// 而那个模型的请求会一直失败 —— 覆盖才反映上游当前的事实。想保留手工项就别用 save。
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/store"
)

type discoverReq struct {
	ID      string `json:"id"`
	BaseURL string `json:"base_url"`
	Key     string `json:"key"`
	Save    bool   `json:"save"`
}

func (h *Handler) channelsDiscover(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	if h.cfg.ChannelProvider == nil {
		// 接线问题，不是用户输入问题 ⇒ 说清楚，别让用户去猜"是不是我的 BaseURL 写错了"
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "服务端未注入 ChannelProvider（组合根漏接线），无法向上游查询模型"})
		return
	}
	var req discoverReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}

	var ch *channelstore.Channel
	if req.ID != "" {
		got, ok := s.Get(req.ID)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道不存在: " + req.ID})
			return
		}
		ch = got
	}
	baseURL, key := req.BaseURL, req.Key
	if ch != nil {
		if baseURL == "" {
			baseURL = ch.BaseURL
		}
		if key == "" {
			key = firstEnabledKey(ch)
		}
	}
	if baseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "base_url 必填（或给一个已存在的渠道 id）"})
		return
	}
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "没有可用的 key：该渠道没有启用状态的 key，且请求里也没带 key"})
		return
	}

	id, name := "discover", "discover"
	if ch != nil {
		id, name = ch.ID, ch.Name
	}
	p := h.cfg.ChannelProvider(id, baseURL)
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ChannelProvider 工厂返回了 nil"})
		return
	}
	cred := gateway.Credential{
		Provider: id, UID: id, Nickname: name,
		// 与转发路径同一个跨上游最小公共形状（核心不知道"哪种上游怎么读 key"）
		Secret: map[string]any{"api_key": key},
	}
	ms, err := p.Models(r.Context(), cred)
	if err != nil {
		// ⚠ 错误里**不得**出现 key 明文（provider 已经保证了这一点，这里再声明一次意图）
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "向上游查询模型失败: " + err.Error(),
			"hint":  "检查 Base URL 是否指向 OpenAI 兼容端点（通常以 /v1 结尾）与 key 是否有效",
		})
		return
	}
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids) // 稳定输出：前端每次点"自动发现"看到的顺序一致

	saved := false
	if req.Save && ch != nil && len(ids) > 0 {
		updated := *ch
		updated.Models = ids
		if err := s.Upsert(&updated); err != nil {
			writeJSON(w, http.StatusInternalServerError,
				map[string]any{"error": "已查到模型但写回失败: " + err.Error(), "models": ids})
			return
		}
		saved = true
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models": ids,
		"count":  len(ids),
		"saved":  saved,
		// 空列表要能被区分：是真的上游没有模型，还是它回了个空数组
		"note": map[bool]string{true: "上游返回了 0 个模型（检查 Base URL 是否指向 /v1）",
			false: "ok"}[len(ids) == 0],
	})
}

// firstEnabledKey 取该渠道第一个**启用**状态的 key。
//
// ⚠ 不取停用的：停用 key 去问上游只会拿到 401，然后用户会以为"自动发现功能坏了"。
func firstEnabledKey(ch *channelstore.Channel) string {
	for _, k := range ch.Keys {
		if k.Status == channelstore.StatusEnabled && k.Key != "" {
			return k.Key
		}
	}
	return ""
}

// ── B3：测试渠道 / 余额 / 上游模型变更 ─────────────────────────────────────
//
// # 明确标注「不适用」的部分（不造假实现，任务书要求逐条说明）
//
// hyxapi 的渠道管理页还带这些子功能，它们对应的是 hyxapi 特有的**上游类型**，
// 本仓 13 个上游一个都不是这类，也没有对应的抽象（ChannelProvider 只认
// OpenAI 兼容的 BaseURL + api_key）：
//
//	Ollama 渠道类型            —— 本仓没有本地模型运行时渠道（渠道只有 OpenAI 兼容一种）。
//	Codex 用量查询             —— 本仓没有 Codex 上游，也没有"用量"这一维度
//	                              （额度在账号池侧，不在渠道侧）。
//	vLLM / SGLang 渠道类型     —— 同上，没有自托管推理框架渠道。
//	任务插件绑定（plugin）     —— 本仓的任务机制在 internal/scheduler/codearts 等上游包内部，
//	                              与"渠道"不是一个维度，没有可绑定的对应物。
//
// 这四条**不做占位端点**：一个永远回空/永远 501 的端点比没有更糟（前端会以为
// 功能存在，用户点了却什么都拿不到）。前端按 manifest 的能力位不渲染它们即可。

// b3ChannelTargets 解析"对谁做"（单条 or 全量）。
//
// 两种形态：{"id":"ch-x"} 单条；{"all":true} 全量。
// 全量的执行方式：**同步顺序执行**（见下面每个端点的说明），不引入后台任务。
func (h *Handler) b3ChannelTargets(w http.ResponseWriter, id string, all bool) ([]*channelstore.Channel, bool) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return nil, false
	}
	if h.cfg.ChannelProvider == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "服务端未注入 ChannelProvider（组合根漏接线），无法向上游查询"})
		return nil, false
	}
	if strings.TrimSpace(id) != "" {
		ch, ok := s.Get(strings.TrimSpace(id))
		if !ok || ch == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道不存在: " + id})
			return nil, false
		}
		return []*channelstore.Channel{ch}, true
	}
	if !all {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 或 all:true 至少给一个"})
		return nil, false
	}
	rows := s.List()
	out := make([]*channelstore.Channel, 0, len(rows))
	for _, ch := range rows {
		if ch != nil {
			out = append(out, ch)
		}
	}
	return out, true
}

// upstreamModelIDs 用某条渠道的 BaseURL + 第一个启用 key 问一次上游模型目录。
//
// 与 /discover 走**同一条链路**（cfg.ChannelProvider 注入的 Provider）：
// 核心不认识具体上游，"测试"这件事只能是"向上游要一次它的模型目录"。
// 返回耗时（毫秒）：测试渠道的 response_time 就是它。
func (h *Handler) upstreamModelIDs(ctx context.Context, ch *channelstore.Channel) ([]string, int64, error) {
	if ch == nil {
		return nil, 0, fmt.Errorf("渠道为空")
	}
	key := firstEnabledKey(ch)
	if key == "" {
		return nil, 0, fmt.Errorf("该渠道没有启用状态的 key")
	}
	p := h.cfg.ChannelProvider(ch.ID, ch.BaseURL)
	if p == nil {
		return nil, 0, fmt.Errorf("ChannelProvider 工厂返回了 nil")
	}
	cred := gateway.Credential{
		Provider: ch.ID, UID: ch.ID, Nickname: ch.Name,
		Secret: map[string]any{"api_key": key},
	}
	start := time.Now()
	ms, err := p.Models(ctx, cred)
	cost := time.Since(start).Milliseconds()
	if err != nil {
		return nil, cost, err
	}
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, cost, nil
}

// channelsTest POST /admin/channels/test  {"id":"ch-x"} 或 {"all":true}
//
// # 「测试渠道」在本仓是什么（不伪造）
//
// 本仓的渠道是 **OpenAI 兼容的 Key 型渠道**（BaseURL + key）。可用的"测试"动作
// 就是向上游要一次它的模型目录：拿到 = 连通且凭据可用；拿不到 = 如实报错。
// 刻意**不**发一次 chat 探测：那会消耗额度、依赖具体模型名，而"目录能拿到"
// 已经证否了最要紧的两类故障（地址错、key 失效）。
//
// 全量：**同步顺序执行**。理由：渠道数 13~25，每条一次 HTTPS 调用（provider 自带超时），
// 最坏几秒；换成后台任务要引入任务表 + 进度轮询 + 结果缓存三个新面，成本远大于收益。
// 若将来渠道上百条，再改成后台任务（那时"同步会超时"才会成为真问题）。
//
// 每条无论成败都写 test_time/response_time —— 界面上"上次测试"必须是**真的跑过**的时刻。
func (h *Handler) channelsTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	targets, ok := h.b3ChannelTargets(w, req.ID, req.All)
	if !ok {
		return
	}
	ctx := r.Context()
	results := make([]map[string]any, 0, len(targets))
	okCount, failCount := 0, 0
	for _, ch := range targets {
		ids, cost, err := h.upstreamModelIDs(ctx, ch)
		item := map[string]any{"id": ch.ID, "name": ch.Name, "ms": cost}
		if err != nil {
			item["ok"] = false
			item["error"] = err.Error()
			failCount++
		} else {
			item["ok"] = true
			item["models"] = len(ids)
			okCount++
			if len(ids) == 0 {
				// 200 但空目录：这是"上游答了、但没有模型"，与"连不上"是两码事。
				item["note"] = "上游应答成功但模型目录为空 —— 检查 Base URL 是否指向 /v1"
			}
		}
		if uerr := h.touchChannelTestResult(ch.ID, cost); uerr != nil {
			item["store_error"] = uerr.Error()
		}
		results = append(results, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": failCount == 0, "total": len(targets), "passed": okCount, "failed": failCount,
		"mode":    map[bool]string{true: "single", false: "all"}[len(targets) == 1],
		"results": results,
	})
}

// touchChannelTestResult 写 test_time / response_time。
//
// ⚠ 走 store.GetChannel + UpdateChannel（而不是裸 UPDATE）：UpdateChannel 对
// 空 Key 的语义是"不传就不改"（见它的注释），所以拿一条 Omit("key") 的行回写
// **不会**把明文 key 抹掉 —— 这正是这里敢整行回写的前提。
func (h *Handler) touchChannelTestResult(externalID string, ms int64) error {
	row, ok := h.channelRowByExternalID(externalID)
	if !ok || row == nil {
		return fmt.Errorf("渠道 %s 不存在", externalID)
	}
	row.TestTime = time.Now().Unix()
	row.ResponseTime = int(ms)
	return store.UpdateChannel(row)
}

// channelsBalance POST /admin/channels/balance  {"id":"ch-x"} 或 {"all":true}
//
// # 「余额查询」在本仓的诚实结论：不支持
//
// 本仓的渠道抽象只有 **OpenAI 兼容**一种（gateway.Provider 的 Models/Chat 两个方法），
// 它**没有余额接口**。所以这里逐条回 supported:false + 原因，并且
// **一个字节都不写 balance** —— 任务书写得很清楚：不要把"不支持"伪造成 0。
// （写 0 的后果是界面上显示"余额 0"，用户会以为额度用完了。）
//
// 将来接一个真有余额接口的上游时：在下面这个 switch 里按渠道类型分派，
// 并且**只在该类型真的支持时**调 store.UpdateChannelBalance。
func (h *Handler) channelsBalance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	targets, ok := h.b3ChannelTargets(w, req.ID, req.All)
	if !ok {
		return
	}
	results := make([]map[string]any, 0, len(targets))
	for _, ch := range targets {
		results = append(results, map[string]any{
			"id": ch.ID, "name": ch.Name,
			"supported": false,
			"balance":   nil,
			"reason":    "该渠道类型（OpenAI 兼容）没有余额查询接口；本仓不把「不支持」写成 0",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "supported": false, "total": len(targets),
		"note":    "本仓渠道抽象（gateway.Provider）没有余额方法 ⇒ 全部为 supported:false；未写任何 balance 列",
		"results": results,
	})
}

// channelsModelsCheck POST /admin/channels/models/check  {"id":"ch-x"} 或 {"all":true}
//
// 检测上游模型变更：把上游当前目录与该渠道**声明的** models 对比，给出
// added（上游有、本地没有）/ removed（本地有、上游没有）/ kept（两边都有）。
//
// 只报不改 —— 应用变更走 /admin/channels/models/apply（两步分开是刻意的：
// 上游抖动时"自动应用"会把用户手挑的模型删掉，而那是不可逆的编辑）。
func (h *Handler) channelsModelsCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	targets, ok := h.b3ChannelTargets(w, req.ID, req.All)
	if !ok {
		return
	}
	ctx := r.Context()
	results := make([]map[string]any, 0, len(targets))
	changedCount := 0
	for _, ch := range targets {
		upstream, _, err := h.upstreamModelIDs(ctx, ch)
		if err != nil {
			results = append(results, map[string]any{
				"id": ch.ID, "name": ch.Name, "ok": false, "error": err.Error(),
			})
			continue
		}
		added, removed, kept := diffChannelModels(ch.Models, upstream)
		if len(added) > 0 || len(removed) > 0 {
			changedCount++
		}
		results = append(results, map[string]any{
			"id": ch.ID, "name": ch.Name, "ok": true,
			"added": added, "removed": removed, "kept": kept,
			"upstream_count": len(upstream), "declared_count": len(ch.Models),
			"changed": len(added) > 0 || len(removed) > 0,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": len(targets), "changed": changedCount, "results": results,
	})
}

// channelsModelsApply POST /admin/channels/models/apply
//
//	{"id":"ch-x","mode":"overwrite"|"add"|"remove"}  或  {"all":true,"mode":"..."}
//
//	overwrite —— 完全按上游覆盖（= /discover 的 save；上游删掉的模型不再赖在列表里）
//	add       —— 只把上游新增的并进来（保留手工项）
//	remove    —— 只把上游已经没有的删掉（保留新模型，先不动）
//
// 写回走 channelstore 的 Get+Upsert：它会在同一事务里重建能力行，
// 直接改 models 列会让"渠道声明变了、能力索引还是旧的"（路由选不到新模型）。
func (h *Handler) channelsModelsApply(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	if h.cfg.ChannelProvider == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "服务端未注入 ChannelProvider（组合根漏接线），无法向上游查询"})
		return
	}
	var req struct {
		ID   string `json:"id"`
		All  bool   `json:"all"`
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	switch mode {
	case "overwrite", "add", "remove":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode 只能是 overwrite / add / remove"})
		return
	}
	targets, ok := h.b3ChannelTargets(w, req.ID, req.All)
	if !ok {
		return
	}
	ctx := r.Context()
	results := make([]map[string]any, 0, len(targets))
	applied := 0
	for _, ch := range targets {
		upstream, _, err := h.upstreamModelIDs(ctx, ch)
		if err != nil {
			results = append(results, map[string]any{"id": ch.ID, "ok": false, "error": err.Error()})
			continue
		}
		added, removed, _ := diffChannelModels(ch.Models, upstream)
		var next []string
		switch mode {
		case "overwrite":
			next = upstream
		case "add":
			next = append(append([]string{}, ch.Models...), added...)
		case "remove":
			keep := map[string]bool{}
			for _, r := range removed {
				keep[r] = true
			}
			next = make([]string, 0, len(ch.Models))
			for _, m := range ch.Models {
				if !keep[m] {
					next = append(next, m)
				}
			}
		}
		next = dedupeStrings(next)
		if len(next) == 0 && len(upstream) == 0 {
			// 空的覆盖会把渠道变成"什么都提供不了" —— 那更像上游抖动而不是用户的意图。
			results = append(results, map[string]any{
				"id": ch.ID, "ok": false, "skipped": true,
				"error": "上游目录为空 ⇒ 拒绝用空列表覆盖（渠道声明为空等于它不再服务任何模型）",
			})
			continue
		}
		cur, ok2 := s.Get(ch.ID)
		if !ok2 || cur == nil {
			results = append(results, map[string]any{"id": ch.ID, "ok": false, "error": "渠道读不到"})
			continue
		}
		cur.Models = next
		if uerr := s.Upsert(cur); uerr != nil {
			results = append(results, map[string]any{"id": ch.ID, "ok": false, "error": uerr.Error()})
			continue
		}
		applied++
		results = append(results, map[string]any{
			"id": ch.ID, "ok": true, "mode": mode, "count": len(next),
			"added": added, "removed": removed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "mode": mode, "applied": applied, "total": len(targets), "results": results,
	})
}

// diffChannelModels 比较"本地声明"与"上游目录"。
//
// 都按声明顺序/升序输出，便于前端稳定展示（集合语义，但输出有序）。
func diffChannelModels(declared, upstream []string) (added, removed, kept []string) {
	have := map[string]bool{}
	for _, m := range declared {
		have[m] = true
	}
	up := map[string]bool{}
	for _, m := range upstream {
		up[m] = true
	}
	for _, m := range upstream {
		if have[m] {
			kept = append(kept, m)
		} else {
			added = append(added, m)
		}
	}
	for _, m := range declared {
		if !up[m] {
			removed = append(removed, m)
		}
	}
	return added, removed, dedupeStrings(kept)
}

// dedupeStrings 去重并保序（空串丢弃）。
func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
