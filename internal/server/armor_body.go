package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	"workbuddy2api/internal/prompt"
)

// armorBlockedCode 破甲拦截的契约错误码（前端/脚本按它判定，不按文案）。
const armorBlockedCode = "prompt_armor_blocked"

// armorRejection 一次"被破甲策略拦下"的结果。
//
// # 为什么是**类型化错误**而不是一个布尔
//
// 拦截必须一路传到 HTTP 层才有的说：中间隔着账号轮转、别名切换、工具循环三层，
// 每一层都会把裸 error 当成"上游/网络失败"处理。用一个具体类型 + errors.As 判定，
// 才能在这些层里**显式早退**，而不是被分类成账号故障（本仓刚因"把非账号问题算成
// 账号层失效"摘过健康号，见 doc 里那条教训）。
//
// # 语义（照用户拍板，别自创）
//
//	状态码 4xx（403：请求合法但策略拒绝） + {"error":{"code":"prompt_armor_blocked",…}}
//	**绝不算账号失败**：不打账号、不换号、不进冷却、不禁用渠道 key
type armorRejection struct {
	// Detection 命中的模式与上下文（写日志、给用户看文案）。
	Detection prompt.Detection
	// Target 这次判决看到的目标（上游 id / 渠道名）。
	Target prompt.Target
}

func (e *armorRejection) Error() string {
	return fmt.Sprintf("破甲拦截：命中 %q（目标 %s/%s）", e.Detection.Pattern, e.Target.Upstream, e.Target.Channel)
}

// Status 拦截的 HTTP 状态码。
//
// 403 而不是 400：请求本身语法完全合法（它是一个能正常处理的 JSON），
// 只是被本地策略拒绝 —— 4xx 区间里 403 的语义最贴（用户给的是"400 或 403"）。
func (e *armorRejection) Status() int { return http.StatusForbidden }

// Message 给用户看的一句话（照拒绝语义：说清"去掉什么再试"）。
func (e *armorRejection) Message() string {
	return "请求未发出：本地破甲策略命中了提示词里的破甲/越狱特征（模式 " + e.Detection.Pattern +
		"）。请去掉破甲/越狱相关字样后重试；若这是误判，可在设置页把这一档改为「观察」或调整范围/模式库。"
}

// armorRejectionOf 判定一个 error 是不是破甲拦截（各处早退分支的唯一判据）。
func armorRejectionOf(err error) *armorRejection {
	var rej *armorRejection
	if errors.As(err, &rej) {
		return rej
	}
	return nil
}

// armorBody 核心**出站边界**：注入 / 拦截 / 原样放行。
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
// （没人断言"装饰器实现了所有扩展点"）。
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
// # 三态（2026-11-13 用户拍板，判决在 prompt.Judge）
//
//	off              原样返回入参切片（同一底层数组，未启用功能不改变任何字节）
//	append / replace 范围内注入（写法沿用原值 —— 已有设置不被覆盖）
//	block            范围内命中 ⇒ 返回 *armorRejection，**调用方必须直接返回**，
//	                 不许再调上游（这是"上游一次都没被调用"的保证方式）
//
// ⚠ target 由调用方给：三个调用点上"这次要发给谁"都是已知的（上游 id / 渠道名），
//
//	按名单过滤才可能成立。**不要**把它做成 Handler 上的字段 —— Handler 是全局共享的，
//	并发请求会串目标（A 的上游被当成 B 的上游判决）。
func (h *Handler) armorBody(target prompt.Target, body []byte) ([]byte, *armorRejection) {
	if h == nil || h.cfg.Armor == nil {
		return body, nil
	}
	// ⚠ 只读**一次**配置：判决与注入必须看到同一份（否则会出现"判决用 append、
	//   注入用 replace"这种同一请求里两个模式）。
	cfg := h.cfg.Armor.Config()
	out, d := prompt.Judge(cfg, target, body)
	switch d.Action {
	case prompt.ActionBlock:
		// 命中且未开观察 ⇒ 拦下。body 原样返回（**不剥离、不改写** —— 用户原话
		// "让他自己去修改"），调用方据此早退，因此这次请求**不会**到达上游。
		h.noteArmorHit(target, d, true)
		return body, &armorRejection{Detection: d.Detection, Target: target}
	case prompt.ActionObserve:
		// 观察模式：放行，只留痕（用户要求"日志与统计里能查到"）。
		h.noteArmorHit(target, d, false)
		return body, nil
	}
	return out, nil
}

// armorCheck 只做判决、**不改写** body，返回拦截（若命中）。
//
// # 用在哪
//
// 单上游回落路径（cfg.Provider == nil）：那里的 body 由 upstream.Client 内部的
// applyPrompt 注入（见 armorWhitelist 里那条豁免），**不能**在这里重复注入 ——
// 那会改变该路径既有字节（模板改写等）。但「禁用破甲」这一档必须仍然生效，
// 否则它在单上游部署下是个空开关（"配了没用"是这一层最坏的失效形态）。
//
// 与 armorBody 的分工：两者都问同一个 prompt.Judge，只是一个要注入结果、一个只要判决。
// 判决逻辑只有 Judge 一份 —— 这里不重复任何判据。
func (h *Handler) armorCheck(target prompt.Target, body []byte) *armorRejection {
	if h == nil || h.cfg.Armor == nil {
		return nil
	}
	_, d := prompt.Judge(h.cfg.Armor.Config(), target, body)
	switch d.Action {
	case prompt.ActionBlock:
		h.noteArmorHit(target, d, true)
		return &armorRejection{Detection: d.Detection, Target: target}
	case prompt.ActionObserve:
		h.noteArmorHit(target, d, false)
	}
	return nil
}

// armorSealed 给"重试包装器"用的已盖章 body。
//
// # 为什么它与 armorBody 是两个函数
//
// 那几处（把 body 交给 MaybeRefusalRetry / retryRefusalNonStreamBytes 的地方）的 body
// **已经过判决**：要么来自 chatVia / forwardViaChannelAs 内部那一次 armorBody，
// 要么是判决为放行的同一份。在那里再判一次没有新信息，却会引入一个新的失效面 ——
// "包装器里的第二次判决把请求拦掉了，而客户端已经收到 200"。
//
// 所以这里只做幂等注入（block 态下 ArmorEnabled 为假 ⇒ 一个字节都不改）。
func (h *Handler) armorSealed(body []byte) []byte {
	if h == nil || h.cfg.Armor == nil {
		return body
	}
	return prompt.ApplyArmorToBodyCfg(body, h.cfg.Armor.Config())
}

// noteArmorHit 命中留一行（**给人看**的那种），并计入 ArmorState 的读数。
//
// 这一行必须能回答三件事（用户要拿它校准模式库）：
//
//	命中了哪条模式 · 目标是哪个上游/渠道 · 这次是拦了还是只观察
//
// ⚠ 用 log.Printf 而不是请求日志表：命中可能发生在**请求还没落到那条路径**之前
// （渠道分支在 chatStat 之前就 return），表里可能根本没这一行。排障时先看 stdout。
func (h *Handler) noteArmorHit(target prompt.Target, d prompt.Decision, blocked bool) {
	verdict := "观察（已放行）"
	if blocked {
		verdict = "拦截（未发出）"
	}
	log.Printf("破甲：%s 命中模式 %q 目标 upstream=%q channel=%q 范围=%s 名单命中=%v 片段=%q",
		verdict, d.Detection.Pattern, target.Upstream, target.Channel, d.Scope, d.InList, d.Detection.Excerpt)
	if h.cfg.Armor != nil {
		h.cfg.Armor.NoteHit(blocked)
	}
}

// rejectArmorBlock 把一次拦截写成终态回执（4xx + 契约错误码）。
//
// 与 retryWithDegradedPrompt（内容策略拦截）同一形态：**绝不调用 applyErrorPolicy**、
// 绝不 fail(uid) —— 策略是账号无关的，同一套判据在所有账号后面。
func (h *Handler) rejectArmorBlock(w http.ResponseWriter, st *chatStat, rej *armorRejection) {
	writeOpenAIError(w, rej.Status(), armorBlockedCode, rej.Message())
	if st != nil {
		st.status = rej.Status()
		st.decision = "armor_blocked:client"
	}
}
