// jsonc.go 让 config.json **可以带注释**（JSONC 的子集）。
//
// # 为什么需要它（用户原话）
//
//	「我发现这个 Config 配置有点太丑了，不知道怎么回事啊，太乱了我只能说。」
//
// 诊断出来的四类"乱"，其中两类（顺序乱、写了等于没写）可以靠"重写一份干净配置"
// 解决；但**加了注释的干净配置会被 `encoding/json` 直接拒绝** ——
// 标准库不认 `//` 与 `/* */`。
//
// 不给注释的话，"重写一份干净的 config"就只是"把字段排整齐"，
// 而下一个来看配置的人（包括几个月后的用户自己）仍然不知道
// `soft_rate` 是什么、`idle_weight_per_hour` 调大点会怎样。
// 所以注释是这次整理的核心价值，必须先支持它。
//
// # 支持范围（刻意是 JSONC 的**子集**）
//
//	✅ `// 行注释`
//	✅ `/* 块注释 */`
//	❌ 尾随逗号（`[1,2,]`）—— 不做
//	❌ 单引号字符串 —— 不做
//
// 尾随逗号**不做**是有意的：它要求解析器理解"逗号后面是不是真的结束了"，
// 而那正是最容易在字符串边界上出错的地方。注释剥离没有这个风险
// （只要字符串状态机正确，`//` 与 `/*` 就永远不会是"字符串内部的东西"）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// stripJSONComments 把 JSONC 里的注释去掉，返回**等长的**字节切片
// （注释位置替换成空格，保留所有换行）。
//
// # 为什么必须等长（而不是"删掉注释重新拼"）
//
// 因为要报**正确的行号与列号**。删掉注释会让后面的错误位置整体前移，
// 用户按报的坐标去文件里找，找到的是别的东西 —— 比不报位置更糟。
// 用空格替换则行/列完全不变（换行符一律保留，见下）。
//
// ⚠ 换行符必须原样保留（不能把块注释整体压成一行）：否则一个跨 10 行的
// 块注释后面的报错行号会差 10 行。所以这里只把**非换行字符**替成空格。
//
// # 状态机
//
//	正常        扫描引号与注释起始
//	字符串中    转义符（\）跳过一个字符；未转义的 " 结束字符串
//	行注释      直到换行为止
//	块注释      直到 */ 为止
//
// 字符串状态是**必需的**（不能只做正则替换）：`"http://x"` 里的 `//`
// 是 URL 的一部分，不是注释。这个坑很常见，且症状是"配置里写了个 URL
// 就解析失败"，很难往"注释剥离"上想。
func stripJSONComments(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)

	const (
		stNormal = iota
		stString
		stLineComment
		stBlockComment
	)
	st := stNormal
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch st {
		case stNormal:
			switch {
			case c == '"':
				st = stString
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				st = stLineComment
				out[i] = ' '
				out[i+1] = ' '
				i++
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				st = stBlockComment
				out[i] = ' '
				out[i+1] = ' '
				i++
			}
		case stString:
			switch c {
			case '\\':
				// 转义：连下一个字符一起跳过（`\"` 不是字符串结束）。
				i++
			case '"':
				st = stNormal
			}
		case stLineComment:
			if c == '\n' || c == '\r' {
				st = stNormal // 换行本身保留
				break
			}
			out[i] = ' '
		case stBlockComment:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i] = ' '
				out[i+1] = ' '
				i++
				st = stNormal
				break
			}
			if c == '\n' || c == '\r' {
				break // 换行保留：行号不能变
			}
			out[i] = ' '
		}
	}
	return out
}

// unmarshalJSONC 解析"可能带注释"的 JSON。
//
// 两条路径：
//
//	不带注释（绝大多数情况）→ 直接 json.Unmarshal，零开销
//	带注释                 → 先剥离再解析；**剥离后仍解析失败**时，
//	                        把错误按**原始**文本报出来（位置已对齐）
//
// ⚠ 为什么先探测"有没有注释"再决定：`stripJSONComments` 要复制整个
// 缓冲区，而 config.json 每次启动都读。不带注释是常态（程序自己写回的
// 那种），为它白复制一份不值得。
func unmarshalJSONC(raw []byte, v any) error {
	if !bytes.Contains(raw, []byte("//")) && !bytes.Contains(raw, []byte("/*")) {
		return json.Unmarshal(raw, v)
	}
	clean := stripJSONComments(raw)
	if err := json.Unmarshal(clean, v); err != nil {
		// 剥离后仍失败：报原文的错误（位置与原文一致，见函数头说明）。
		return fmt.Errorf("%w（注：config 支持 // 与 /* */ 注释，已自动忽略）", err)
	}
	return nil
}
