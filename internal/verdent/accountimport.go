// accountimport.go 实现 gateway.AccountImportExt —— 界面上那个「批量导入」按钮。
//
// # 为什么需要它（用户要求「对齐其他上游」）
//
// 不实现时界面里没有这个按钮（前端按 CapImport 显隐），而本上游**最需要它**：
// 账号来源本来就是手工投放，用户手上通常一次有一批 token（从注册机导出的、
// 或从多个设备抓的），一个一个见面板地粘太低效。
//
// # 认哪些形状（都要认，因为用户手上的东西长短不一）
//
//	① 裸 JWT                    eyJhbGci….eyJ1c2Vy….xxx
//	② 带前缀                    Bearer eyJ…
//	③ 一行一个（批量）          上面任意形状 × N 行
//	④ 完整凭证 JSON（扁平形）    {"userId":"…","token":"…","email":"…"}
//	⑤ 完整凭证 JSON（包装形）    {"auth":{"userId":"…","token":"…"}}
//	⑥ 从 DevTools 复制的片段     token=eyJ… / "token": "eyJ…"
//
// ⚠ ⑥ 那种"从别处复制来的一段"最容易被漏：用户看的是 DevTools 的一行，
// 复制出来常带 `token=` 或 `"token": ` 前缀。要求他手工清理等于把一次导入
// 变成一次手工活 —— 所以这里把它们都剥掉。
//
// # 幂等且无副作用（接口硬要求）
//
// 本方法**不写盘、不碰账号池** —— 落盘与池对齐由核心负责（核心才知道
// AuthDir 在哪、池怎么对齐）。这样上游包仍然不依赖 internal/pool。
package verdent

import (
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api/internal/gateway"
)

// 编译期断言。
var _ gateway.AccountImportExt = (*Provider)(nil)

// ImportCredentials 把用户粘的一段文本转成可落盘的凭证。
//
// 返回的每条是 (文件名, 文件内容, uid, 展示名)。
// 解析失败**必须返回带原因的错误**，不能静默返回空切片 ——
// 用户粘错时界面要告诉他"哪里不对"，而不是"导入了 0 个"。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	text := strings.TrimSpace(pasted)
	if text == "" {
		return nil, fmt.Errorf("verdent: 没有粘贴任何内容")
	}
	out := []gateway.ImportedCredential{}
	var lastErr error
	n := 0
	for _, raw := range splitCandidates(text) {
		n++
		a, err := parseAnyCredential(raw)
		if err != nil {
			lastErr = err
			continue
		}
		if a == nil || !a.Usable() {
			lastErr = fmt.Errorf("第 %d 段里没有可用的 token", n)
			continue
		}
		// 补齐身份信息（token 里能解出来的都补上）。
		a.fillFromJWT()
		body, err := MarshalAuthFile(a, "批量导入")
		if err != nil {
			lastErr = err
			continue
		}
		out = append(out, gateway.ImportedCredential{
			FileName: a.FileName(),
			Raw:      body,
			UID:      a.UID(),
			Nickname: a.DisplayName(),
		})
	}
	if len(out) == 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("verdent: 没有解析出任何账号（最后一条原因：%w）", lastErr)
		}
		return nil, fmt.Errorf("verdent: 没有解析出任何账号")
	}
	return out, nil
}

// splitCandidates 把粘进来的文本切成若干"候选凭证"。
//
// 两种情况：
//   - 整段是一个 JSON 对象（含 "token"）→ 只切一段（里面的换行是格式，不是分隔）
//   - 否则按行切，丢掉空行与明显不是凭证的行
func splitCandidates(text string) []string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		// 整段 JSON（可能跨行）。多个对象用换行分隔的情况少见，
		// 按"能不能整段解成一个 JSON 对象"来判。
		if json.Valid([]byte(trimmed)) {
			return []string{trimmed}
		}
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// parseAnyCredential 认下 accountimport.go 文件头列的那几种形状。
func parseAnyCredential(raw string) (*Auth, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("空行")
	}
	// ④⑤ 完整凭证 JSON（两种形态都认 —— 复用 parseAuthFile，口径只有一份）。
	if strings.HasPrefix(s, "{") {
		if a, err := parseAuthFile([]byte(s)); err == nil && a != nil && strings.TrimSpace(a.Token) != "" {
			return a, nil
		}
		// JSON 但不是凭证形状：可能是 {"token":"…"} 这种片段，继续往下剥。
		var probe struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(s), &probe); err == nil && strings.TrimSpace(probe.Token) != "" {
			return &Auth{Token: strings.TrimSpace(probe.Token)}, nil
		}
	}
	// ②⑥ 前缀/片段：Bearer / token= / "token":
	t := s
	for _, pfx := range []string{"Bearer ", "bearer ", "token=", "token: ", "\"token\": ", "\"token\":"} {
		t = strings.TrimPrefix(t, pfx)
	}
	t = strings.Trim(t, "\"',; ")
	// ① 到这里应当只剩裸 JWT。
	if _, err := DecodeJWT(t); err != nil {
		return nil, fmt.Errorf("不是合法 JWT 也不是凭证 JSON: %w", err)
	}
	return &Auth{Token: t}, nil
}

// fillFromJWT 用 JWT 里能解出来的信息补齐 userId / 过期时刻。
//
// ⚠ 只在**字段为空**时补 —— 用户粘的完整凭证 JSON 里那些字段是显式声明的事实，
// 不该被 token 里的值覆盖掉（两者理论上一致，不一致时以文件为准）。
func (a *Auth) fillFromJWT() {
	p, err := DecodeJWT(a.Token)
	if err != nil {
		return
	}
	if strings.TrimSpace(a.UserID) == "" && strings.TrimSpace(a.UserIDLower) == "" {
		a.UserID = p.UserIDString()
	}
	if a.ExpiresAt == 0 && p.Exp > 0 {
		a.ExpiresAt = p.Exp * 1000
	}
}
