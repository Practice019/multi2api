// apikey_models.go API key 的**模型白名单**准入判断。
//
// # 用户的需求
//
//	「API管理里面，我们设置那个API，我觉得需要设置这个API可以用哪些模型？」
//
// 也就是：一把 key 只允许调用指定的模型，别的模型回 403。
//
// # 为什么判断放在出口层，而不是鉴权中间件
//
// `withAuth` 跑在**请求体被解析之前** —— 它拿不到 `model` 字段。
// 而"哪把 key 在调用"是鉴权阶段的事实（已放进 ctx，见 apikeyCtxKey），
// "要调哪个模型"是解析之后的事实。所以：身份在 withAuth 落定，
// 判断在各自 handler 里、拿到 model 之后立刻做。
//
// # 三个入口都要判，而且判的位置都在"拿到模型名之后、动手之前"
//
//	/v1/chat/completions   解析出 peek.Model 之后
//	/v1/images/generations 同上（生图模型与对话模型是两笔账）
//	/v1/models             **反向**：把列表裁成白名单里的那些
//
// 第三条不是可选的：一个被限制的 key 若在 /v1/models 里看见全部模型，
// 调一个就 403 一次 —— 用户会以为是网关坏了，而不是"这把 key 没这个权限"。
// 列表如实反映权限，权限才可发现。
package server

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"workbuddy2api/internal/gateway"
)

// modelAllowed 报告 model 是否被这份白名单放行（**唯一**一份实现）。
//
//	空白名单（nil / 空）→ 恒定 true（不限制）
//	非空               → 按**裸模型名**比（剥掉 "provider/" 前缀）
//
// # 为什么按裸名比，而不是逐字比
//
// 客户端发来的名字可能是 `glm-5.2`（裸名 → 走默认上游），也可能是
// `workbuddy/glm-5.2`（显式带上游前缀）—— 两者是**同一个模型**。
// 用户在白名单里填哪一种都是他的本意；逐字比会让另一种写法被拒，
// 而他完全看不出为什么（他填的名字明明对）。
//
// # 为什么这份实现只在 server 里有一份
//
// 剥前缀用的是 `gateway.SplitModel` —— 与**选路**同一个函数，所以"白名单
// 认的模型"与"请求会路由到的模型"不可能分叉。
//
// ⚠ 它**不能**放进 internal/apikey：import 了 gateway 的包会被
// discoverUpstreams 当成上游实现，于是核心依赖 apikey 就成了"核心依赖上游"
// （架构守卫实测抓到过）。而"在 apikey 里再写一份近似的剥前缀"更糟 ——
// 同一个判据两份实现，在"模型名本身含 '/'"时必然分叉。
// 结论：**存储留在 apikey（零依赖），解释留在 server（认识 gateway）**。
func modelAllowed(models []string, model string) bool {
	if len(models) == 0 {
		return true // 不限制
	}
	wantProv, want, wantHas := gateway.SplitModel(strings.TrimSpace(model))
	if want == "" {
		// 空模型名：白名单生效时不放行，让 403 发生在这里。
		// 放上去的话上游回 400，而 400 更容易被读成「网关坏了」。
		return false
	}
	for _, m := range models {
		gotProv, got, gotHas := gateway.SplitModel(strings.TrimSpace(m))
		if got != want {
			continue
		}
		// ⚠ 白名单条目带上游前缀时，前缀必须一起比。
		//
		// # 用户报的 bug（实测复现）
		//
		// 白名单 9 条、别名映射 9 条，但 DSH 的模型列表里出现第 10 个：
		//
		//	workbuddy-intl/deepseek-v4.1-flash     ← 多出来的
		//
		// 而白名单里只有 workbuddy/deepseek-v4.1-flash，没有 -intl。
		//
		// 根因：旧实现只比剥过前缀的裸名。SplitModel 按第一个 / 切，
		// 于是这两个是同一个裸名：
		//
		//			workbuddy/deepseek-v4.1-flash      → (workbuddy,      deepseek-v4.1-flash)
		//			workbuddy-intl/deepseek-v4.1-flash → (workbuddy-intl, deepseek-v4.1-flash)
		//
		// 但它们**是两个独立上游**（不同账号池、不同额度、不同模型目录），
		// 路由时按前缀分派 —— 于是「白名单放行」与「实际打到哪个上游」分叉：
		// 用户能在列表里看到 -intl 那条、也能调通，但那是他没授权的上游。
		//
		// ⇒ 规则：**白名单条目写了前缀就精确匹配前缀；没写前缀才按裸名放行。**
		//
		// 这条规则保住了原来的灵活性：用户填 `glm-5.2`（裸名）仍然匹配
		// 任意上游的 glm-5.2 —— 那是他明确表达的「哪个上游都行」。
		if !wantHas {
			// 请求是裸名（不指定上游）：白名单任何一条同名都算匹配。
			return true
		}
		if !gotHas {
			// 白名单条目是裸名：表示不限上游，同样放行。
			return true
		}
		if gotProv == wantProv {
			return true
		}
	}
	return false
}

// allowedModelsForRequest 取本次请求命中的 API key 的模型白名单。
//
// 返回 (models, restricted)：
//
//	restricted=false  不限制 —— 没有普通 key 命中（走管理钥匙），
//	                  或该 key 的白名单为空（见 apikey.Key.Models 的三态）
//	restricted=true   只允许 models 里的模型
//
// ⚠ 管理钥匙（config.api_key）**永远不受限**：它是运维/调试用的那把，
// 被一个白名单挡住会让"排障时连模型列表都拿不到"，而它本来就没有对应的
// Key 记录可挂白名单。
func (h *Handler) allowedModelsForRequest(r *http.Request) ([]string, bool) {
	if h.cfg.APIKeys == nil {
		return nil, false
	}
	// ⚠ nil request = 「问全量目录」，不是「某个调用方」。
	//
	// 管理台的一致性检查要拿「所有渠道能对外提供哪些模型名」，它与任何
	// 具体的 key 无关。调用方传 nil 表达这件事。若在这里 r.Context() 直接
	// 解引用会 panic —— 而那是从管理台点一下按钮就触发的崩溃。
	if r == nil {
		return nil, false
	}
	kid, _ := r.Context().Value(apikeyCtxKey).(string)
	if kid == "" {
		// 没命中普通 key ⇒ 是管理钥匙（或未启用鉴权）⇒ 不限制。
		return nil, false
	}

	// ── ① 分组优先（P2）：key.Groups 非空 ⇒ 允许范围 = 这些分组的模型并集 ──
	//
	// # 为什么分组排在白名单前面
	//
	// 白名单已废弃（PLAN-groups.md Q3：两者语义完全重复，必然不一致）。
	// 升级后的正确形态是「分组非空、白名单为空」；而**迁移之前**两种形态
	// 都可能出现（老 key 只有白名单，新 key 只有分组）。
	//
	// 顺序定成"分组优先"而不是"交集"，是因为交集要多维护一处配置 ——
	// 那正是 Q3 否决方案 A 的理由。分组非空时白名单**不参与判定**。
	groups, groupModels, ok := h.cfg.APIKeys.GroupModelsOf(kid)
	if !ok {
		// key 在鉴权之后被删掉了（并发窗口）。按"不限制"处理而不是拒绝 ——
		// 鉴权那一步刚放行过这次请求，这里再拒会表现成"偶发 403"，
		// 而那就成了一个没人能复现的故障。
		return nil, false
	}
	if len(groups) > 0 && h.cfg.APIKeys.HasGroupResolver() {
		// ⚠ HasGroupResolver 这一条不能省。装配层忘了注入解析函数时
		//   groupModels 恒为空 —— 若照直当成"分组里没有模型"，这把 key 会
		//   **一个模型都不许用**，而 403 只会说"不允许调用模型 X"，
		//   排查方向直接跑偏。所以未接线时降级到白名单路径（存量行为不变）。
		return groupModels, true
	}
	// groups 非空但解析函数未接线 ⇒ 落到下面的白名单分支。

	// ── ② 白名单兜底（存量兼容，P3.5 迁移完成后这条会自然走空）──
	models, ok := h.cfg.APIKeys.ModelsOf(kid)
	if !ok || len(models) == 0 {
		return nil, false
	}
	return models, true
}

// modelAllowedForRequest 判断本次请求是否被允许调用 model。
//
// 不允许时**自己写好 403** 并返回 false —— 调用方只需 `if !ok { return }`，
// 免得三处入口各写一遍错误体（写三遍必然写得不一样）。
//
// 错误体带上**允许的模型清单**（截断到前若干个）：调用方拿到 403 时最需要
// 知道的就是"那我到底能用什么"，否则他只能去问发 key 的人。
// 这不是信息泄漏 —— 这是他自己那把 key 的权限。
// modelAllowedForRequest 判断本次请求是否允许调用 model。
//
// 不允许时自己写好错误并返回 false —— 调用方只需 `if !ok { return }`，
// 免得三处入口各写一遍错误体。
//
// # model 参数应当是**客户端发来的名字**（别名或裸名），不是解析后的内部名
//
// 用户实测过一个复合 bug：传别名 006/x 时，resolveModelAlias 先把它换成
// 内部名 workbuddy/x，然后「拒绝上游真名」那条判据把内部名当成真名拒掉 ——
// 用别名的合法请求反而 403。
//
// 所以调用点传 publicModel；而白名单里存的是内部名，这里要**反查一次**。
func (h *Handler) modelAllowedForRequest(w http.ResponseWriter, r *http.Request, model string) bool {
	allowed, restricted := h.allowedModelsForRequest(r)
	if !restricted {
		return true
	}
	// ⚠ restricted 但 allowed 为空 = **一个模型都不许**，不是"不限制"。
	//
	// modelAllowed 把空列表读成"不限制"（空白名单的语义），那条语义对**白名单**
	// 是对的：白名单为空本来就是"没设限"。但分组路径能产出另一种"空" ——
	// 这把 key 指向的分组里一个模型都没有（分组被清空了、或者全被删了）。
	// 那不是"没设限"，那是"限到了空集"。照直走 modelAllowed 会让权限**被放大**
	// 成不限制，而界面上完全看不出异常（用户以为自己只是删了个分组）。
	//
	// 所以这里显式拦一道。它不复制任何分组判定 —— 只是把"受限且为空"这个
	// 状态说清楚，两个入口（准入 / 目录）各判一次。
	if len(allowed) == 0 {
		writeOpenAIError(w, http.StatusForbidden, "model_not_allowed",
			"这把 API key 被分配的分组里没有任何模型（分组可能已被删除或尚未加入模型）")
		return false
	}
	m := strings.TrimSpace(model)
	if m == "" {
		writeOpenAIError(w, http.StatusBadRequest, "missing_model",
			"请求体缺少 model 字段；可用模型请查 GET /v1/models")
		return false
	}
	// 泄露 2：别名 key 拒绝「上游真名」。
	//
	// 真名是全局可用的（不属于任何 key 的虚拟命名空间），别名 key 放行它
	// 等于给了绕过脱敏的路：客户端拿到真名就能直接调，响应 model 回显真名。
	//
	// ⚠ 2026-10-24 修：判据从「形如 001/xxx」改成「**在不在这张别名表里**」。
	//
	// 原来用 isAliasForm（要求数字前缀）当放行判据，但那条规则只对内置的
	// 001/002/… 成立。用户自定义别名可以是任意字符串（my-flash、kimi-x），
	// 于是只要配了自己的别名，**所有**请求都被这条判据拒掉 —— 包括合法的。
	//
	// isAliasForm 自己的注释早就写明「只用于出错时的提示文案，不用于放行」，
	// 是这里的用法写错了。现在改成查表：表里有 = 是别名，放行去换真名；
	// 表里没有 = 客户端发的是上游真名（或胡写的），拒。
	if h.keyUsesAliases(r) {
		mm := h.modelMapForRequest(r)
		_, isAlias := mm[m]
		if !isAlias {
			// 再试裸名：客户端可能发 `my-flash`、而表里存的是带前缀的键。
			if _, bare, _ := gateway.SplitModel(m); bare != "" && bare != m {
				_, isAlias = mm[bare]
			}
		}
		if !isAlias {
			writeOpenAIError(w, http.StatusForbidden, "model_not_allowed",
				fmt.Sprintf("这把 API key 只接受 GET /v1/models 里列出的名字；%q 不在其中", m))
			return false
		}
	}
	// 白名单里存的是内部名，客户端发的是别名 —— 先换回内部名再比。
	if resolved := h.resolveModelAlias(r, m); resolved != m {
		m = resolved
	}
	if modelAllowed(allowed, m) {
		return true
	}
	// ⚠ 错误体**不能**列出允许的模型名（用户实测的泄露 1）。
	//
	// 白名单里存的是内部名（workbuddy/...），列出来等于公布上游身份；
	// 而客户端拿到清单后可以直接用真名调用。只回数量，不回名字。
	//
	// 要看具体能调什么走 /v1/models —— 那里回的是别名（且已脱敏）。
	writeOpenAIError(w, http.StatusForbidden, "model_not_allowed",
		fmt.Sprintf("这把 API key 不允许调用模型 %q（本 key 授权了 %d 个模型，清单请查 GET /v1/models）", strings.TrimSpace(model), len(allowed)))
	return false
}

// filterModelsForRequest 把模型列表裁成该 key 允许的那些。
//
// 不限制时**原样返回**（同一个切片），不复制 —— 这条路径每个客户端都会走，
// 不必要地复制一份上百条的列表是白花钱。
//
// ⚠ 判据用的是与准入**同一个** apikey.ModelAllowed：两处若各写一套，
// 会出现"列表里看得见、调了却 403"这种最难解释的形态。
//
// # P3：分组不需要在这里单独判
//
// 分组对目录的收窄**已经生效**，但不在这段代码里 —— 它在 allowedModelsForRequest
// 的返回值上：分组非空时那里返回的就是「这些分组的模型并集」，本函数拿到的
// 就是那个并集。
//
// 所以我**没有**在这里再加一段 groups 判断。理由就是本仓反复踩过的那条：
// 同一判据两处实现必然分叉。这里若再查一次分组，就会出现
//
//	准入（modelAllowedForRequest）看分组并集
//	目录（本函数）看分组并集 ∩ 白名单
//
// 而两者不一致的表现是"列表里没有、调用却成功"（或反过来），
// 且只在"分组与白名单同时非空"的 key 上出现 —— 那正是 P3.5 迁移前
// 短暂存在的形态，测试最难覆盖、用户最难报障。
//
// # 一个必须成立的语义：分组里的模型目录里有才出现
//
// 分组是「**允许范围**」，不是「保证可用」（PLAN 3.4）。本函数遍历的是
// **真实目录**（上游 + 渠道），分组只负责从中筛，因此：
//
//	分组里有 q、但没有任何渠道/上游提供 q ⇒ 列表里没有 q
//
// 这是对的：列出来的东西必须真能调通。反过来若按分组清单直接生成列表，
// 客户端会看到一堆调了必 404 的模型名。
func (h *Handler) filterModelsForRequest(r *http.Request, list []map[string]any) []map[string]any {
	allowed, restricted := h.allowedModelsForRequest(r)
	if !restricted {
		return list
	}
	// 受限且为空 ⇒ 一个都不给（理由同 modelAllowedForRequest 里那段）。
	// 返回空切片而不是原列表：把列表原样发出去等于告诉客户端"这些你都能调"，
	// 而实际每个都会 403 —— 那正是"列表里看得见、调了却 403"的来源。
	if len(allowed) == 0 {
		return []map[string]any{}
	}
	tbl := h.aliasTableForRequest(r)
	out := make([]map[string]any, 0, len(list))
	// 已输出的对外名（去重用）。两个内部模型映射到同一个对外名是配置错误，
	// 但**不能**让它在列表里出现两次 —— 客户端建模型索引时会把第二条当重复项丢掉，
	// 而丢掉哪一条取决于它的实现，我们没法预测。
	seenPub := make(map[string]bool, len(list))
	for _, m := range list {
		id, _ := m["id"].(string)
		if !modelAllowed(allowed, id) {
			continue
		}
		// ⚠ 2026-10-21：输出**对外名**，不是内部名。
		//   这里必须与请求侧反向：调用方在列表里看到什么名字，就用什么名字发请求，
		//   而请求进来会被 resolveModelAlias 换回内部名。两边不一致的话，
		//   用户复制列表里的名字去调用会得到一个"未知模型"。
		pub := publicNameForMulti(tbl, id)
		if pub == "" || seenPub[pub] {
			continue
		}
		seenPub[pub] = true
		if pub == id {
			out = append(out, m)
			continue
		}
		// 要改名：复制一份，不能就地改 m —— list 可能是别处共享的切片，
		// 改了会让"另一个 key 看到的列表"也被改名（越权泄漏别名的形态）。
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		cp["id"] = pub
		out = append(out, cp)
	}
	return out
}

// applyPublicNames 把列表里**仍是内部名**的条目的 id 换成对外名。
//
// # 为什么需要第二遍（2026-10-21 修，用户报「设置了对外名称 csg 还是原来的名字」）
//
// `/v1/models` 的组装顺序是：
//
//	h.filterModelsForRequest(r, h.modelList())   ← 上游目录，这里已经改过名
//	data = h.appendChannelModels(r, data)        ← ⚠ 追加渠道声明的**原模型名**
//
// 而渠道声明的名字（如 `csg/deepseek-v4.1-flash-1017`）**不在上游目录里**
// （csg 是渠道，不是 gateway 的 provider），所以它绕过了第一遍的改名，
// 以内部名直接出现在列表里。
//
// 实测证据（临时日志）：
//
//	（只有这一条 —— csg 那条压根没进过滤循环）
//
// ⇒ 所以改名要做两遍，或者干脆把它抽出来在**最后**做一次。
//
//	选后者：先由前两步决定"列表里有哪些模型"，最后统一改名。
//
// ⚠ 幂等：对已经是对外名的条目，publicNameFor 找不到匹配会原样返回。
//
//	（`001/DeepSeek-V4.1-Flash` 的裸名与 `loomy/DeepSeek-V4.1-Flash` 相同，
//	 所以可能"再命中一次"得到同一个值 —— 那也是幂等的。）
//
// ⚠ 没有映射表时直接返回原切片，不复制：这条路径每个客户端都会走。
func (h *Handler) applyPublicNames(r *http.Request, list []map[string]any) []map[string]any {
	// ⚠ 用一对多原表：单值表会漏掉同一对外名下的其余内部名 ⇒ 上游真名泄露。
	tbl := h.aliasTableForRequest(r)
	if len(tbl) == 0 {
		return list
	}
	seen := make(map[string]bool, len(list))
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		id, _ := m["id"].(string)
		pub := publicNameForMulti(tbl, id)
		if pub == "" || seen[pub] {
			continue
		}
		seen[pub] = true
		if pub == id {
			out = append(out, m)
			continue
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		cp["id"] = pub
		out = append(out, cp)
	}
	return out
}

// joinLimited 用中文顿号连接，超过 n 个就加"…（共 N 个）"。
//
// 为什么限个数：白名单可能有几十项，整串塞进错误体后前端弹窗会变成一堵墙，
// 反而看不到最关键的那句"不许调用"。
func joinLimited(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, "、")
	}
	return strings.Join(list[:n], "、") + "…（共 " + strconv.Itoa(len(list)) + " 个）"
}

// bumpKeyUsage 把一次请求的用量记给命中的 API key（**渠道分支**专用）。
//
// # 为什么需要它（2026-10-21 修，用户报「请求/失败一直是 0」）
//
// 账号池路径在 `handler.go` 的 `st.bump` 回调里记（chatStat.done 统一出口）。
// 但**渠道分支在那之前就 return 了**：
//
//	L1506  if h.channelServes(peek.Model) { ... return }   ← 渠道请求在这里结束
//	L1632  st := newChatStat(...)                          ← 用量记录从这里才开始
//	L1652  st.bump = ...
//
// ⇒ 走渠道的请求（如 `csg/...`）**一条都不会被记**，界面上「请求」「失败」恒为 0。
//
// 实测佐证：磁盘上 `requests=0 fails=0 used=0`，而同一把 key 当天发过大量 csg 请求。
//
// # 为什么不做成"把渠道分支也套进 chatStat"
//
// 渠道分支有自己的收尾（logChatRowProvider + writeChannelStream），
// 硬套 chatStat 要改两处出口的结构，风险大于收益。
// 这里只补**缺的那一件事**（记 key 用量），日志仍走它原来的路。
//
// ⚠ toks <= 0 时不累加 Used（BumpUsage 内部有 `if tokens > 0`）——
//
//	渠道失败时没有 token 数，传 0；成功但 usage 拿不到时传 -1（未知哨兵），
//	两者都不该污染已用量。
func (h *Handler) bumpKeyUsage(r *http.Request, toks int, ok bool) {
	if h.cfg.APIKeys == nil {
		return
	}
	kid, _ := r.Context().Value(apikeyCtxKey).(string)
	if kid == "" {
		return
	}
	h.cfg.APIKeys.BumpUsage(kid, int64(toks), ok)
}

// ──────────────────────────────────────────────────────────────────────────
// 模型对外名映射（2026-10-21）
// ──────────────────────────────────────────────────────────────────────────

// 用户的需求（原话）：
//
//	「这些模型，开放的模型，对于这个 API key 开放的模型，我觉得需要有一个
//	 模型映射，就是对外的模型应该叫什么」
//
// # 为什么这是 key 维度而不是渠道维度
//
// 渠道层已经有 `Channel.ModelMap`（公开名 → 上游名），它解决的是
// 「同一个上游模型，在这个渠道里叫什么」。而这里解决的是
// 「同一把 key 的调用方，看到的是哪套名字」—— 两件事正交：
//
//	渠道层负责「怎么打到上游」，key 层负责「对外怎么称呼」
//
// 换一把 key 可以换一套对外名，而渠道配置不用动。这才是中转站该有的形态：
// 发给 A 客户的一套名字、发给 B 客户的另一套，后端是同一批模型。
//
// # 方向：对外名 → 内部名
//
// 请求路径每一跳都要查它（每次对话请求 + /v1/models），方向必须是
// "进来的名字 → 内部的名字"。反过来存就得遍历全表找 value。
// 与 `Channel.ModelMap` 同向，读代码时不用在两套约定之间来回切。

// aliasTableForRequest 取本次请求生效的「对外名 → 内部名列表」表。
//
// # 2026-10-24：来源从 API Key 改为**分组**，且支持一对多
//
// 用户要求：「对外模型不要在这里设置 在分组里面设置」，随后又要求
// 「希望可以重复别名……它不是有几个上游吗？那就轮询就行了」。
//
// 别名的语义是「这个模型对外叫什么」，而「哪些模型」也是分组定义的 ——
// 两件事本来就绑在一起。放在 key 上有两个问题：
//
//	① 同一个模型被两把 key 引用时要配两遍，改一个漏一个
//	② key 上配了别名但模型不在任何分组里 ⇒ 那个别名永远不生效
//	   （判定链先过分组，根本不认识这个名字），而界面上看不出矛盾
//
// 多分组时同名对外名的列表**按分组 ID 字典序拼接**。
//
// 为什么是字典序而不是「谁的模型多用谁的」：后者会让结果随分组内容变化而跳变
// （改一个分组的模型，另一处的别名突然换了指向）。字典序是一个与内容无关的
// 稳定判据 —— 同样的两组分组，永远得到同样的列表顺序。
func (h *Handler) aliasTableForRequest(r *http.Request) map[string][]string {
	if h.cfg.APIKeys == nil || h.cfg.Groups == nil {
		return nil
	}
	if r == nil {
		return nil
	}
	kid, _ := r.Context().Value(apikeyCtxKey).(string)
	if kid == "" {
		return nil
	}
	groups, _ := h.cfg.APIKeys.GroupsOf(kid)
	if len(groups) == 0 {
		return nil
	}
	ids := append([]string(nil), groups...)
	sort.Strings(ids)
	var out map[string][]string
	seen := map[string]map[string]bool{} // 对外名 → 已收的内部名集合（跨分组去重）
	for _, gid := range ids {
		g, ok := h.cfg.Groups.Get(gid)
		if !ok || g == nil || len(g.ModelMap) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string][]string, 8)
		}
		for pub, internals := range g.ModelMap {
			if seen[pub] == nil {
				seen[pub] = map[string]bool{}
			}
			for _, in := range internals {
				in = strings.TrimSpace(in)
				if in == "" || seen[pub][in] {
					continue
				}
				seen[pub][in] = true
				out[pub] = append(out[pub], in)
			}
		}
	}
	return out
}

// modelMapForRequest 取「对外名 → 首个内部名」的单值表。
//
// ⚠ 这个函数**只保留给历史调用方**（少数地方只需要一个代表值）。
//
//	目录脱敏必须用 publicNameForMulti —— 一对多时单值表会漏掉其余内部名，
//	后果是上游真名泄露到 /v1/models（2026-10-24 端到端实测踩到过）。
func (h *Handler) modelMapForRequest(r *http.Request) map[string]string {
	tbl := h.aliasTableForRequest(r)
	if len(tbl) == 0 {
		return nil
	}
	out := make(map[string]string, len(tbl))
	for pub, internals := range tbl {
		if len(internals) > 0 {
			out[pub] = internals[0]
		}
	}
	return out
}

// publicNameForMulti 是一对多版的 publicNameFor：内部名 → 对外名。
//
// # 为什么单独写一个而不是把 modelMapForRequest 展开
//
// publicNameFor 的第 ① 步判「name 自己是不是某个对外名」用来做幂等。
// 若把内部名也塞进那张表的**键**里，① 会先命中内部名并原样返回 ——
// 脱敏就失效了（实测：/v1/models 里 3 个上游真名全露出来）。
//
// 这里用一对多原表直接查，语义清晰：
//
//	· 键 = 对外名，值 = 内部名列表
//	· 反查时遍历值，命中即返回对应键
func publicNameForMulti(tbl map[string][]string, internal string) string {
	name := strings.TrimSpace(internal)
	if name == "" || len(tbl) == 0 {
		return internal
	}
	// ① 幂等：name 自己就是某个对外名 ⇒ 原样返回。
	if _, isAlias := tbl[name]; isAlias {
		return name
	}
	// ② 精确反查：某个对外名的列表里包含 name。
	for pub, internals := range tbl {
		for _, in := range internals {
			if strings.TrimSpace(in) == name {
				return pub
			}
		}
	}
	// ③ 裸名兜底：**唯一命中**才用（多个对外名共享同一裸名时不猜，
	//   否则列表里的名字会随 map 遍历顺序漂移）。
	if _, bare, _ := gateway.SplitModel(name); bare != "" {
		hit, n := "", 0
		for pub, internals := range tbl {
			for _, in := range internals {
				if _, ib, _ := gateway.SplitModel(strings.TrimSpace(in)); ib != "" && ib == bare {
					hit = pub
					n++
					break
				}
			}
			if n > 1 {
				break
			}
		}
		if n == 1 {
			return hit
		}
	}
	return internal
}

// aliasCandidates 把客户端发来的名字解析成**候选内部名列表**（可能多个）。
//
// 返回值语义：
//   - 命中别名 ⇒ 该别名的全部内部名（按配置顺序，供轮询）
//   - 未命中   ⇒ 原样返回单元素列表（没有别名就走原名，行为与改造前一致）
//
// ⚠ 裸名兜底只在**唯一命中**时使用：客户端可能发 deepseek-v4.1-flash，
//
//	而映射表里的键是 csg/deepseek-v4.1-flash。多个别名共享同一裸名时不猜，
//	否则同一个请求两次可能走不同上游 —— 那对排障是灾难。
func (h *Handler) aliasCandidates(r *http.Request, model string) []string {
	tbl := h.aliasTableForRequest(r)
	name := strings.TrimSpace(model)
	if name == "" {
		return []string{model}
	}
	if len(tbl) == 0 {
		return []string{name}
	}
	if hit, ok := tbl[name]; ok && len(hit) > 0 {
		return hit
	}
	// 裸名兜底：唯一命中才用。
	if _, bare, _ := gateway.SplitModel(name); bare != "" && bare != name {
		if hit, ok := tbl[bare]; ok && len(hit) > 0 {
			return hit
		}
	}
	return []string{name}
}

// aliasAttempts 返回本轮要依次尝试的内部名。
//
// aliasOrder 由 pickAliasTarget 给出（已按轮询起点排好顺序）；
// 为空时退化成"只试当前这一个"，行为与改造前一致。
//
// ⚠ 去重：同一个内部名可能在多个分组里重复出现，重复尝试只会把
//
//	同一个上游的同一个错误再打一遍，白等一轮超时。
func aliasAttempts(order []string, current string) []string {
	if len(order) == 0 {
		return []string{current}
	}
	seen := make(map[string]bool, len(order))
	out := make([]string, 0, len(order))
	for _, m := range order {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return []string{current}
	}
	return out
}

// aliasRR 轮询游标：对外名 → 上次选到的下标。
//
// # 为什么用轮询而不是随机
//
// 用户原话：「它不是有几个上游吗？那就轮询就行了」。
//
// 轮询是**可预测**的：N 个上游就严格按 1,2,3,...,N,1,2,... 分发。
// 随机在高并发下会出现「某个上游 5 分钟内一次没轮到」，排查时看到的现象是
// 「额度没变但就是不走那条」，而轮询不会。均匀性也更直观。
//
// ⚠ 全局共享一个游标（按对外名分桶），不是每请求独立：
//
//	每请求独立就等于"每次都从第一个开始"，那不成轮询。
var aliasRR struct {
	mu   sync.Mutex
	next map[string]uint64
}

// pickAliasTarget 从候选里按**轮询**挑一个，返回 (内部名, 本轮尝试顺序)。
//
// # 只轮询「有健康账号」的候选（2026-10-24 加）
//
// 用户原话：
//
//	「右边有三个我设置一样的 所以请求右边一样的模型需要有三个上游对应
//	  上游有可用健康模型才进去发请求」
//
// 也就是说：一个对外名背后有 3 个上游，但只有 2 个当前有健康账号 ——
// 那就**只在这 2 个之间轮询**，第 3 个连试都不试。
//
// 这解决了实测踩到的浪费：workbuddy 上游 6 个号全被 429 冷却时，
// 网关仍要花 1.2-2.5 秒（逐个号打上去、每个吃一次 429、试满预算才放弃），
// 而同一时刻 workbuddy-intl / loomy 是健康的。
//
// # 判定用「有没有健康账号」，不是「上游是否熔断」
//
// 熔断是"连续失败推断出来的"，滞后；而 AvailableUIDsFor 是**当下**的事实：
// 这个上游现在有没有一个能用的号。用它做筛选，判据更直接。
//
// ⚠ 渠道（channel）不是账号池，不走这个判据 —— 它的可用性由渠道自己的
//
//	key 状态决定（channelroute.Plan 会过滤掉没有可用 key 的渠道）。
//	所以这里对"渠道型候选"一律保留，不误杀。
//
// ⚠ 全部候选都没有健康账号时**保留原顺序**（不清空）：
//
//	那样调用方会走完整的失败路径并给出准确错误（"6 个账号全部冷却"比
//	"没有候选"更有信息量）。宁可慢一点，也不要丢掉排障证据。
func (h *Handler) pickAliasTarget(public string, cands []string) (string, []string) {
	n := len(cands)
	if n == 0 {
		return "", nil
	}
	if n == 1 {
		return cands[0], cands
	}
	// 只保留"有健康账号"的候选。
	healthy := h.healthyAliasCandidates(cands)
	pool := cands
	if len(healthy) > 0 {
		pool = healthy
	}
	// 诊断：候选筛选结果（排障时能直接看出"为什么选了没号的那家"）。
	if len(healthy) != len(cands) {
		log.Printf("[apikey] 别名 %q 候选 %d 个，其中有健康账号的 %d 个: %v",
			public, len(cands), len(healthy), healthy)
	}
	m := len(pool)
	aliasRR.mu.Lock()
	if aliasRR.next == nil {
		aliasRR.next = make(map[string]uint64, 8)
	}
	idx := int(aliasRR.next[public] % uint64(m))
	aliasRR.next[public]++
	aliasRR.mu.Unlock()

	order := make([]string, 0, m)
	for i := 0; i < m; i++ {
		order = append(order, pool[(idx+i)%m])
	}
	return order[0], order
}

// nextAliasUpstream 按别名候选顺序找下一个"有健康账号且没试过"的上游。
//
// 返回 (上游ID, 该上游的模型名)；找不到返回 ("", "")。
//
// # 与 alternateUpstream 的区别（关键）
//
// alternateUpstream 用 normModelKey 猜"同一个模型"：去前缀 → 小写 → 只留字母数字。
// 实测它的误判率不可接受 —— 目录里 4 组模型被它判成同一个：
//
//	nvidia/nemotron-3.5-lightning-free  vs  nvidia/nemotron-3.5-lightning:free
//	qwen3.8-max                          vs  qwen-3.8-max
//
// 那是**两个不同的模型**，换过去等于偷偷换了用户要的东西（正是 2026-10-18
// 用户否决换上游时的原话：「我调用的是这个模型，但是它优选，选到别的模型去了」）。
//
// 本函数不猜：只走 aliasOrder —— 那是用户在分组里**显式**配的对应关系
// （"这三个内部模型对外都叫 X"），语义确定。
func (h *Handler) nextAliasUpstream(order []string, tried map[string]bool) (string, string) {
	if h.cfg.Pool == nil || h.cfg.Provider == nil {
		return "", ""
	}
	for _, m := range order {
		prov, _, has := gateway.SplitModel(strings.TrimSpace(m))
		if !has || prov == "" {
			continue
		}
		if tried != nil && tried[prov] {
			continue
		}
		if !h.cfg.Provider.Has(prov) {
			continue // 渠道前缀：不归账号池管
		}
		if len(h.cfg.Pool.AvailableUIDsFor(prov)) == 0 {
			continue // 这家没号了，跳过
		}
		return prov, m
	}
	return "", ""
}

// healthyAliasCandidates 筛出「当前有健康账号」的候选（保持原顺序）。
//
// 判定口径与 pickForTier 一致：healthyForModel 用的就是 e.healthy + 在途未满，
// 而 AvailableUIDsFor 正是那个判据的批量版（见它的实现）。
//
// ⚠ 渠道型候选（走 channelstore 的那条路）不参与筛选：
//
//	它们不在账号池里，AvailableUIDsFor 会返回空 —— 若照此过滤会把
//	所有渠道候选误杀。判据用「这个候选是不是池里的上游」来区分。
func (h *Handler) healthyAliasCandidates(cands []string) []string {
	if h.cfg.Pool == nil || h.cfg.Provider == nil {
		return nil
	}
	out := make([]string, 0, len(cands))
	for _, m := range cands {
		prov, _, hasPrefix := gateway.SplitModel(strings.TrimSpace(m))
		if !hasPrefix || prov == "" {
			continue // 裸名 / 渠道：不参与筛选
		}
		if !h.cfg.Provider.Has(prov) {
			continue // 不是注册过的上游（可能是渠道前缀）：不参与筛选
		}
		if len(h.cfg.Pool.AvailableUIDsFor(prov)) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// resetAliasRoundRobin 清空游标（测试用；也让配置变更后能重新开始）。
func resetAliasRoundRobin() {
	aliasRR.mu.Lock()
	aliasRR.next = nil
	aliasRR.mu.Unlock()
}

// resolveModelAlias 取**第一个**候选内部名。
//
// ⚠ 这个函数保留给「只需要一个答案」的调用方（目录展示、白名单比较）。
//
//	真正要发请求的地方必须用 pickAliasTarget —— 那里才做轮询。
func (h *Handler) resolveModelAlias(r *http.Request, model string) string {
	c := h.aliasCandidates(r, model)
	if len(c) == 0 {
		return model
	}
	return c[0]
}

// publicNameFor 把一个**内部**模型名换成它对外的名字（/v1/models 用）。
//
// 没有反向映射 ⇒ 返回原值（内部名就当对外名用，与"不配映射"的行为一致）。
//
// ⚠ 这里必须遍历 value 找键 —— 存储方向是为请求路径优化的（对外名 → 内部名），
//
//	而 /v1/models 是低频管理路径（每客户端首次连接一次），遍历成本可忽略。
//	两张表各存一份反而会引入"改了一边忘改另一边"的不一致。
func publicNameFor(modelMap map[string]string, internal string) string {
	if len(modelMap) == 0 {
		return internal
	}
	name := strings.TrimSpace(internal)
	if name == "" {
		return internal
	}
	// ① 幂等：name 自己就是某个**对外名** ⇒ 原样返回，不要再改名。
	//
	// ⚠ 2026-10-21 用户报「配了 006/deepseek-v4.1-flash，列表里却没有」的真根因就在这里。
	// /v1/models 的组装是 filterModelsForRequest（**已改名**）→ appendChannelModels
	// → applyPublicNames（**又改名一遍**）。第二遍拿到的 id 是 006/…，精确匹配不命中
	// （map 的键是 alias、值是内部名），于是掉进下面的裸名兜底：004/005/006 三个
	// 对外名的 target 裸名全是 deepseek-v4.1-flash ⇒ 随机挑中 004 ⇒ 006 被改名成
	// 004，随后 seen[pub] 去重把它整条丢掉。用户看到的就是"配了三条，列表只剩两条"。
	if _, isAlias := modelMap[name]; isAlias {
		return name
	}
	// ② 精确命中（值是内部名）
	for alias, target := range modelMap {
		if strings.TrimSpace(target) == name {
			return alias
		}
	}
	// ③ 裸名兜底（映射表里可能写的是带前缀的内部名）——**只在唯一命中时**使用。
	//   多个别名共享同一裸名时（同一模型挂在多个上游）不猜：返回原值，
	//   否则列表里的名字会随 map 遍历顺序漂移，同一个 key 两次请求看到不同的名字。
	if _, bare, _ := gateway.SplitModel(name); bare != "" {
		hit, n := "", 0
		for alias, target := range modelMap {
			if _, tb, _ := gateway.SplitModel(strings.TrimSpace(target)); tb != "" && tb == bare {
				hit = alias
				n++
				if n > 1 {
					break
				}
			}
		}
		if n == 1 {
			return hit
		}
	}
	return internal
}

// keyUsesAliases 报告本次请求的 key 是不是「用别名对外」的那种。
//
// 判据：该 key 配了 model_map（有别名映射）。
//
// 别名 key 的白名单里存的是内部名（workbuddy/deepseek-v4.1-flash），
// 而内部名 / 上游真名是全局可用的 —— 不属于任何 key 的虚拟命名空间。
// 于是客户端拿到真名就能绕过别名直接调，响应 model 回显真名，
// 让 chatcmpl-gw-* 改写与 model 别名回填那套脱敏完全失效。
//
// 没配别名的 key 不拦：那种 key 本来就不脱敏
// （用户说「对话密钥按原来的上游即可」），对它拦真名是过度限制。
func (h *Handler) keyUsesAliases(r *http.Request) bool {
	return len(h.modelMapForRequest(r)) > 0
}

// isAliasForm 报告 model 是否是「别名形式」（形如 008/xxx 的数字前缀）。
//
// 本项目里别名统一用三位数字前缀（001/ 002/ ...），上游真名的前缀是字母。
// 这条判据只用于出错时的提示文案，不用于放行 —— 放行仍由白名单决定。
func isAliasForm(model string) bool {
	prov, _, has := gateway.SplitModel(strings.TrimSpace(model))
	if !has || prov == "" {
		return false
	}
	for i := 0; i < len(prov); i++ {
		if prov[i] < '0' || prov[i] > '9' {
			return false
		}
	}
	return true
}
