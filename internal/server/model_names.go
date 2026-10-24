// model_names.go 对外暴露「当前模型目录」（内部名），供管理台做一致性检查。
//
// # 为什么需要它
//
// 分组里存的模型名是**内部名**（如 csg/deepseek-v4.1-flash-1017）。上游下线某个
// 模型之后，那个名字就「陈旧」了 —— 留在分组里既不报错也不生效，用户看不见。
// 一致性检查要把这类条目列出来，所以需要一个「当前目录有哪些名字」的来源。
//
// # 为什么用缓存而不是实时拉
//
// 调用方是管理台的 /admin/groups/consistency，**页面会轮询它**。若每次都打上游：
//
//	· 每次都消耗一个账号的配额
//	· 上游抖动时检查端点跟着挂，界面上表现为「检查失败」而不是「有 N 条陈旧」
//
// # 两次踩过的坑（都记录在这里，别再犯）
//
//	① 只读 dynamicModelsCache（那只是**默认上游**的目录）⇒ 把 csg / loomy /
//	   qiniuyun / senseaudio 这些**渠道与其它上游**的模型全报成陈旧。
//	   目录必须由与 /v1/models 相同的聚合逻辑得出。
//
//	② 改成聚合之后，又要求「所有上游缓存都就绪」，否则整个结果作废。
//	   实测永远是 catalog_size=0 —— 因为总有上游（比如没配账号的）缓存是空的，
//	   于是一个永远不可用的功能。
//
// 现在的口径：**能拿到多少就说多少**，但把「哪些来源没查」如实回报。
// 调用方据此显示「部分来源未就绪」，而不是把未查过的当成「不存在」。
package server

// ModelNameSnapshot 模型目录的一份快照，附带「哪些来源没查成」。
type ModelNameSnapshot struct {
	// Names 已确认存在的模型名（内部名）。
	Names []string
	// Unavailable 未能检查的来源（上游 id 或 "channels"）。
	//
	// ⚠ 非空只说明「这份快照不完整」，**不能**据此判定某个模型已下线 ——
	// 那个模型可能恰好来自没查成的那个来源。
	Unavailable []string

	// Entries 模型名 + 它的上游（owner）。
	//
	// # 为什么除了 Names 还要这个
	//
	// 前端要「先选上游，再看模型」。绝大多数情况下模型名自带前缀
	// （"cline/xxx"），切第一个斜杠就得到上游；但**不能赌**这一点 ——
	// 单上游部署、或上游 id 为空时模型名没有前缀，那时只能靠 owner。
	// 这与 webui.html 的 modelOwnerOf(id, ownedBy) 同一口径：
	// 有前缀用前缀，没有才回退 owned_by。
	Entries []ModelNameEntry
}

// ModelNameEntry 目录里的一条：模型名 + 它来自哪个上游。
type ModelNameEntry struct {
	ID    string
	Owner string
}

// CachedModelNames 返回当前缓存里的模型目录（内部名）。
//
// 只读缓存，从不主动刷新**其它上游**（它们的拉取由请求路径负责）。
// 默认上游那条路径用 fetchDynamicModels，它自带 TTL 与失败负缓存。
//
// 返回 nil 表示**一个来源都没查到**（目录完全不可用）。
func (h *Handler) CachedModelNames() []string {
	return h.CachedModelNamesSnapshot().Names
}

// CachedModelNamesSnapshot 同 CachedModelNames，但附带未查成的来源。
func (h *Handler) CachedModelNamesSnapshot() ModelNameSnapshot {
	var snap ModelNameSnapshot
	if h.cfg.Upstream == nil {
		snap.Unavailable = append(snap.Unavailable, "upstream")
		return snap
	}
	def := h.defaultProvider()

	seen := make(map[string]bool, 1024)
	add := func(name, owner string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		snap.Names = append(snap.Names, name)
		snap.Entries = append(snap.Entries, ModelNameEntry{ID: name, Owner: owner})
	}

	// ① 默认上游（含对话模型）。与 /v1/models 一致：有前缀就带前缀。
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		for _, mi := range infos {
			if def == "" {
				add(mi.ID, def)
				continue
			}
			add(def+"/"+mi.ID, def)
		}
	} else {
		snap.Unavailable = append(snap.Unavailable, def)
	}

	// ② 其余上游：只读缓存。未预热的记进 Unavailable，不因此作废整份快照。
	for _, id := range h.otherProviders(def) {
		cached, ok := peekProviderModelsCached(id)
		if !ok {
			snap.Unavailable = append(snap.Unavailable, id)
			continue
		}
		for _, mi := range cached {
			add(id+"/"+mi.ID, id)
		}
	}

	// ③ 渠道：**必须走 channelPublicNames**，不能直接读 ch.Models。
	//
	// 为什么（这个 bug 我犯过一次）：渠道声明的名字是**上游原名**
	// （如 qiniuyun 的 deepseek-v4.1-flash），而分组里存的是**对外名**
	// （qiniuyun/deepseek-v4.1-flash 或某个别名）。直接拿 ch.Models 会比不上，
	// 于是渠道模型全被误报成「陈旧」。
	//
	// channelPublicNames 做的正是这件事：能力索引 + 反向映射，得到对外名。
	// 它需要 *http.Request 只为取"这把 key 的白名单"；这里传 nil，
	// allowedModelsForRequest 对 nil request 会安全返回不受限（见它的实现）。
	if h.cfg.Channels != nil {
		names, _ := h.channelPublicNames(nil)
		// 渠道模型：owner 用渠道名（它是「这类模型由谁提供」的答案）。
		for _, n := range names {
			add(n, "")
		}
	}

	return snap
}
