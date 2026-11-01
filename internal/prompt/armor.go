// armor.go 破甲层：出站请求自动注入 dsh-purge 的**逐字**注入正文。
//
// # 来源与"1:1"的含义（这一版改过，前因后果留档）
//
// 用户要求「按照原项目 1:1 移植」。第一版我拿 `test/fixtures/inject-clauses.json`
// 当来源 —— 那是**片段夹具**（被截断的行），于是我自己"重组成语义完整的条款块"，
// 结果只有 1230 字符，而原项目真正注入的是 `~/.dsh/prompt-inject.md`：
// **21194 字符 / 226 行 / 47 段**（见 dsh-purge 的 docs/inject-review.md，
// 它就是逐段评审那份文件的）。覆盖率 5.8% —— 那不是移植，是重写。
//
// 现在：`armor-payload.md` 就是那份文件**逐字节**的副本（Copy-Item 后校验过 -ceq），
// 作为**唯一内置载荷**。要改就改那一个文件；不存在第二份"浓缩版"。
//
// ⚠ 逐字注入的后果要写清：上游会收到你本机那份操作者指令的**全文**
// （含称呼、内部约定、路由表等）。这是用户明确要的效果，不是失误；
// 启动日志会打印载荷来源与字符数，随时可核对。
//
// # 与既有 prompt 层的关系
//
// 本包原先只有一件事：**替换/透传**客户端的 system，用来消灭上游内容审核的
// 逐字指纹（见 prompt.go 的文件头）。破甲是**另一个方向**的需求：不只是换掉，
// 而是往出站提示词里**加入**一段操作者意志。两者共用同一条出站改写路径
// （internal/upstream/client.go 的 applyPrompt），所以放在同一个包里。
//
// # 三个模式
//
//	off      不注入（默认）
//	append   保留客户端 system，把破甲正文**追加**在后面（推荐：客户端人格仍在）
//	replace  用破甲正文**替换**客户端 system（最彻底，代价是丢掉客户端自己的约定）
//
// # 幂等
//
// 注入块带 <!-- WB2API:ARMOR:CLAUSES --> 标记，重复注入会先剥旧块再加新块。
// 理由：这条路径会被**重试**经过（审核拦截降级重试、拒绝重试），
// 每次追加一遍会让提示词越来越长，最后把上下文挤爆。
package prompt

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

//go:embed armor-payload.md
var armorPayload string

// ArmorPayload 内置载荷（1:1 副本，见文件头）。导出给测试与"载荷有没有被改过"的核对。
func ArmorPayload() string { return armorPayload }

// 破甲模式。
const (
	// ArmorOff 不注入（默认）。
	ArmorOff = "off"
	// ArmorAppend 保留客户端 system，追加破甲条款。
	ArmorAppend = "append"
	// ArmorReplace 用破甲人设替换客户端 system。
	ArmorReplace = "replace"
)

// armorStart / armorEnd 注入块的标记（幂等用，见文件头）。
const (
	armorStart = "<!-- WB2API:ARMOR:CLAUSES -->"
	armorEnd   = "<!-- WB2API:ARMOR:END -->"
)

// ArmorConfig 破甲配置。
type ArmorConfig struct {
	// Mode off / append / replace。空串按 off 处理（默认不开）。
	Mode string
	// Payload 注入正文（空 = 用内置 armor-payload.md，即原项目那份文件的逐字副本）。
	//
	// ⚠ 逐字注入意味着**上游会收到你本机那份操作者指令全文**（含称呼、内部约定等）。
	//   这是用户明确要的效果，不是失误；启动日志会打印载荷来源与字符数，随时可核对。
	Payload string
}

// ArmorEnabled 该模式是否要注入。
func ArmorEnabled(mode string) bool {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case ArmorAppend, ArmorReplace:
		return true
	}
	return false
}

// ArmorText 破甲注入块的正文 —— **逐字**（外层只包幂等标记）。
//
// 载荷： cfg.Payload（配置给的）> 内置 armor-payload.md（原项目那份文件的逐字副本）。
//
// ⚠ 标记必须包住整块：第一版只包了"条款"部分，于是 append 模式下第二次注入
//
//	会把标记外的内容重复一遍（剥离只从标记开始剥）—— 幂等测试当场抓住。
//	现在整块（= 全部注入正文）都在标记内，重复注入只会剥掉重加。
func ArmorText(cfg ArmorConfig) string {
	payload := cfg.Payload
	if strings.TrimSpace(payload) == "" {
		payload = armorPayload
	}
	return armorStart + "\n" + strings.TrimRight(payload, "\n") + "\n" + armorEnd
}

// ApplyArmor 把破甲块注入出站提示词。
//
// 返回注入后的文本与是否发生了改动：
//
//	Mode=off                 → (base, false)
//	Mode=append              → (base + 破甲块, true)（base 为空时就是破甲块本身）
//	Mode=replace             → (破甲块, true)
//
// ⚠ 幂等：base 里已有注入块时先剥掉旧块。重试路径会多次经过这里。
func ApplyArmor(base string, cfg ArmorConfig) (string, bool) {
	if !ArmorEnabled(cfg.Mode) {
		return base, false
	}
	block := ArmorText(cfg)
	stripped := stripArmorBlock(base)
	var out string
	if strings.EqualFold(strings.TrimSpace(cfg.Mode), ArmorReplace) {
		out = block
	} else {
		// append：空 base 时不要留下一个空行开头的提示词。
		trimmed := strings.TrimSpace(stripped)
		if trimmed == "" {
			out = block
		} else {
			out = trimmed + "\n\n" + block
		}
	}
	// ⚠ changed 的语义是"文本真的变了"，不是"这个模式本来就会注入"。
	//   第一版在 append 分支直接 `return ..., true`，于是"第二次注入"也报改动 ——
	//   而幂等测试正是靠这个返回值判定的（它当场报红，才没把重复注入带出去）。
	return out, out != base
}

// stripArmorBlock 剥掉此前注入过的破甲块（幂等，见文件头）。
//
// 只认**我们自己**的标记：没有标记的文本原样返回 —— 用户或客户端自己写的
// 类似内容不该被这一层删掉。
func stripArmorBlock(s string) string {
	for {
		i := strings.Index(s, armorStart)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], armorEnd)
		if j < 0 {
			// 只有开标记没有闭标记（被截断）⇒ 从这里切到结尾，避免留下半个块。
			return strings.TrimRight(s[:i], " \t\r\n")
		}
		s = s[:i] + s[i+j+len(armorEnd):]
	}
}

// ArmorState 破甲配置的**热可切**持有者。
//
// # 为什么要一个状态对象而不是字符串字段
//
// 设置页要能"改完立即生效"。出站客户端是启动时构造的（每条上游一份），
// 配置字符串会被**复制**进去 —— 改设置改不到那些副本。所以按本仓既有手法
// （PromptGate 就是这样）给一个指针对象：装配层持有、设置层调 Set、出站路径每次读。
type ArmorState struct {
	mu  sync.RWMutex
	cfg ArmorConfig
}

// NewArmorState 构造（cfg.Mode 为空 = 关闭）。
func NewArmorState(cfg ArmorConfig) *ArmorState { return &ArmorState{cfg: cfg} }

// Config 当前配置（副本）。
func (s *ArmorState) Config() ArmorConfig {
	if s == nil {
		return ArmorConfig{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Mode 当前模式（空 = 关闭）。
func (s *ArmorState) Mode() string { return s.Config().Mode }

// Set 改配置（设置页保存时调用）。nil 接收者安全（"这条上游没有注入层"是合法状态）。
func (s *ArmorState) Set(cfg ArmorConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// SystemText 从请求体里取出**第一条** system/developer 消息的文本。
//
// 用途：append 模式要"保留客户端人格 + 追加破甲条款"，而 Rewrite 是**整体替换** ——
// 没有这个读取口，append 只能从空白开始拼，客户端 system 就丢了（那其实是 replace）。
//
// content 支持字符串与分片数组（多模态），只取 type=text 的分片 —— 与 Rewrite 同口径。
// 两边不一致会让"读出来的原文"与"写回去的内容"不是同一份，append 后的文本看着像被篡改。
//
// 读不到（没有 messages / 没有 system）→ ok=false，调用方按"没有客户端人格"处理。
func SystemText(body []byte) (string, bool) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return "", false
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			return c, true
		case []any:
			var sb strings.Builder
			for _, part := range c {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := pm["type"].(string); t != "text" {
					continue
				}
				if s, _ := pm["text"].(string); s != "" {
					sb.WriteString(s)
				}
			}
			return sb.String(), true
		}
	}
	return "", false
}
