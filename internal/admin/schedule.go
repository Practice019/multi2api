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
	"log"
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

	// AccountColumns 该上游自报的**账号池列集**（有序的列 id）。
	//
	// # 空（字段不出现）= "用核心默认列"
	//
	// 这与"实现了扩展点但返回空数组"是两件不同的事（见
	// gateway.AccountColumnsExt 的注释）：前者是 workbuddy 的形态
	//（用户要求复用现在的表头，所以它后端零改动），后者是明确说"一列都不要"。
	// `omitempty` 让前者不出现在 JSON 里，前端把它读成 `undefined`
	// → 回落默认列。**前端不需要认识任何上游名**。
	AccountColumns []string `json:"accounts_columns,omitempty"`
}

// accountColumnsOf 问上游「账号池里你要哪些列」。
//
// 未实现扩展点 → 返回 nil（= 用默认列，字段不下发）。
//
// # 为什么要把"未知列 id"记成日志
//
// 列 id 是前后端之间的**字符串契约**。拼错一个字母不会编译失败、不会报错，
// 前端查不到那个 id 就**跳过** → 那一列静默消失。用户看到的只是"少了一列"，
// 分不清是漏了还是本来没有 —— 这与本项目反复吃过的"静默失效"是同一形态。
//
// 所以这里主动校验一次并留痕：日志里出现这行，说明**上游报错了列名**。
// 校验失败**不阻断**（照常下发，前端照样跳过）—— 账号列表不该因为
// 一个列名拼错就整片打不出来。
func accountColumnsOf(p gateway.Provider) []string {
	ext, ok := gateway.ExtOf[gateway.AccountColumnsExt](p)
	if !ok {
		return nil
	}
	cols := ext.AccountColumns()
	for _, c := range cols {
		if !gateway.IsKnownAccountColumn(c) {
			log.Printf("admin: 上游 %s 自报的账号列 id %q 不在规范词汇表里 —— "+
				"前端会跳过它（该列不会显示）。检查 gateway 的列 id 常量。", p.ID(), c)
		}
	}
	return cols
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
			// 登录能力：该上游**真的能**在页面内添加账号吗。
			//
			// ⚠ 判据是 `ExtOf` **加上** `Configured` —— 不能只看 ExtOf。
			//
			// 为了让 `ExtOf` 认出来，上游必须把 `Start`/`Poll` 挂在
			// Provider 自己身上（方法集要匹配，「返回接口的访问器」
			// 对 Go 的类型断言**不可见** —— 这是实测踩到的坑）。
			// 但挂上去之后，**没配登录流程的部署也会"看起来"实现了**。
			//
			// 所以加一层 `Configured()`：由上游自报"我这份配置真的能登录吗"。
			// 两道都满足才给按钮 —— 只满足一道会渲染出点了报错的假按钮。
			if lf, ok := gateway.ExtOf[gateway.LoginFlow](p); ok && lf.Configured() {
				info.Login = &providerLogin{Kind: "device", Label: "添加账号"}
			}
			// 账号池列集：上游自报（未实现 = nil = 用默认 11 列）。
			info.AccountColumns = accountColumnsOf(p)
			infos = append(infos, info)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": infos,
		"default":   h.cfg.DefaultProvider,
	})
}
