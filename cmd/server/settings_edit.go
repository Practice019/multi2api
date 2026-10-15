// settings_edit.go 设置项的**字节级**写回。
//
// # 为什么不能用"解析成 map 再序列化"
//
// 2026-10-15 的真实事故：用户点了一下「保存设置」，config.json 从 26 个顶层键
// 变成只剩设置段那 9 个 —— `api_key` / `listen` / `auth_dir` / `state_file` /
// 全部上游段 / **73 行注释** 全部消失。
//
// 第一层原因（已修）是"解析失败被当成从空开始"；第二层原因就是这里：
// 即使解析成功，**把 map 序列化写回去本身就会丢掉注释与字段顺序** ——
// 那正是用户上一轮报的「配置太乱」的同一个机制。
//
// 所以写回不做"整体序列化"，而是**逐项字节级替换**：
// 只动那几个被改的值的字节，其余（注释、顺序、空行、缩进、未知键）逐字节保留。
// 缺项才插入一行，插进去同样不影响别的。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// jsonValueEnd 从 i 起找出一个 JSON 值的结束位置（不含尾随空白/逗号）。
// 返回 -1 表示畸形。
//
// 支持四种形态：
//
//	字符串  转义感知地扫到收尾引号
//	对象/数组  花括号/方括号配对（字符串内的括号不算）
//	标量    到逗号/右括号/空白为止
func jsonValueEnd(b []byte, i int) int {
	for i < len(b) && isJSONSpace(b[i]) {
		i++
	}
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		j := i + 1
		for j < len(b) {
			if b[j] == '\\' {
				j += 2
				continue
			}
			if b[j] == '"' {
				return j + 1
			}
			j++
		}
		return -1
	case '{', '[':
		open := b[i]
		var closeCh byte = '}'
		if open == '[' {
			closeCh = ']'
		}
		depth := 0
		inStr := false
		for j := i; j < len(b); j++ {
			c := b[j]
			if inStr {
				if c == '\\' {
					j++
					continue
				}
				if c == '"' {
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case open:
				depth++
			case closeCh:
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	default:
		j := i
		for j < len(b) && !isJSONSpace(b[j]) && b[j] != ',' && b[j] != '}' && b[j] != ']' {
			j++
		}
		if j == i {
			return -1
		}
		return j
	}
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// sectionSpanIn 找出顶层段 `"name": { ... }` 的**内部**区间 [start, end)。
//
// 用剥离注释后的副本来定位（剥离保持等长，见 jsonc.go），所以返回的下标
// 对**原文**同样有效 —— 这是"注释不能影响定位"的关键。
func sectionSpanIn(b []byte, name string) (start, end int, ok bool) {
	clean := stripJSONComments(b)
	needle := []byte(`"` + name + `"`)
	i := bytes.Index(clean, needle)
	if i < 0 {
		return 0, 0, false
	}
	j := i + len(needle)
	for j < len(clean) && isJSONSpace(clean[j]) {
		j++
	}
	if j >= len(clean) || clean[j] != ':' {
		return 0, 0, false
	}
	j++
	for j < len(clean) && clean[j] != '{' {
		if clean[j] == ',' || clean[j] == '}' || clean[j] == '"' {
			return 0, 0, false // 值不是对象 ⇒ 不是段
		}
		j++
	}
	if j >= len(clean) {
		return 0, 0, false
	}
	open := j
	depth := 0
	for k := open; k < len(clean); k++ {
		switch clean[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return open + 1, k, true
			}
		}
	}
	return 0, 0, false
}

// replaceKeyValueInSection 在段 `section` 内把 `"key": <value>` 的值换成 valueJSON。
//
// section 为空串 = 顶层。返回 ok=false 表示该键在该段里不存在（调用方据此改为插入）。
//
// ⚠ 只替换**值的那一段字节**，其余（包括该段里其它键、注释、顺序）原样保留。
func replaceKeyValueInSection(raw []byte, section, key string, valueJSON []byte) (out []byte, ok bool) {
	lo, hi := 0, len(raw)
	if section != "" {
		var span bool
		lo, hi, span = sectionSpanIn(raw, section)
		if !span {
			return nil, false
		}
	}
	// 在该段内部找 `"key"`（跳过字符串与注释）。
	keyLit := []byte(`"` + key + `"`)
	clean := stripJSONComments(raw)
	for i := lo; i < hi; {
		c := clean[i]
		if c == '"' {
			// 跳到这个字符串的收尾。
			j := i + 1
			for j < hi {
				if clean[j] == '\\' {
					j += 2
					continue
				}
				if clean[j] == '"' {
					break
				}
				j++
			}
			if j >= hi {
				return nil, false
			}
			if bytes.Equal(clean[i:j+1], keyLit) {
				k := j + 1
				for k < hi && isJSONSpace(clean[k]) {
					k++
				}
				if k < hi && clean[k] == ':' {
					k++
					for k < hi && isJSONSpace(clean[k]) {
						k++
					}
					ve := jsonValueEnd(clean, k)
					if ve < 0 || ve > hi {
						return nil, false
					}
					res := make([]byte, 0, len(raw)-(ve-k)+len(valueJSON))
					res = append(res, raw[:k]...)
					res = append(res, valueJSON...)
					res = append(res, raw[ve:]...)
					return res, true
				}
			}
			i = j + 1
			continue
		}
		if c == '{' || c == '[' {
			i++
			continue
		}
		i++
	}
	return nil, false
}

// insertKeyValueInSection 在段 `section` 里插入一行 `"key": <value>,`。
//
// 三种情形：
//
//	段存在   取段开括号之后插入（只影响那一行的位置）
//	段不存在 连同整个段一起插入（保留其余逐字节）
//	顶层     取顶层对象开括号之后插入
//
// ⚠ 第二、三种都会**新增**字节（与原文件不同长），但都不动已有的任何一个字节：
// 对"用户的配置文件"，能少动一个字节就少动一个字节。
func insertKeyValueInSection(raw []byte, section, key string, valueJSON []byte) (out []byte, ok bool) {
	// 顶层键：插在顶层对象开括号之后。
	at := -1
	if section == "" {
		at = topLevelOpenBrace(raw)
		if at < 0 {
			return nil, false
		}
		insert := []byte("\n  \"" + key + "\": " + string(valueJSON) + ",")
		res := make([]byte, 0, len(raw)+len(insert))
		res = append(res, raw[:at+1]...)
		res = append(res, insert...)
		res = append(res, raw[at+1:]...)
		return res, true
	}
	// 段存在：插在该段开括号之后。
	if lo, _, span := sectionSpanIn(raw, section); span {
		at = lo - 1
		insert := []byte("\n    \"" + key + "\": " + string(valueJSON) + ",")
		res := make([]byte, 0, len(raw)+len(insert))
		res = append(res, raw[:at+1]...)
		res = append(res, insert...)
		res = append(res, raw[at+1:]...)
		return res, true
	}
	// 段不存在：连同整个段一起插入。
	at = topLevelOpenBrace(raw)
	if at < 0 {
		return nil, false
	}
	insert := []byte("\n  \"" + section + "\": {\n    \"" + key + "\": " + string(valueJSON) + "\n  },")
	res := make([]byte, 0, len(raw)+len(insert))
	res = append(res, raw[:at+1]...)
	res = append(res, insert...)
	res = append(res, raw[at+1:]...)
	return res, true
}

// topLevelOpenBrace 返回顶层对象 `{` 的下标（跳过前导空白与注释）。
func topLevelOpenBrace(raw []byte) int {
	clean := stripJSONComments(raw)
	for i := 0; i < len(clean); i++ {
		if isJSONSpace(clean[i]) {
			continue
		}
		if clean[i] == '{' {
			return i
		}
		return -1
	}
	return -1
}

// blankKeyInSection 在段里把 `"key": <value>,` 整段抹成空白。
//
// 只用于**删掉历史遗留的别名键**（见 persist 的调用处）。
// 抹成空白而不是删掉整行：行号因此不变，别的字节一个都不动 ——
// 对"用户的配置文件"，能少动一个字节就少动一个字节。
func blankKeyInSection(raw []byte, section, key string) []byte {
	lo, hi := 0, len(raw)
	if section != "" {
		var ok bool
		lo, hi, ok = sectionSpanIn(raw, section)
		if !ok {
			return raw
		}
	}
	keyLit := []byte(`"` + key + `"`)
	clean := stripJSONComments(raw)
	for i := lo; i < hi; {
		if clean[i] != '"' {
			i++
			continue
		}
		j := i + 1
		for j < hi {
			if clean[j] == '\\' {
				j += 2
				continue
			}
			if clean[j] == '"' {
				break
			}
			j++
		}
		if j >= hi {
			return raw
		}
		if bytes.Equal(clean[i:j+1], keyLit) {
			k := j + 1
			for k < hi && isJSONSpace(clean[k]) {
				k++
			}
			if k < hi && clean[k] == ':' {
				k++
				for k < hi && isJSONSpace(clean[k]) {
					k++
				}
				ve := jsonValueEnd(clean, k)
				if ve < 0 {
					return raw
				}
				// 只把这一段的非换行字符抹成空格（保留换行 ⇒ 行号不变）。
				out := make([]byte, len(raw))
				copy(out, raw)
				for x := i; x < ve; x++ {
					if out[x] != '\n' && out[x] != '\r' {
						out[x] = ' '
					}
				}
				return out
			}
		}
		i = j + 1
	}
	return raw
}

// managedSetting 一个受"保存设置"管理的键（段 + 键 + 新值）。
type managedSetting struct {
	section string // 空串 = 顶层
	key     string
	value   any
}

// applyManagedSettings 把全部受管设置字节级写回 raw。
//
// ⚠ 返回的 missing 是"在原文里找不到、只能插入"的那些键 —— 调用方应当把它
// 记进日志：插入会动到那个段的行布局（虽然其余逐字节保留），
// 而"静默调整用户的文件"是要说出来的。
func applyManagedSettings(raw []byte, sets []managedSetting) (out []byte, missing []string, err error) {
	out = raw
	for _, s := range sets {
		enc, merr := json.Marshal(s.value)
		if merr != nil {
			return nil, nil, fmt.Errorf("编码设置项 %s: %w", s.key, merr)
		}
		if next, ok := replaceKeyValueInSection(out, s.section, s.key, enc); ok {
			out = next
			continue
		}
		next, ok := insertKeyValueInSection(out, s.section, s.key, enc)
		if !ok {
			return nil, nil, fmt.Errorf("定位不到 %s.%s 的插入点（配置畸形？）", s.section, s.key)
		}
		out = next
		missing = append(missing, s.section+"."+s.key)
	}
	return out, missing, nil
}
