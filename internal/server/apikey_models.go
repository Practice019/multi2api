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
	"sort"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
	modelMap := h.modelMapForRequest(r)
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
		pub := publicNameFor(modelMap, id)
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
	modelMap := h.modelMapForRequest(r)
	if len(modelMap) == 0 {
		return list
	}
	seen := make(map[string]bool, len(list))
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		id, _ := m["id"].(string)
		pub := publicNameFor(modelMap, id)
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

// modelMapForRequest 取本次请求命中的 API key 的映射表。
//
// 返回 nil 表示"不做映射"，包括三种情况（都对）：
//   - 没命中普通 key（走管理钥匙）—— 管理钥匙永远不做别名替换
//   - key 存在但没有配映射
//   - APIKeys 存储未启用
// modelMapForRequest 取本次请求生效的「对外名 → 内部名」映射。
//
// # 2026-10-24：来源从 API Key 改为**分组**
//
// 用户要求：「对外模型不要在这里设置 在分组里面设置」。
//
// 别名的语义是"这个模型对外叫什么"，而**哪些模型**也是分组定义的 ——
// 两件事本来就绑在一起。放在 key 上有两个问题：
//
//	① 同一个模型被两把 key 引用时要配两遍，改一个漏一个
//	② key 上配了别名但模型不在任何分组里 ⇒ 那个别名永远不生效
//	   （判定链先过分组，根本不认识这个名字），而界面上看不出矛盾
//
// 所以现在把该 key 所有分组的 ModelMap **合并**起来。多分组时若同一个
// 对外名指向不同内部名，按分组 ID 字典序先到先得（见 mergeGroupMaps 注释）。
func (h *Handler) modelMapForRequest(r *http.Request) map[string]string {
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
	return h.mergeGroupMaps(groups)
}

// mergeGroupMaps 把多个分组的别名表合并成一张。
//
// 冲突处理：**按分组 ID 字典序先到先得**。
//
// 为什么是字典序而不是"谁的模型多用谁的"：后者会让结果随分组内容变化而跳变
// （改一个分组的模型，另一处的别名突然换了指向）。字典序是一个与内容无关的
// 稳定判据 —— 同样的两组分组，永远得到同样的结果。
func (h *Handler) mergeGroupMaps(groupIDs []string) map[string]string {
	ids := append([]string(nil), groupIDs...)
	sort.Strings(ids)
	var out map[string]string
	for _, gid := range ids {
		g, ok := h.cfg.Groups.Get(gid)
		if !ok || g == nil || len(g.ModelMap) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]string, 8)
		}
		for pub, internal := range g.ModelMap {
			if _, exists := out[pub]; !exists {
				out[pub] = internal
			}
		}
	}
	return out
}

// resolveModelAlias 把客户端发来的**对外名**换成**内部模型名**。
//
// 没有命中映射 ⇒ **原样返回**。这条很重要：映射是增量能力，不是白名单。
// 若把未命中的名字判成非法，那配一张映射表就等于把所有老调用方式打死 ——
// 而老调用方看到的会是"以前能用的模型突然 404 了"。
//
// ⚠ 必须在**判白名单之前**调用：白名单里存的是内部名，拿对外名去比会 403，
// 而那个对外名恰恰是我们发布出去让人用的名字。
//
// ⚠ 返回替换后的名字，但**不**改写 body —— body 交给下游重写：
// 渠道分支由 `forwardViaChannel` 用（model 参数 + body）重新写 model 字段，
// legacy 分支同理。只改 peek.Model 一处，所有消费方自动一致。
func (h *Handler) resolveModelAlias(r *http.Request, model string) string {
	m := h.modelMapForRequest(r)
	if len(m) == 0 {
		return model
	}
	name := strings.TrimSpace(model)
	if name == "" {
		return model
	}
	if target, ok := m[name]; ok && strings.TrimSpace(target) != "" {
		return strings.TrimSpace(target)
	}
	// ⚠ 再试一次裸名（剥掉 provider/ 前缀）：客户端可能发 `deepseek-v4-pro`，
	//   而映射表里存的是 `csg/deepseek-v4-pro` 这种带前缀的键。
	//   两处都试，才能让"用户填了带前缀的名字"也生效 —— 与白名单的裸名比较同一考虑。
	if _, bare, _ := gateway.SplitModel(name); bare != "" && bare != name {
		if target, ok := m[bare]; ok && strings.TrimSpace(target) != "" {
			return strings.TrimSpace(target)
		}
	}
	return model
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
