// 核心调度相关的两条只读端点。
//
// # 为什么它们住在 core（Task 3c 之后又被移回来）
//
// `/admin/schedule` 与 `/admin/task` **不表达任何上游身份** ——
// 一个报核心排的班（签到/保活启停、时点、下次唤醒），
// 一个报全量任务槽的状态。任何上游都该有这两条。
//
// Task 3c 把它们随另外 20 条一起搬进了 workbuddy，理由是
// "写方（签到/保活）在 workbuddy，读写不宜分家"。
// 阶段 0 评审指出这会**真出问题**：第二个上游若不声明 `CapCheckin`，
// 这两条路由就没人服务、且前端会按能力位把它们隐藏 ——
// 而它们本该对所有上游可见。
//
// 所以移回核心。任务槽由 cmd/server 注入（它的生命周期属于核心调度器）。
package admin

import (
	"net/http"
	"time"

	"workbuddy2api/internal/gateway"
)

// schedule GET /admin/schedule —— 下一次唤醒时刻与当前时点设置。
func (h *Handler) schedule(w http.ResponseWriter, r *http.Request) {
	sv := h.cfg.Scheduler
	if sv == nil {
		writeError(w, http.StatusNotImplemented, "调度器未接线")
		return
	}
	at, names := sv.NextWake()
	checkinH, keepaliveH := sv.Hours()
	resp := map[string]any{
		"checkin_enabled":   sv.CheckinEnabled(),
		"keepalive_enabled": sv.KeepaliveEnabled(),
		"checkin_hours":     checkinH,
		"keepalive_hours":   keepaliveH,
	}
	if !at.IsZero() {
		resp["next_at"] = at
		resp["next_in_sec"] = int64(time.Until(at).Seconds())
		resp["next_tasks"] = names
	}
	writeJSON(w, http.StatusOK, resp)
}

// taskStatus GET /admin/task —— 全量任务槽状态。
//
// 未注入任务槽时返回一个"空闲"形状的空快照，而不是 501 ——
// 前端按固定字段渲染，缺字段会让它显示异常。
func (h *Handler) taskStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.TaskSlot == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"running": false,
			"kind":    "",
			"results": nil,
		})
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.TaskSlot.Snapshot())
}

// providerInfo 一个已注册上游的对外描述（Task 8）。
//
// 前端用它做两件事：
//  1. 账号池按其 ID 分组渲染（uid 归属哪个上游一眼可见）
//  2. 按 Capabilities 决定显示哪些入口 —— workbuddy 显示成长/旅行，
//     codearts 显示福利/配额，互不干扰
//
// Capabilities 用**字符串名**（gateway.Capability.Names 的输出）而不是位掩码：
// 位掩码的数值是内部表示，前端不该依赖它；名字是稳定契约。
type providerInfo struct {
	ID           string   `json:"id"`
	Capabilities []string `json:"capabilities"`
	// Default 该上游是缺省上游（裸模型名走它）。
	Default bool `json:"default"`
	// AccountCount 该上游当前有多少账号（前端分组标题显示数量）。
	AccountCount int `json:"account_count"`
	// Login 该上游**是否支持在页面内添加账号**；不支持时为 null。
	//
	// # 为什么要有这个字段
	//
	// 添加账号是**上游专属**动作：workbuddy 是 OAuth 设备码，
	// codearts 是 OAuth + DPoP（步骤数都不同，见 gateway.LoginFlow 的注释）。
	// 前端"账号池"的每个上游分组行据此决定：
	//   · 有 login  → 渲染「＋ 添加账号」按钮
	//   · 没有      → 不渲染（**不放假按钮** —— 点了会走错上游或用不了）
	//
	// 判据是 Registry 里的**类型断言**（该上游有没有实现 LoginFlow），
	// 与 AdminExt / JobExt 同一个模式 —— 前端不写死上游名，
	// 加第三个上游时前端 0 改动。
	Login *providerLogin `json:"login"`
}

// providerLogin 登录能力的**声明**（不含任何实现细节）。
//
// Kind 用字符串而不是数字：它是稳定契约，前端据此决定渲染什么按钮。
// 目前只有 "device"（设备码/授权链接），将来可能有别的形态（如密钥对导入）。
type providerLogin struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// providers GET /admin/providers —— 已注册上游清单 + 能力位。
//
// 空注册表返回空列表而不是报错：前端要能在"未配置任何上游"的部署下
// 正常渲染空态，而不是拿到 500 后整页崩。
func (h *Handler) providers(w http.ResponseWriter, r *http.Request) {
	infos := []providerInfo{}
	if h.cfg.Registry != nil {
		for _, p := range h.cfg.Registry.All() {
			info := providerInfo{
				ID:           p.ID(),
				Capabilities: p.Caps().Names(),
				Default:      p.ID() == h.cfg.DefaultProvider,
			}
			// 账号数：按 provider 过滤统计（pool 的 ListFor 已支持）。
			if h.cfg.Pool != nil {
				info.AccountCount = len(h.cfg.Pool.ListFor(p.ID()))
			}
			// 登录能力：该上游有没有实现 gateway.LoginFlow。
			//
			// ⚠ 这是**类型断言**，不是"看上游名" —— 与本包 AdminExt / JobExt
			// 同一个模式（见 uimanifest.go 对 AdminExt 的用法）。
			// 前端据此决定分组行渲染「＋ 添加账号」还是不渲染。
			//
			// 当前只有 workbuddy 实现（codearts 的 DPoP 登录流程尚不存在），
			// 所以 codearts 的 login 是 null，前端不渲染按钮 —— 那是**正确**的：
			// 它真的做不到，放个按钮点了会失败。
			if _, ok := gateway.ExtOf[gateway.LoginFlow](p); ok {
				info.Login = &providerLogin{Kind: "device", Label: "添加账号"}
			}
			infos = append(infos, info)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": infos,
		"default":   h.cfg.DefaultProvider,
	})
}
