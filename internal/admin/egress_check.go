// egress_check.go 核心的**出口自检**：POST /admin/accounts/egress-check。
//
// # 它回答的问题
//
// 「为什么这个上游的号全部失败？」—— 在只有错误文案的时候，答案往往是用户自己猜的：
// 他看到 `net/http: TLS handshake timeout`，去查账号、查上游公告，而真正的问题是他本机
// 那个代理没起来（TCP 连得上、TLS 没回音 —— 与「上游慢」长得一模一样）。
//
// 这个端点把「出口」变成一个**可主动测的对象**：逐条发一次最小请求，
// 回「哪条路通、哪条不通、不通是哪一类、该做什么」。
//
// # 为什么走任务槽而不是同步返回
//
// 死代理下每一条要等满探测超时（monkeycode 那边是 8s × 3 条 = 24s）。
// 同步返回会让按钮转 24 秒；任务槽本来就是这种「跑一会儿、回执里报结果」的形态
// （与跨上游签到同一条路：202 + 轮询 /admin/task）。
package admin

import (
	"context"
	"log"
	"net/http"
	"sort"
	"time"

	"workbuddy2api/internal/gateway"
)

// egressCheckTaskKind 任务槽里的类型名（前端 watchTask 按它显示）。
const egressCheckTaskKind = "egress-check"

// egressCheckTimeout 整趟自检的上限。
//
// 每条探测自己的超时在上游侧（monkeycode = 8s），这里只是**兜底**：
// 万一某个上游忘了给自己的探测设超时，也不能把任务槽占死（它只允许一个全量任务）。
const egressCheckTimeout = 90 * time.Second

// egressCheckers 注册表里**能实测出口**的上游（按 ID 排序，输出稳定）。
//
// 判据同其余扩展点：类型断言，不认上游名。没实现的上游**不列** —— 列了就是谎报
// （与 checkinProviders 同一条原则：回执里出现的每一项都必须真的跑过）。
func (h *Handler) egressCheckers() []gateway.Provider {
	if h.cfg.Registry == nil {
		return nil
	}
	out := make([]gateway.Provider, 0)
	for _, p := range h.cfg.Registry.All() {
		if ext, ok := gateway.ExtOf[gateway.EgressCheckExt](p); ok && ext != nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// accountsEgressCheck POST /admin/accounts/egress-check —— 触发出口自检（后台任务）。
func (h *Handler) accountsEgressCheck(w http.ResponseWriter, r *http.Request) {
	providers := h.egressCheckers()
	if len(providers) == 0 {
		// 一个都没实现时不假装成功：那要么是没启用上游，要么是接线问题。
		writeError(w, http.StatusNotImplemented,
			"没有任何上游实现了出口自检（gateway.EgressCheckExt）")
		return
	}
	slot, ok := h.cfg.TaskSlot.(taskSlotStarter)
	if !ok || slot == nil {
		writeError(w, http.StatusNotImplemented, "后台任务槽未接线，无法执行出口自检")
		return
	}
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.ID())
	}
	if !slot.Start(egressCheckTaskKind, func() []map[string]any {
		return h.runEgressCheck()
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"started":   true,
		"providers": names,
		"count":     len(names),
	})
}

// runEgressCheck 逐个上游跑它的出口探测，汇总成任务槽要的形状。
//
// # 为什么每个上游单独 recover
//
// 与跨上游签到同一条理由：一个上游 panic 不该让后面几个拿不到结果，
// 更不该带崩网关。
func (h *Handler) runEgressCheck() []map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), egressCheckTimeout)
	defer cancel()
	out := make([]map[string]any, 0)
	for _, p := range h.egressCheckers() {
		if ctx.Err() != nil {
			log.Printf("admin: 出口自检在跑到 %s 之前超时/取消，后续上游未执行", p.ID())
			break
		}
		out = append(out, egressProbesOf(ctx, p)...)
	}
	return out
}

// egressProbesOf 跑一个上游的出口探测，并 panic 兜底。
func egressProbesOf(ctx context.Context, p gateway.Provider) []map[string]any {
	ext, ok := gateway.ExtOf[gateway.EgressCheckExt](p)
	if !ok || ext == nil {
		return nil
	}
	var probes []gateway.EgressProbe
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("admin: 出口自检 %s panic: %v", p.ID(), rec)
				probes = []gateway.EgressProbe{{Name: "(panic)", OK: false, Kind: gateway.EgressKindNet, Hint: "执行 panic（已兜住，其它上游继续）"}}
			}
		}()
		probes = ext.EgressCheck(ctx)
	}()
	out := make([]map[string]any, 0, len(probes))
	for _, pr := range probes {
		status := "ok"
		detail := pr.Via
		if !pr.OK {
			status = "fail"
			detail = pr.Via + " · " + pr.Hint
			if pr.Detail != "" {
				detail += "（" + pr.Detail + "）"
			}
		}
		out = append(out, map[string]any{
			// uid 位置放「上游 · 探测名」：任务回执是按行渲染的，这一列是给人看的主键。
			"uid":    p.ID() + " · " + pr.Name,
			"status": status,
			"detail": detail,
			"ms":     pr.MS,
		})
	}
	return out
}
