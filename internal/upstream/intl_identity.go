// intl_identity.go 国际版（www.workbuddy.ai）对话请求的**客户端身份与渠道校验**。
//
// # 为什么单独一个文件
//
// 这套头不是"多几个 header"那么随意：国际版上游 2026-10 起上线了渠道校验，
// 对话请求缺会话头族就回 400 / code 11128（displayTips「这次调用没通过渠道校验，
// 可能不是官方客户端发出的」）。判据与取值来自 Agent2API 的实测抓包
// （core/endpoints.rs 的 EDITION_INTL + providers/workbuddy/adapter.rs 的 chat_headers），
// 本机 2026-10-21 逐项复现确认：补齐后同样的账号立刻能出流。
//
// # 缺一不可的三层（每层都单独验证过）
//
//	① 身份头：UA(5.8.4 / workbuddy-ai) + X-IDE-Name/Type/Version + X-Product + X-Agent-*
//	② 会话头族：X-Conversation-ID / -Request-ID / -Message-ID / X-Root-Request-ID / X-Request-ID
//	③ 请求体：**第一条消息必须是 system**
//
// 只补 ① ⇒ 仍 11128；补 ①+② ⇒ 11128 但 msg 变成 "first message is not system prompt"
// （说明会话头已过，卡在 body 形态）；三层齐了 ⇒ 200 + SSE。
//
// # 国内版**不受影响**
//
// 国内（www.codebuddy.cn / copilot.tencent.com）没有这层校验（实测无 system、
// 无会话头也 200）。所以这里全部按 domain 分支，cn 路径一个字节都不改 ——
// 改 cn 的出站身份属于风险敏感的变更，没有抓包依据就不动。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

const (
	// intlChatUA 国际版对话链路的完整 UA（2026-10-16 对 5.8.4 客户端抓包值）。
	// ⚠ 产品段是小写 workbuddy-ai，末段 CLI/2.161.0 是客户端内置 CLI 版本。
	intlChatUA = "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0"
	// intlChatIDEVersion 对话链路的 X-IDE-Version。与 UA 的 5.8.4 同源；
	// 计费/登录链路不吃这套身份，所以不做全局替换。
	intlChatIDEVersion = "5.8.4"
	// intlAcceptLanguage 国际版按 en-US 发。
	intlAcceptLanguage = "en-US"
	// intlDefaultSystem 客户端没带 system 时补的默认提示词。
	// 内容取最中性的表述：它只是为了让"第一条是 system"成立，不塑造人格。
	intlDefaultSystem = "You are a helpful assistant."
)

// IsIntlDomain 报告这个账号的 domain 是否属于国际版（www.workbuddy.ai）。
//
// 判据用 domain 而不是 channel 字段：channel 是导入时的标注（可能空），
// domain 是请求真正要打的那个主机 —— 拿它判不会出现"标了 intl 却打国内域名"。
func IsIntlDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	return strings.Contains(d, "workbuddy.ai")
}

// ApplyIntlChatHeaders 注入国际版对话身份头与会话头族（覆盖同名项）。
func ApplyIntlChatHeaders(req *http.Request, domain string) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		domain = "www.workbuddy.ai"
	}
	origin := "https://" + domain
	convReq, msg, trace, span := hexN(16), hexN(16), hexN(16), hexN(8)

	h := req.Header
	// ① 客户端身份
	h.Set("User-Agent", intlChatUA)
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Version", intlChatIDEVersion)
	h.Set("X-Product", "SaaS")
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Purpose", "conversation")
	h.Set("X-Agent-Type", "main")
	h.Set("X-Private-Data", "true")
	// ② 会话头族（渠道校验的判据）：会话 id 带连字符，其余是 32 位 hex；
	//    -Request-ID 与 -Root-Request-ID 必须**同值**（官方一次 send 内复用）。
	h.Set("X-Conversation-ID", uuidV4())
	h.Set("X-Conversation-Request-ID", convReq)
	h.Set("X-Root-Request-ID", convReq)
	h.Set("X-Conversation-Message-ID", msg)
	h.Set("X-Request-ID", msg)
	// 链路头族：W3C traceparent + b3 + X-B3-*（官方 5.8.4 随带）
	h.Set("X-Trace-ID", trace)
	h.Set("traceparent", "00-"+trace+"-"+span+"-01")
	h.Set("b3", trace+"-"+span+"-1")
	h.Set("X-B3-TraceId", trace)
	h.Set("X-B3-SpanId", span)
	h.Set("X-B3-Sampled", "1")
	// 浏览器侧习惯头：官方 Electron 客户端都会带
	h.Set("Accept-Language", intlAcceptLanguage)
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-CodeBuddy-Request", "1")
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")
}

// EnsureSystemFirst 保证 messages[0] 是 system（国际版上游的硬要求）。
//
// 不是 system 就在最前面插一条默认 system。原 body 的**其它字段原样保留**
// （用 map[string]json.RawMessage 只重写 messages，避免把 tool_calls /
// response_format / stream_options 这些我们还不认识的字段吃掉）。
func EnsureSystemFirst(body []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return body
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil || len(msgs) == 0 {
		return body
	}
	var role string
	if r, ok := msgs[0]["role"]; ok {
		_ = json.Unmarshal(r, &role)
	}
	if role == "system" || role == "developer" {
		return body
	}
	sys := map[string]json.RawMessage{
		"role":    json.RawMessage(`"system"`),
		"content": json.RawMessage(`"` + intlDefaultSystem + `"`),
	}
	msgs = append([]map[string]json.RawMessage{sys}, msgs...)
	nb, err := json.Marshal(msgs)
	if err != nil {
		return body
	}
	raw["messages"] = nb
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// uuidV4 生成带连字符的 v4 UUID（会话 id 形态）。
func uuidV4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return hexN(16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// hexN 生成 n 字节的十六进制串（n=16 → 32 位 hex）。
func hexN(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
