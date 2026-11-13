package prompt

import (
	"encoding/json"
	"strings"
)

// ApplyArmorToBody 在**请求体**层面注入破甲，让**所有**出站请求都吃得到
// （不只是走 internal/upstream 的那一家上游）。
//
// # 与 ApplyRetryNudge 的分工（两者都会改 system，但各自独立、可叠加）
//
//	ApplyRetryNudge   命中的**那一次**重试，往尾部追加一句「直接执行、不要再问」；
//	                 **只认 messages[0] 是 system**（逐字照抄原项目 withNudge，见 refusal.go）。
//	ApplyArmorToBody  每一次出站请求注入人格 + 条款；
//	                 按 **SystemText 同一口径**找第一条 system/developer（不限于第 0 条），
//	                 找不到就**插入**一条。
//
// # 为什么"没有 system 也要插入"
//
// 用户的要求是「只要别人向我们这个网关发请求就自动破甲」。客户端只要不发 system，
// 就绕过了这一层 —— 那等于把开关交给调用方。所以缺 system ⇒ 插一条。
//
// # 幂等
//
// 注入块自带标记（见 armor.go），ApplyArmor 会先**剥掉旧块**再注入 ⇒ 同一份请求
// 重复经过这一层**不会叠加**。这条是有意留的余量：workbuddy 那条路径会在
// internal/upstream/client.go 的 applyPrompt 里再经过一次（模板改写），两条路都要安全。
//
// # 失败一律原样返回
//
// 解析不出 JSON / messages 形态不认识 ⇒ 返回**入参字节**。不许因为"想注入"把请求弄坏。
func ApplyArmorToBody(body []byte, st *ArmorState) []byte {
	if st == nil {
		return body
	}
	return ApplyArmorToBodyCfg(body, st.Config())
}

// ApplyArmorToBodyCfg 与 ApplyArmorToBody 同一实现，只是**用调用方已经读到的那份配置**。
//
// # 为什么要有它（不是重复代码）
//
// 出站边界（internal/server 的 armorBody）已经为了**判决**读过一次配置；
// 判决为 inject 时再让 ApplyArmorToBody 去读第二次，等于同一个请求里读两遍同一把锁，
// 且两次读之间配置可能被设置页改掉 —— 那会出现"判决用的是 append、注入用的是 replace"。
// 传进来那一份保证同一次判决与同一次注入看到的是同一份配置。
//
// ⚠ block 态在这里会被 ArmorEnabled 挡掉（**一个字节都不注入**，见 ArmorEnabled 的注释）。
func ApplyArmorToBodyCfg(body []byte, cfg ArmorConfig) []byte {
	if !ArmorEnabled(cfg.Mode) {
		return body // off / block = 零改动（这条是开关的语义本身，有测试钉住）
	}
	// ⚠ 已经盖过章（system 里已有我们的标记）⇒ **原样返回，不再动它**。
	//
	// 这条不是优化，是正确性。重试路径的形状是：
	//
	//	客户原文 + 破甲块(尾) + 重试话术(尾)
	//
	// 若这里再走一遍 ApplyArmor，它会**剥掉块再追加到末尾** ⇒ 话术被挤到块前面；
	// replace 模式下更糟：整段被替换成块 ⇒ **话术直接消失**，重试就白重试了。
	// （接线时推演顺序发现的，有测试钉住。）
	//
	// 代价：调用方若把带标记的 body 再发一次、且此时模式刚从 append 改成 replace，
	// 这一次仍用旧块。可接受 —— 正常客户端请求**从不**携带我们的标记（标记只由本层
	// 自己写入），带标记的只有重试路径，而它本来就该保留上一次的注入。
	if t, ok := SystemText(body); ok && strings.Contains(t, armorStart) {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return body
	}

	// 找第一条 system/developer（口径与 SystemText 一致，避免"选哪条"有两套说法）。
	idx := -1
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role == "system" || role == "developer" {
			idx = i
			break
		}
	}

	if idx < 0 {
		// 没有 system/developer ⇒ 插一条（理由见文件头：否则调用方不发 system 就能绕过）。
		inserted := make([]any, 0, len(msgs)+1)
		inserted = append(inserted, map[string]any{"role": "system", "content": ArmorText(cfg)})
		inserted = append(inserted, msgs...)
		obj["messages"] = inserted
		return marshalBody(body, obj)
	}

	target, ok := msgs[idx].(map[string]any)
	if !ok {
		return body
	}
	switch c := target["content"].(type) {
	case string:
		out, changed := ApplyArmor(c, cfg)
		if !changed {
			return body // 文本没变（例如已经是我们注入的那一份）⇒ 不重编码，保持字节原样
		}
		target["content"] = out
	case []any:
		// 数组形态：把所有 text 块拼成一段基底、注入后放回**一个** text 块，
		// 非 text 块（图片等）原样留在后面。
		//
		// ⚠ 不能"每个 text 块各注入一遍" —— replace 模式下那会把整块载荷复制 N 份。
		var base string
		kept := make([]any, 0, len(c))
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				kept = append(kept, part)
				continue
			}
			if t, _ := pm["type"].(string); t != "text" {
				kept = append(kept, part)
				continue
			}
			if s, ok := pm["text"].(string); ok {
				if base != "" {
					base += "\n"
				}
				base += s
			}
		}
		out, changed := ApplyArmor(base, cfg)
		if !changed {
			return body
		}
		newContent := make([]any, 0, len(kept)+1)
		newContent = append(newContent, map[string]any{"type": "text", "text": out})
		newContent = append(newContent, kept...)
		target["content"] = newContent
	default:
		return body // content 既不是字符串也不是数组：不懂就不动
	}
	msgs[idx] = target
	obj["messages"] = msgs
	return marshalBody(body, obj)
}

// marshalBody 编码失败时回退到入参字节（绝不返回半成品）。
func marshalBody(orig []byte, obj map[string]any) []byte {
	out, err := json.Marshal(obj)
	if err != nil {
		return orig
	}
	return out
}
