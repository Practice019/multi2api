// 账号池列集的**跨上游**不变量。
//
// # 为什么需要这个文件（用户报的问题）
//
// 用户："现在上游列表里，账号池标签页中的上游列表缺少「成功」列。
//
//	每一个上游都需要有「成功」列。"
//
// 实测发现 **3 个上游**缺它，原因各不相同但根子是同一个：
// 前两个人（包括我）把它当成了"可选的排障计数器"，于是显式省略：
//
//	codearts  注释：'成功/熔断/在途 通用计数器。用户要求保持清爽'
//	trae      注释：'熔断/在途按用户本轮要求全上游不显示'
//	zcode     注释：'success/breaker/in_flight 排障计数器，用户已经删掉了'
//
// **三处注释都错了**，而且错法一样：把「成功」与「熔断/在途」混为一谈。
// 事实上：
//
//	熔断 / 在途  用户**明确要求删掉**的只有这两列
//	             （见 gateway.DefaultAccountColumns 的注释）
//	成功         **从来不在删除之列**，它就在默认列集里
//
// 更根本的区别在**数据来源**：
//
//	success  ← pool.NoteSuccess（core 侧 successCount）
//	            **每个账号都有**这个计数器，与上游有没有对应能力位无关
//
// 所以"要不要显示成功列"根本不是上游能选的 —— 核心已经在为每个账号数了。
// 上游漏报它，只是让用户看不到一个**已经存在**的事实。
//
// # 为什么放在 main 包（cmd/server）而不是各上游包
//
// 必须在**一处**能看到全部上游。各上游包里的测试只能看到自己，
// 而"每个上游都要有"是一个**跨包**命题 —— 分散到 11 个包里就没人守了。
//
// 这与 arch_test.go 的分工一致：那个管"依赖方向"，这个管"列集契约"。
package main

import (
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/cline"
	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/lobsterai"
	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/raccoon"
	"workbuddy2api/internal/trae"
	"workbuddy2api/internal/workbuddy"
	"workbuddy2api/internal/zcode"
)

// upstreamCase 一个上游实例 + 它登记用的标识（失败信息里用）。
type upstreamCase struct {
	// id 用于清单覆盖检查的**目录名/上游名**。
	id string
	// label 展示用（qoder/qodercn 共用目录，所以 label 比 id 细）。
	label string
	p     gateway.Provider
}

// allUpstreams 构造每个上游的 Provider —— 全部走各包自己的**契约工厂**
// （`NewProvider()` / `New()`），与 cmd/server/main.go 的注册段同源。
//
// # 为什么用契约工厂而不是 NewWithConfig(具体配置)
//
// 工厂是各包**为契约测试准备的无依赖构造**（它们自己的注释这么写的：
// "只构造，不做网络请求 —— 契约测试会多次调用 factory，不能有副作用"）。
// 用它就不必在这里复刻每个 Config 的一堆必填项 ——
// 而复刻出来的配置一旦与 main.go 分叉，这个测试守的就不是生产形态了。
func allUpstreams(t *testing.T) []upstreamCase {
	t.Helper()
	return []upstreamCase{
		{"workbuddy", "workbuddy", workbuddy.New()},
		// workbuddy-intl 与 workbuddy 是同一个包的两个实例
		//（靠 cfg.DisableGrowthTravel 区分）。它的列集只比国内版少一列
		//（今日签到），成功列在两者里都必须有 —— 所以两个都查。
		{"workbuddy", "workbuddy-intl", workbuddy.NewWithConfig(workbuddy.Config{
			DisableGrowthTravel: true,
		})},
		{"codearts", "codearts", codearts.NewProvider()},
		{"loomy", "loomy", loomy.NewProvider()},
		{"trae", "trae", trae.NewProvider()},
		{"cline", "cline", cline.NewProvider()},
		{"raccoon", "raccoon", raccoon.NewProvider()},
		{"lobsterai", "lobsterai", lobsterai.NewProvider()},
		{"qoder", "qoder", qoder.NewProvider()},
		{"qoder", "qodercn", qoder.NewProviderCN()},
		{"zcode", "zcode", zcode.New(zcode.Config{})},
	}
}

// columnsOf 取一个 Provider 自报的列集，**含"未实现 → 默认列"的回落**。
//
// ⚠ 必须复刻 core 的回落语义，否则这个测试会与生产行为分叉：
//
//	未实现 AccountColumnsExt → DefaultAccountColumns()
//	实现了但返回空数组        → 空（"我一列都不要"，合法值）
//
// 拿 nil 当"用默认列"的哨兵是错的 —— 空数组是合法值，
// 两者混起来会让"这个上游一列都不要"被误判成"它有默认列"。
func columnsOf(p gateway.Provider) []string {
	if ext, ok := gateway.ExtOf[gateway.AccountColumnsExt](p); ok {
		return ext.AccountColumns()
	}
	return gateway.DefaultAccountColumns()
}

// contains 切片里有没有这个字符串。
func contains(list []string, want string) bool {
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

// TestEveryUpstreamShowsSuccessColumn 每个上游都必须显示「成功」列。
//
// # 这条断言直接对应用户的报障
//
// 用户看到的是"上游列表里缺少成功列"。而"缺少"这个词有歧义 ——
// 可能是①整个账号池没有这一列，也可能是②某些上游的分组没有。
//
// 实测是②：**3 个上游**显式省略了它。所以断言必须是逐个上游的 ——
// 只断言默认列集的话，这条 bug 完全测不出来
// （默认列集一直含 success，而 bug 恰恰出在**自报列集**的上游身上）。
func TestEveryUpstreamShowsSuccessColumn(t *testing.T) {
	cases := allUpstreams(t)
	for _, u := range cases {
		if u.p == nil {
			t.Errorf("上游 %s 的 Provider 为 nil —— 检查本文件的构造", u.label)
			continue
		}
		cols := columnsOf(u.p)
		if !contains(cols, gateway.AccountColSuccess) {
			t.Errorf("上游 %s 的账号池列集里没有 %q（成功列）。\n"+
				"用户明确要求「每一个上游都需要有成功列」——\n"+
				"该列数据来自核心的 pool.NoteSuccess（每个账号都有计数器），\n"+
				"与上游有没有对应能力位**无关**。\n"+
				"当前列集: %v\n"+
				"修法：在 %s 的 AccountColumns() 里加 gateway.AccountColSuccess",
				u.label, gateway.AccountColSuccess, cols, u.label)
		}
	}
	if len(cases) < 11 {
		t.Errorf("应覆盖 11 个上游实例，实际只有 %d 个", len(cases))
	}
}

// 「熔断」「在途」仍然**不该**出现（用户此前的另一条要求不能被带回来）。
//
// # 为什么这条必须与上面那条成对
//
// 修「缺成功列」最省事的错法是"把三列一起加回来"——
// 而那会破坏用户更早的明确要求（删掉熔断/在途）。
// 两条断言一起，才把"加一列"与"别加两列"同时钉住。
func TestBreakerAndInFlightStayHidden(t *testing.T) {
	for _, u := range allUpstreams(t) {
		if u.p == nil {
			continue
		}
		cols := columnsOf(u.p)
		for _, banned := range []string{gateway.AccountColBreaker, gateway.AccountColInFlight} {
			if contains(cols, banned) {
				t.Errorf("上游 %s 回归了 %q 列 —— 用户明确要求账号池不显示"+
					"熔断/在途（排障计数器，几乎不变却占两列宽）。\n"+
					"当前列集: %v", u.label, banned, cols)
			}
		}
	}
}

// 列集里**不能有未知 id**（拼错列名是静默失效）。
//
// 加这条是因为我自己犯过的错：`AccountColumns()` 曾经回过中文标题
// （`[]string{"通道","平台"}`）—— 那不是 id。核心会记一行日志、
// 前端跳过该列，于是**那一列直接消失**，而界面上看不出是漏了还是本来没有。
//
// 所以在跨上游这一层再拦一道：任何上游报出规范词汇表之外的 id 都是错的。
func TestAllReportedColumnsAreCanonical(t *testing.T) {
	for _, u := range allUpstreams(t) {
		if u.p == nil {
			continue
		}
		for _, c := range columnsOf(u.p) {
			if !gateway.IsKnownAccountColumn(c) {
				t.Errorf("上游 %s 自报的列 id %q 不在规范词汇表里 ——\n"+
					"前端会跳过它（该列不显示），而且**不会报错**。\n"+
					"用 gateway.AccountCol* 常量，不要写字面量或中文标题。", u.label, c)
			}
		}
	}
}

// 反向断言：清单必须覆盖 internal/ 下的**每个**上游目录。
//
// # 为什么需要它
//
// 上面几条都只遍历 `allUpstreams()` 的**手写清单** —— 于是新增一个上游
// 时忘了登记，它就永远不会被检查，而测试全绿。
// 那正是 arch_test.go 反复踩过的"清单漏登记即失效"。
//
// 判据：internal/ 下每个目录都该被清单覆盖，或是明确的核心/工具包。
// 加新上游时忘登记 → 这条红。
func TestAllUpstreamsAreCovered(t *testing.T) {
	known := map[string]bool{}
	for _, u := range allUpstreams(t) {
		known[u.id] = true
	}

	// 不是上游的包（core + 工具包）。判据：**不实现 gateway.Provider**。
	// 与 gateway.arch_test 的 discoveryExemptPackages 同一精神，
	// 但这里是独立清单 —— 那个是 unexported，跨包拿不到。
	notUpstream := map[string]bool{
		"admin": true, "apikey": true, "auth": true, "browseropen": true,
		"checkinlog": true, "clientlogin": true, "dailycheckin": true,
		"gateway": true, "logbuf": true, "oauth": true, "pool": true,
		"prompt": true, "qrcode": true, "qoderwasm": true, "redisstore": true,
		"scheduler": true, "server": true, "session": true, "upstream": true,
		"wire": true,
	}

	entries, err := os.ReadDir("../../internal")
	if err != nil {
		t.Fatalf("读 internal/ 失败: %v", err)
	}
	var missing []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if notUpstream[name] || known[name] {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		t.Errorf("internal/ 下有 %d 个目录没被 allUpstreams() 覆盖: %v\n"+
			"两种情况：\n"+
			"  · 它是新上游 → 在 allUpstreams() 里登记\n"+
			"  · 它不是上游（core/工具包）→ 加到 notUpstream 里\n"+
			"不登记的话，「每个上游都要有成功列」这条断言**管不到它**，"+
			"而测试会全绿。", len(missing), missing)
	}
}
