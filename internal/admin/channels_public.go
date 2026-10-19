// channels_public.go 管理端视角的「对外模型」。
//
// # 为什么需要它（而不是让前端去调 /v1/models）
//
// /v1/models 的可见性取决于**请求用的那把 key**（白名单是 key 的属性）。管理页问的是另一个问题：
// 「**我配的渠道**现在让外面看见什么」—— 它不该依赖"管理台手里那把 key 恰好是什么"。
//
// 所以这里用同一套规则（internal/modelvis）但**不叠令牌白名单**，并把每条模型的**来源渠道**
// 与**被挡原因**一起回给前端，让"某个模型看不见"能一眼定位：
//
//	渠道没声明/停用了  → 它根本不在 ability_count 里
//	映射把它改名了      → 显示的是公开名，附 upstream 告诉你是哪条
//	被某把 key 的名单挡 → 那是**key 维度**的事，不是这里；前端要按 key 看就去看 /admin/apikeys
package admin

import (
	"net/http"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/modelmap"
	"workbuddy2api/internal/modelvis"
)

// publicModelView 一条对外可见的模型。
type publicModelView struct {
	Public   string   `json:"public"`   // 对外名字（客户端请求时写这个）
	Upstream string   `json:"upstream"` // 上游真实名（未映射时与 public 相同）
	Channels []string `json:"channels"` // 能服务它的启用渠道名（按优先级降序，前端据此显示"谁提供的"）
}

// channelsPublicModels GET /admin/channels/public-models
//
// 免 key 的管理视角：现在对外可见什么、每条由谁提供。
func (h *Handler) channelsPublicModels(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}

	abilities := s.EnabledModels(channelstore.DefaultGroup)
	reverse, forward := h.channelMaps()

	// 用与 /v1/models 完全相同的规则（protected=false：管理视角不叠 key 白名单）
	public := modelvis.PublicModels(abilities, nil, false, reverse)

	views := make([]publicModelView, 0, len(public))
	for _, name := range public {
		v := publicModelView{Public: name, Upstream: name}
		// 公开名 → 上游名：由 reverse 反查（同一份表，两个方向不能各写一遍）
		for real, pub := range reverse {
			if pub == name {
				v.Upstream = real
				break
			}
		}
		v.Channels = channelsForUpstream(s, v.Upstream, forward)
		views = append(views, v)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models":       views,
		"public_count": len(views),
		// 能力索引规模：与 public_count 不等时，差值就是"被映射折叠掉/改名了"的量
		"ability_count": len(abilities),
		// 启用/停用渠道数：停用的渠道**不产生**能力，是"看不见某模型"的常见原因
		"enabled_channels": countChannelsByStatus(s, channelstore.StatusEnabled),
		"total_channels":   len(s.List()),
	})
}

// channelMaps 一次遍历同时拿到两个方向：reverse（上游名→公开名）与 forward（公开名→上游名）。
//
// ⚠ 为什么同时要两个方向：显示要用 reverse（把上游名翻成公开名），
// 而"谁提供这条模型"必须用 forward 反推（渠道声明的是上游名）。
// 两个方向都从同一份 channel.ModelMap 走 ⇒ 不会分叉。
func (h *Handler) channelMaps() (reverse, forward map[string]string) {
	s := h.channelsStore()
	if s == nil {
		return nil, nil
	}
	reverse, forward = map[string]string{}, map[string]string{}
	for _, ch := range s.List() {
		if len(ch.ModelMap) == 0 {
			continue
		}
		rev, _ := modelmap.Mapping(ch.ModelMap).Reverse()
		for real, pub := range rev {
			if _, ok := reverse[real]; !ok {
				reverse[real] = pub
			}
		}
		for pub, real := range ch.ModelMap {
			if _, ok := forward[pub]; !ok {
				forward[pub] = real
			}
		}
	}
	return reverse, forward
}

// channelsForUpstream 哪些启用渠道声明了这个上游模型（优先级降序 ⇒ 与选路顺序一致）。
func channelsForUpstream(s *channelstore.Store, upstream string, _ map[string]string) []string {
	type pair struct {
		name string
		prio int
	}
	var hits []pair
	for _, ch := range s.List() {
		if ch.Status != channelstore.StatusEnabled {
			continue
		}
		for _, m := range ch.Models {
			if m == upstream {
				hits = append(hits, pair{name: ch.Name, prio: ch.Priority})
				break
			}
		}
	}
	// 优先级高的在前；同优先级按名字稳定排序（避免前端每次刷新顺序乱跳）
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0; j-- {
			if hits[j].prio > hits[j-1].prio || (hits[j].prio == hits[j-1].prio && hits[j].name < hits[j-1].name) {
				hits[j], hits[j-1] = hits[j-1], hits[j]
			} else {
				break
			}
		}
	}
	out := make([]string, 0, len(hits))
	for _, p := range hits {
		out = append(out, p.name)
	}
	return out
}

func countChannelsByStatus(s *channelstore.Store, status int) int {
	n := 0
	for _, ch := range s.List() {
		if ch.Status == status {
			n++
		}
	}
	return n
}
