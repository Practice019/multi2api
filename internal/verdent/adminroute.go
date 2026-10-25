// adminroute.go 本上游的管理端点。
//
// # 为什么必须有（不是"锦上添花"）
//
// 契约测试有一条硬判据：声明了 CapModels / CapQuotaProbe / CapImport
// 就**必须实现 AdminExt** —— 这些能力的可操作性只能通过管理端点暴露。
// 不实现时契约直接判不合格（「声明了能力但没实现 AdminExt」）。
//
// 这也是用户那句「少了很多东西，你对齐其他上游」的技术落点：
// 界面上的按钮由**能力位**驱动，而能力位要求背后有真端点。
//
// # 只读优先
//
// 打开面板是高频路径，绝不碰写端点 —— 所以额度/模型两条都是 GET。
// （loomy 上踩过「打开面板即意外触发签到」。）
package verdent

import (
	"encoding/json"
	"net/http"
	"strings"

	"workbuddy2api/internal/gateway"
)

// 管理端点路径。
const (
	adminQuotaPath   = "/admin/verdent/quota"
	adminModelsPath  = "/admin/verdent/models"
	adminRefreshPath = "/admin/verdent/quota-refresh"
)

// AdminRoutes 本上游的管理端点。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			// 额度：账号池表格里已经有一列显示（QuotaExt），
			// 再单开一个导航页只是重复，而且它渲染成原始 JSON。
			// 所以 Hidden —— 路由照挂，只是不生成左侧标签。
			Method:     http.MethodGet,
			Path:       adminQuotaPath,
			Handler:    p.handleQuota,
			Capability: gateway.CapQuotaProbe,
			Title:      "额度",
			Hidden:     true,
		},
		{
			// 模型目录：/v1/models 与「可用模型」页已经列全，这里再开一个页是重复。
			// 但端点要留 —— 排查"某个模型走哪条协议"时它是唯一入口。
			Method:     http.MethodGet,
			Path:       adminModelsPath,
			Handler:    p.handleModels,
			Capability: gateway.CapModels,
			Title:      "模型目录",
			Hidden:     true,
		},
		{
			// 批量刷新本上游全部账号的额度。
			// 「刷新本上游额度」按钮打的就是它（前端按 CapQuotaProbe 显隐）。
			Method:     http.MethodPost,
			Path:       adminRefreshPath,
			Handler:    p.handleQuotaRefreshAll,
			Capability: gateway.CapQuotaProbe,
			Title:      "刷新本上游额度",
			Hidden:     true,
		},
		{
			// 页内「添加账号」的粘贴完成端点（由 ManualLogin 报给前端）。
			Method:     http.MethodPost,
			Path:       adminManualCompletePath,
			Handler:    p.handleManualComplete,
			Capability: 0, // 通用动作，不对应某个能力位
			Title:      "粘贴 token 完成添加账号",
			Hidden:     true,
		},
	}
}

// handleQuota 单账号额度（GET ?uid=）。
func (p *Provider) handleQuota(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 uid"})
		return
	}
	q, ok := p.RefreshQuota(uid)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有这个账号"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "quota": q})
}

// handleModels 模型目录（GET）。
func (p *Provider) handleModels(w http.ResponseWriter, r *http.Request) {
	a := p.firstAuth()
	list := KnownModels()
	var errMsg string
	if a != nil {
		got, err := p.client().FetchModels(r.Context(), a.Token)
		if err != nil {
			errMsg = err.Error() // 如实回报"这次拉失败、用的是兜底表"
		} else {
			list = got
		}
	}
	ids := make([]string, 0, len(list))
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	out := map[string]any{"ok": true, "count": len(ids), "models": ids}
	if errMsg != "" {
		out["warning"] = "实时拉取失败，以下为静态兜底表：" + errMsg
	}
	writeJSON(w, http.StatusOK, out)
}

// handleQuotaRefreshAll 批量刷新本上游全部账号的额度。
//
// ⚠ 只**探测并回结果**，不写账号池 —— 写池是核心的职责（Pool.SetQuota），
// 上游包不得 import internal/pool。前端拿到 results 后由核心的批量刷新接口落库；
// 单账号那条路径（QuotaExt.RefreshQuota）才由核心直接写。
func (p *Provider) handleQuotaRefreshAll(w http.ResponseWriter, r *http.Request) {
	uids := p.UIDs()
	type row struct {
		UID   string            `json:"uid"`
		Quota gateway.QuotaView `json:"quota"`
	}
	rows := make([]row, 0, len(uids))
	for _, uid := range uids {
		if q, ok := p.RefreshQuota(uid); ok {
			rows = append(rows, row{UID: uid, Quota: q})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(rows), "results": rows})
}

// firstAuth 取任意一份可用凭证（拉模型目录用）。
func (p *Provider) firstAuth() *Auth {
	for _, uid := range p.UIDs() {
		if a := p.authOfUID(uid); a != nil {
			return a
		}
	}
	return nil
}

// ---- 本包自用的小助手 ----
//
// # 为什么这里各留一份（而不是复用别的包）
//
// 它们都只有几行，而各自的实现是**未导出**的（gateway / server 里那份外面拿不到）。
// 为了省几行去加宽一个共享库的公开面，代价比收益大 ——
// 与 internal/openaicompat/util.go 里留第二份 truncate 是同一条理由。

// writeJSON 写一个 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonDecode 解请求体（限制 4MiB，防超大 body 把我们 OOM）。
func jsonDecode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	return dec.Decode(v)
}

// shortErr 把错误压成一行短文本（给界面提示用；nil 时给兜底文案）。
func shortErr(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	return truncate(strings.ReplaceAll(err.Error(), "\n", " "), 160)
}
