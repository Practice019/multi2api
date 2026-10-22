// accountimport.go 批量粘贴导入凭证。
//
// # 用户能粘什么
//
// 三种形状都认（都是从浏览器/文件里复制出来的常见形态）：
//
//	{"auth":{"session":"…","api_key":"oma_…","signing_secret":"omas_…"}}
//	{"session":"…","api_key":"…"}                      扁平形
//	纯 session 字符串（一行一个，uuid 形态）
//
// # 只给 session 也接受
//
// 那是导入时的**常态**：api_key/signing_secret 要事后调签发接口才有。
// 若拒绝"只有 session"的凭证，用户会看到"我明明粘贴了，账号却不见了"——
// 所以 Usable() 只判 session，ChatUsable() 才判三把齐。
package monkeycode

import (
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api/internal/gateway"
)

// 编译期断言。
var _ gateway.AccountImportExt = (*Provider)(nil)

// ImportCredentials 把用户粘贴的原文转成可落盘的凭证。
//
// ⚠ **必须"解析失败 → 返回带原因的错误"**，不能静默返回空切片：
// 用户粘错东西时界面要告诉他哪里不对，而不是"导入了 0 个"。
//
// ⚠ 实现必须幂等且无副作用：不写盘、不碰账号池（那是核心的责任，
// 因为只有核心知道 AuthDir 在哪、池怎么对齐）。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	trimmed := strings.TrimSpace(pasted)
	if trimmed == "" {
		return nil, fmt.Errorf("monkeycode: 粘贴内容为空")
	}

	// ① JSONL（注册机导出的 `accounts-export-*.jsonl` 就是这个形态：一行一个 JSON）。
	//
	// # 为什么必须单独判
	//
	// 注册机的导出文件**不带数组括号**，每行一个独立对象。若直接丢给
	// `json.Unmarshal` 会因为"多个顶层值"报错，于是用户看到
	// 「内容不是合法 JSON」—— 而那文件明明是它自己导出的。
	if looksLikeJSONL(trimmed) {
		return importJSONL(trimmed)
	}

	// ② 整段是一个 JSON（对象或数组）？
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return importJSON(trimmed)
	}

	// ③ 否则按"一行一个 session"处理。
	var out []gateway.ImportedCredential
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		a := &Auth{Session: line}
		if !a.Usable() {
			continue
		}
		c, err := credOf(a)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("monkeycode: 没有识别出任何 session（期望 JSON 或一行一个 uuid）")
	}
	return out, nil
}

// importJSON 处理 JSON 形态的导入。
func importJSON(raw string) ([]gateway.ImportedCredential, error) {
	// 先试单个对象，再试数组。
	var one json.RawMessage
	if err := json.Unmarshal([]byte(raw), &one); err != nil {
		return nil, fmt.Errorf("monkeycode: 内容不是合法 JSON：%w", err)
	}
	var list []json.RawMessage
	if strings.HasPrefix(strings.TrimSpace(raw), "[") {
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil, fmt.Errorf("monkeycode: 解析 JSON 数组失败：%w", err)
		}
	} else {
		list = []json.RawMessage{one}
	}

	var out []gateway.ImportedCredential
	var problems []string
	for _, item := range list {
		a, err := parseAuth(item)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		c, err := credOf(a)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		if len(problems) > 0 {
			return nil, fmt.Errorf("monkeycode: %s", problems[0])
		}
		return nil, fmt.Errorf("monkeycode: 没有识别出任何凭证")
	}
	return out, nil
}

// credOf 把一份 Auth 转成核心的导入结果。
func credOf(a *Auth) (gateway.ImportedCredential, error) {
	if a == nil || !a.Usable() {
		return gateway.ImportedCredential{}, fmt.Errorf("monkeycode: 凭证缺 session")
	}
	name, raw, err := MarshalAuthFile(a, "")
	if err != nil {
		return gateway.ImportedCredential{}, err
	}
	return gateway.ImportedCredential{
		FileName: name,
		Raw:      raw,
		UID:      a.UID(),
		Nickname: a.DisplayName(),
	}, nil
}

// looksLikeJSONL 判断是不是"每行一个 JSON 对象"。
//
// 判据：≥2 行，每行都以 { 开头。单行 { 的情况交给 importJSON（它是合法单对象）。
func looksLikeJSONL(s string) bool {
	lines := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			return false
		}
		lines++
	}
	return lines >= 2
}

// importJSONL 逐行解析 JSONL。
//
// 与 importJSON 的差别只有一处：**坏行跳过、不整批失败**。
//
// # 为什么不整批失败（对注册机的文件特别重要）
//
// 注册机的一次导出可能混着多个平台的记录（它按选中项导出，不按源过滤）。
// 遇到一行不是本上游的，正确行为是"跳过它、导入其余"，
// 而不是"整批拒绝" —— 后者会让用户以为整个文件都坏了。
//
// 但**如果一行都没成**，就必须报出第一条原因：否则用户看到
// 「导入了 0 个」却不知道是格式不对还是 session 过期。
func importJSONL(s string) ([]gateway.ImportedCredential, error) {
	var out []gateway.ImportedCredential
	var problems []string
	ok, skippedOther := 0, 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 不是本上游的记录：静默跳过（注册机允许混导）。
		if !isOwnRecord(line) {
			skippedOther++
			continue
		}
		a, err := parseAuth([]byte(line))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		c, err := credOf(a)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		out = append(out, c)
		ok++
	}
	if ok == 0 {
		if len(problems) > 0 {
			return nil, fmt.Errorf("monkeycode: 没有一行能导入（%s）", problems[0])
		}
		if skippedOther > 0 {
			return nil, fmt.Errorf("monkeycode: 这 %d 行都不是 monkeycode 的记录（它们的 platform 是别的上游）", skippedOther)
		}
		return nil, fmt.Errorf("monkeycode: 文件里没有任何记录")
	}
	return out, nil
}

// isOwnRecord 判断一行 JSON 是不是本上游的记录。
//
// 判据是 platform 字段；没有 platform 时退回"有 session 就算"——
// 前者是注册机导出的标准形态，后者兼容用户手工摘出来的单行。
func isOwnRecord(line string) bool {
	var probe struct {
		Platform string `json:"platform"`
		Session  string `json:"session"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return false
	}
	if p := strings.TrimSpace(probe.Platform); p != "" {
		return strings.EqualFold(p, providerID)
	}
	return strings.TrimSpace(probe.Session) != ""
}
