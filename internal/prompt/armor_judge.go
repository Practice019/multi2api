// armor_judge.go 破甲三态判决 + 范围/名单（2026-11-13 用户拍板）。
//
// # 三态
//
//	off（无）        不管：不注入、不检测
//	append / replace 启用破甲：注入（注入写法沿用原值 —— **已有设置不许被覆盖**）
//	block            禁用破甲：只拦截 —— 范围内命中就**不发出**
//
// # 范围 + 名单（两件，不是四个列表）
//
//	范围   all（全部上游/渠道）/ include（只对名单内）/ exclude（除名单外）
//	名单   条目可以是**上游 id**（phanthywork / loomy）或**渠道名**（渠道页里那个名字）
//
// 两级（上游 / 渠道）就是全部粒度 —— 用户明确说不到模型。
//
// # 判决表（这张表就是本文件）
//
//	范围内 + 命中  →  observe=true: observe（放行，只记日志） / observe=false: block
//	范围内 + 未命中 →  pass
//	范围外          →  pass（**连检测都不做**，见 design-notes 的 L1）
package prompt

import (
	"strings"
)

// Target 一次出站请求的**目标**（范围/名单匹配用）。
//
// 两级而非一个字符串：同一个请求可能既有上游 id（账号池路径）又有渠道名
// （渠道路径）。空串 = 这一级不适用（例如渠道路径没有上游 id）。
type Target struct {
	// Upstream 上游 id（如 phanthywork / loomy）。
	Upstream string
	// Channel 渠道名或渠道 id（渠道页里那个）。
	Channel string
}

// 范围取值。
const (
	// ArmorScopeAll 全部上游/渠道（默认；空串按它处理）。
	ArmorScopeAll = "all"
	// ArmorScopeInclude 只对名单内（白名单语义）。
	ArmorScopeInclude = "include"
	// ArmorScopeExclude 除名单外（黑名单语义）。
	ArmorScopeExclude = "exclude"
)

// 判决动作。
const (
	// ActionPass 放行（原样透传）。
	ActionPass = "pass"
	// ActionInject 注入后发出。
	ActionInject = "inject"
	// ActionBlock 拒绝发出（调用方必须早退）。
	ActionBlock = "block"
	// ActionObserve 命中但放行（观察模式，只记日志）。
	ActionObserve = "observe"
)

// Decision 一次判决的结果。
//
// 为什么连"范围是否命中"也带出来：用户报障时问的第一句话通常是
// 「我配了名单，怎么没拦」—— 日志里要能回答"这次判决看见了什么"，
// 而不是只打一句 blocked。
type Decision struct {
	// Action 见上面四个动作常量。
	Action string
	// Detection 命中的证据（只在 block / observe 时有值）。
	Detection Detection
	// Scope 本次生效的范围（归一化后）。
	Scope string
	// InList 名单是否命中（exclude 语义下也填"是否在名单里"，便于读日志）。
	InList bool
}

// Judge 唯一判决入口：给一份出站 body 与它的目标，回答"该怎么处理"。
//
// 返回的 body：
//
//	pass    → **同一底层数组**（未启用/范围外不得改变任何字节）
//	observe → 同一底层数组（放行）
//	block   → 同一底层数组（**不剥离、不改写** —— 用户原话"让他自己去修改"）
//	inject  → 注入后的新切片
func Judge(cfg ArmorConfig, t Target, body []byte) ([]byte, Decision) {
	mode := normalizeMode(cfg.Mode)
	switch mode {
	case ArmorBlock:
		scope := normalizeScope(cfg.BlockScope)
		inList := targetMatches(cfg.BlockTargets, t)
		d := Decision{Action: ActionPass, Scope: scope, InList: inList}
		if !inScope(scope, inList) {
			return body, d
		}
		text, _ := ArmorBodyText(body)
		det, hit := DetectArmor(text, ArmorPatterns(cfg))
		if !hit {
			return body, d
		}
		if cfg.Observe {
			d.Action = ActionObserve
		} else {
			d.Action = ActionBlock
		}
		d.Detection = det
		return body, d
	case ArmorAppend, ArmorReplace:
		scope := normalizeScope(cfg.InjectScope)
		inList := targetMatches(cfg.InjectTargets, t)
		if !inScope(scope, inList) {
			return body, Decision{Action: ActionPass, Scope: scope, InList: inList}
		}
		return ApplyArmorToBodyCfg(body, cfg), Decision{Action: ActionInject, Scope: scope, InList: inList}
	}
	// off / 未知：零改动。
	return body, Decision{Action: ActionPass, Scope: ArmorScopeAll}
}

// normalizeMode 归一化模式（空 = off）。
func normalizeMode(mode string) string {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		return ArmorOff
	}
	return m
}

// normalizeScope 归一化范围；认不出的值按 all（= 什么都不限制）。
//
// ⚠ 与配置层分工：非法取值在**启动时**就报错（见 cmd/server 的 normalize），
// 这里只是不让一个坏值把整个判决判成"全不生效"。
func normalizeScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case ArmorScopeInclude:
		return ArmorScopeInclude
	case ArmorScopeExclude:
		return ArmorScopeExclude
	}
	return ArmorScopeAll
}

// inScope 范围判据（唯一的解释点）。
//
//	all     → true（名单不参与判决）
//	include → inList
//	exclude → !inList（**空名单 = 全拦**，与 all 等效 —— 这一点写进注释，别让人猜）
func inScope(scope string, inList bool) bool {
	switch scope {
	case ArmorScopeInclude:
		return inList
	case ArmorScopeExclude:
		return !inList
	}
	return true
}

// targetMatches 名单是否命中这个目标（上游 id 或渠道名，任一级命中即算命中）。
//
// 归一化：小写 + 去首尾空白（配置是人工填的，" PhanthyWork " 与 "phanthywork" 应当等价）。
// 空条目跳过（一个空串会让名单"恒命中"，那是把范围开关废掉）。
func targetMatches(list []string, t Target) bool {
	up, ch := strings.ToLower(strings.TrimSpace(t.Upstream)), strings.ToLower(strings.TrimSpace(t.Channel))
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if up != "" && e == up {
			return true
		}
		if ch != "" && e == ch {
			return true
		}
	}
	return false
}
