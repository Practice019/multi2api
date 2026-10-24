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

// TestLogModelUsesInternalName ★ 请求日志的 model 列必须是**内部名**。
//
// # 用户的要求（2026-10-21）
//
//	「不希望请求日志出现对外名称 请求日志显示原本的就行」
//
// # 两条路径的取值来源不同，所以两处都要查
//
//	渠道分支：本来就用 peek.Model（L1524 已替换成内部名）✓
//	账号池 ：`newChatStat(..., body, ...)` 内部用 `parseModelFromBody(body)` ——
//	        而 body 是**客户端原始请求体**，里面有对外名 ⇒ 会被原样写进日志 ✗
//
// 修法是 newChatStat 之后覆盖 `st.model = peek.Model`。
//
// ⚠ 为什么这条必须用源码断言：日志是**旁路观测**，写错不会报错、不会 500，
//
//	只是那列显示了一个对不上渠道的名字 —— 而用户正是靠它排障。
func TestLogModelUsesInternalName(t *testing.T) {
	src := readHandlerSrc(t)

	// ① newChatStat 之后必须紧跟 st.model = peek.Model
	idx := strings.Index(src, "st := newChatStat(")
	if idx < 0 {
		t.Fatal("找不到 `st := newChatStat(`")
	}
	// 取它后面 1200 字节（够覆盖那段注释 + 赋值）
	tail := src[idx:]
	if len(tail) > 1400 {
		tail = tail[:1400]
	}
	if !strings.Contains(tail, "st.model = peek.Model") {
		t.Errorf("★ newChatStat 之后没有 `st.model = peek.Model`\n" +
			"   ⇒ 账号池路径的日志会用 body 里的**对外名**（parseModelFromBody），\n" +
			"     而用户要求显示原本的内部名")
	}

	// ② 别名替换必须发生在 newChatStat **之前**（否则 peek.Model 还是对外名）
	//
	// ⚠ 2026-10-24 改：别名解析改成一対多 + 轮询后，代码从
	//   `if alias := h.resolveModelAlias(...); alias != peek.Model { peek.Model = alias }`
	//   变成了 `peek.Model = chosen`（chosen 来自 pickAliasTarget）。
	//   判据从"找那行字面量"改成"找任何一个对 peek.Model 的赋值点" ——
	//   守的语义没变（日志要显示内部名），只是不再绑死实现写法。
	aliasIdx := -1
	for _, marker := range []string{"peek.Model = chosen", "peek.Model = cands[0]", "peek.Model = alias"} {
		if k := strings.Index(src, marker); k >= 0 && (aliasIdx < 0 || k < aliasIdx) {
			aliasIdx = k
		}
	}
	if aliasIdx < 0 {
		t.Fatal("找不到任何别名替换赋值（peek.Model = ...）—— 别名替换那一段被改了？")
	}
	if aliasIdx > idx {
		t.Errorf("★ 别名替换在 newChatStat **之后**\n" +
			"   ⇒ 覆盖 st.model 时拿到的还是对外名，日志照样显示别名")
	}
}

// readFileSrc 读 internal/server 下的一个源文件（按需读，避免重复读 handler.go）。
func readFileSrc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读 %s: %v", name, err)
	}
	return string(b)
}
