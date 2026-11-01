package server

import "workbuddy2api/internal/prompt"

// armorBody 核心**出站边界**的破甲注入。
//
// # 为什么是"核心边界"，而不是"包一层 Provider"（原设计就是这么提的，实测行不通）
//
// 包一层 Provider 看似最省事：包完所有上游自动生效。但本仓取可选扩展点用的是
//
//	gateway.ExtOf[T](p)   →   t, ok := p.(T)      // 裸类型断言，**没有解包链**
//
// 于是包一层就会把 p 身上**全部**可选接口藏起来（实测数了 16 个：AdminExt、JobExt、
// ChatToolExt、CandidateTierExt、CredentialTokenExt、CredentialRefresher、CredentialExpiryExt、
// AccountColumnsExt、AccountImportExt、EgressExt、EgressCheckExt、DailyActionExt、
// DailyCheckinExt、ErrorClassifier、LoginFlow、AuthDirExt…）。
//
// 后果不是"少个功能"，而是静默塌掉半块产品：管理端点挂不上、JobExt 的签到/保活/成长/
// 旅行守卫轮全部消失、凭证不会自动续期、出口自检没了 —— 而且**测试会全绿**
//（没人断言"装饰器实现了所有扩展点"）。
//
// 所以改用**边界注入**：核心把 body 交给任何 Provider 之前统一过一遍。
// 这也正好满足用户那句「只要是请求都要破甲」—— 覆盖面取决于"谁在发请求"（核心只有三处），
// 而不是取决于"哪个上游记得接"。
//
// # 三个调用点（由 TestAllProviderChatsAreArmored 源码级守卫钉住）
//
//	handler.go          chatVia(...)           账号池路径的唯一漏斗
//	chattool_send.go    dispatcher.Chat(...)   工具循环的直发路径
//	channels_forward.go p.Chat(...)            渠道（Key 型）路径
//
// # off 时零改动
//
// h.cfg.Armor 为 nil 或 mode=off ⇒ **原样返回入参切片**（同一底层数组、同一长度），
// 未启用功能不改变任何字节。
func (h *Handler) armorBody(body []byte) []byte {
	if h == nil || h.cfg.Armor == nil {
		return body
	}
	return prompt.ApplyArmorToBody(body, h.cfg.Armor)
}
