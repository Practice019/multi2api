// apikey_usage_wiring_test.go 守卫「API key 用量在**每条**出口都被记录」
//
// # 用户报的现象（2026-10-21）
//
//	「这个没有记录呢」（API 管理列表的「请求」「失败」两列恒为 0）
//
// # 根因：渠道分支在用量记录之前就 return 了
//
//	handler.go L1506  if h.channelServes(peek.Model) { ... return }   ← 渠道请求在这里结束
//	handler.go L1632  st := newChatStat(...)                          ← 用量记录从这里才开始
//	handler.go L1652  st.bump = ...
//
// 走渠道的请求（如 `csg/...`）**一条都不会被记**。
// 而账号池路径走 chatStat.done()（logging.go 的 s.bump 调用）是记的。
//
// 实测：磁盘上 requests=0 fails=0 used=0，而同一把 key 当天发过大量 csg 请求。
//
// # 这条守卫守什么
//
// 用量记录是**旁路观测**，漏记不会报错、不会 500、日志里也没痕迹 ——
// 它只会让界面上那两个数字永远不动。所以只能靠源码级断言钉住：
//
//	① 渠道分支的**成功出口**必须有 bumpKeyUsage(r, tok, true)
//	② 渠道分支的**失败出口**必须有 bumpKeyUsage(r, 0, false)
//	③ 账号池路径的 st.bump 回调仍在（别把已有的拆了）
//
// ⚠ 为什么用源码扫描而不是行为测试：要覆盖四条出口（渠道成功/失败 +
//
//	账号池成功/失败）得造四种上游与两种鉴权上下文，成本远大于收益；
//	而这里要防的是"新增一条出口时忘了记"，那件事在源码里一眼可判。
package server

import (
	"os"
	"strings"
	"testing"
)

func readHandlerSrc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatalf("读 handler.go 失败: %v", err)
	}
	return string(raw)
}

// TestChannelBranchBumpsKeyUsage ★ 渠道分支两个出口都必须记用量。
func TestChannelBranchBumpsKeyUsage(t *testing.T) {
	src := readHandlerSrc(t)

	// 定位渠道分支：从 `if h.channelServes(` 到 `if h.channelServes(` 之后
	// 第一次出现的 `st := newChatStat`（渠道分支在那之前结束）
	start := strings.Index(src, "if h.channelServes(")
	if start < 0 {
		t.Fatal("找不到渠道分支起始 `if h.channelServes(`")
	}
	end := strings.Index(src[start:], "st := newChatStat(")
	if end < 0 {
		t.Fatal("找不到 `st := newChatStat(` —— 账号池路径的起点变了？")
	}
	branch := src[start : start+end]

	if !strings.Contains(branch, "h.bumpKeyUsage(r, 0, false)") {
		t.Errorf("★ 渠道分支的**失败**出口没有记 key 用量（调用 h.bumpKeyUsage(r, 0, false)）\n" +
			"   ⇒ 渠道请求失败时「失败」列不会增长")
	}
	if !strings.Contains(branch, "h.bumpKeyUsage(r, tok, true)") {
		t.Errorf("★ 渠道分支的**成功**出口没有记 key 用量（调用 h.bumpKeyUsage(r, tok, true)）\n" +
			"   ⇒ 渠道请求成功时「请求」列不会增长（用户报的就是这个）")
	}
}

// TestPoolBranchBumpsKeyUsage 账号池路径的 st.bump 回调必须还在。
//
// ⚠ 这条防的是"修渠道时把已有的拆了" —— 本仓反复出现过那类回归。
func TestPoolBranchBumpsKeyUsage(t *testing.T) {
	src := readHandlerSrc(t)
	if !strings.Contains(src, "st.bump = func(") {
		t.Errorf("★ 账号池路径的 st.bump 回调不见了 —— 那条路径的用量记录会被一起丢掉")
	}
	if !strings.Contains(src, "h.cfg.APIKeys.BumpUsage(") {
		t.Errorf("★ 找不到 h.cfg.APIKeys.BumpUsage( —— 用量记录的总开关被拆了")
	}
}

// TestBumpKeyUsageGuardsEmptyKey bumpKeyUsage 在没命中 key 时必须安全返回。
//
// 这是"管理钥匙 / 未启用鉴权"的路径：不能 panic，也不能把用量记到空 id 上。
func TestBumpKeyUsageGuardsEmptyKey(t *testing.T) {
	// 只用零值 Handler：cfg.APIKeys 为 nil ⇒ 立刻返回，不 panic
	h := &Handler{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("★ bumpKeyUsage 在 cfg.APIKeys 为 nil 时 panic: %v", r)
		}
	}()
	h.bumpKeyUsage(nil, 100, true)
}
