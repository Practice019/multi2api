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

	// 整段是一个或多个 JSON？
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return importJSON(trimmed)
	}

	// 否则按"一行一个 session"处理。
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
