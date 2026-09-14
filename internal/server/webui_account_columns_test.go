package server

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// 本文件把 workbuddy 的 11 列表头**逐字**钉死，并把它与后端契约焊在一起。
//
// # 为什么必须有这条守卫（评审指出的"循环论证"）
//
// 用户的原话是「workbuddy 直接复用现在的标题就可以了」—— 也就是这 11 列
// **逐字不变**。但改造后守卫是这样分布的：
//
//	· internal/gateway/account_columns_test.go 钉住后端那份 11 个 id
//	· tests/frontend/accounts_e2e.js 的期望值取自**被测页面自己的**
//	  `DEFAULT_ACCT_COLUMNS` 常量
//
// 于是两半各测一半，中间那条"前端必须与后端一致"的接缝**没人守**：
// 把 webui.html 里的 `'token'` 改成 `'token_expiry'`、或删掉一列，浏览器断言
// 依旧全绿（它照着自己抄了一遍），Go 断言也照样绿（它根本不读前端）。
// 实测确认：改坏前端那 11 列后，当年那条 `ths === 11` 反而**会红** ——
// 也就是说 T5 的改写在这一处比被替换掉的旧断言更弱。
//
// 更要命的是 webui.html 里已经写着"顺序与内容必须与 gateway.DefaultAccountColumns()
// 逐列一致"——**一条没有人执行的声明**。声明不会自己成立，这条测试才是它的执行者。
//
// # 为什么标题要硬编码在这里，而不是从别处取
//
// 期望值一旦从被测对象派生，断言就退化成恒真。这里宁可让"改文案"同时改测试：
// 那正是"逐字不变"该有的摩擦 —— 用户能看见的东西改了，本来就应该有人确认一次。

// webuiDefaultAcctColumns 抽 `const DEFAULT_ACCT_COLUMNS = [...]` 里的列 id。
func webuiDefaultAcctColumns(t *testing.T, src string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)const DEFAULT_ACCT_COLUMNS\s*=\s*\[(.*?)\]`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("webui.html 里找不到 DEFAULT_ACCT_COLUMNS —— 守卫失效（fail-open）")
	}
	return quotedStrings(m[1])
}

// quotedStrings 取出片段里的所有单引号字符串（按出现顺序）。
func quotedStrings(s string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// webuiColumnTitle 取 `ACCT_COLUMN_DEFS` 里某个列 id 的 title。
//
// 先定位 `<id>: {`，再把窗口截到**下一个 4 空格缩进的键**（或对象结尾），
// 这样多行条目（token_expiry / welfare 带 tip）也不会串到下一列去。
func webuiColumnTitle(t *testing.T, src, id string) string {
	t.Helper()
	loc := regexp.MustCompile(`(?m)^\s{2,6}` + regexp.QuoteMeta(id) + `\s*:\s*\{`).FindStringIndex(src)
	if loc == nil {
		t.Fatalf("ACCT_COLUMN_DEFS 里没有列 %q 的定义 —— 回落到这一列时会渲染不出表头", id)
	}
	rest := src[loc[1]:]
	if nxt := regexp.MustCompile(`(?m)^    \w+\s*:`).FindStringIndex(rest); nxt != nil {
		rest = rest[:nxt[0]]
	} else if i := strings.Index(rest, "\n  };"); i >= 0 {
		rest = rest[:i]
	}
	tm := regexp.MustCompile(`title:\s*'([^']*)'`).FindStringSubmatch(rest)
	if tm == nil {
		t.Fatalf("列 %q 的定义里没有 title:", id)
	}
	return tm[1]
}

// TestWebUIDefaultAcctColumnsMatchGatewayContract 钉住前端回落列集 == 后端契约。
//
// workbuddy **不实现** AccountColumnsExt（后端零改动）→ 它走的就是这条回落。
// 两边一分叉，workbuddy 的表头就与后端契约悄悄错位：前端会去要一个
// 后端没有的列，或漏掉一个后端给了数据的列。
func TestWebUIDefaultAcctColumnsMatchGatewayContract(t *testing.T) {
	src := string(webuiHTML)
	if strings.TrimSpace(src) == "" {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}

	got := webuiDefaultAcctColumns(t, src)
	want := gateway.DefaultAccountColumns()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("前端回落列集与后端契约分叉 —— workbuddy 的表头会错位\n"+
			"  webui.html DEFAULT_ACCT_COLUMNS : %v\n"+
			"  gateway.DefaultAccountColumns() : %v\n"+
			"（顺序也算：顺序 = 表头顺序）", got, want)
	}
}

// TestWebUIWorkbuddyHeadersAreVerbatim 把 workbuddy 那 11 个**表头文字**逐字钉死。
//
// 这是用户那句「workbuddy 直接复用现在的标题」的直接编码。左边是列 id、
// 右边是**硬编码**的期望标题 —— 改任何一个字，这条都会红，改的人必须
// 显式改掉期望值，也就必须承认"我改了用户看得见的东西"。
func TestWebUIWorkbuddyHeadersAreVerbatim(t *testing.T) {
	src := string(webuiHTML)

	// 期望值**硬编码**（不从 webui.html 里抽）—— 见文件头注释。
	wantTitles := []string{
		"上游", "昵称", "UID", "额度", "状态", "Token",
		"今日签到", "成功", "熔断", "在途", "操作",
	}
	ids := gateway.DefaultAccountColumns()
	if len(ids) != len(wantTitles) {
		t.Fatalf("夹具失效：后端契约有 %d 列，期望标题有 %d 个", len(ids), len(wantTitles))
	}

	for i, id := range ids {
		if got := webuiColumnTitle(t, src, id); got != wantTitles[i] {
			t.Errorf("workbuddy 第 %d 列表头被改了（列 %q）：\n  实际 = %q\n  期望 = %q\n"+
				"用户要求这 11 列表头逐字不变；确实要改就同步改掉本测试的期望值",
				i+1, id, got, wantTitles[i])
		}
	}
}

// TestWebUIEveryContractColumnHasDefinition 钉住"契约里的每一列前端都认识"。
//
// 少一个定义，回落时那一列的表头就是空的（而 e2e 只比列数与顺序，可能看不出来）。
func TestWebUIEveryContractColumnHasDefinition(t *testing.T) {
	src := string(webuiHTML)
	for _, id := range gateway.DefaultAccountColumns() {
		if got := webuiColumnTitle(t, src, id); strings.TrimSpace(got) == "" {
			t.Errorf("列 %q 在 ACCT_COLUMN_DEFS 里没有非空 title —— 回落时表头会是空白", id)
		}
	}
}
