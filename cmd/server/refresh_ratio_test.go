// refresh_ratio_test.go 跨上游不变量：**提前续期窗口必须按「寿命过半」算**。
//
// # 这条守卫是用户要求的（2026-11-01）
//
// 「我要我们现在自动续期都是统一一个逻辑，就是它的生命周期到达了 50% 就续期。」
//
// 共享判据早就在 `internal/gateway/refreshratio.go`（DefaultRefreshRatio = 0.5），
// 但当时只有一部分上游用了它，另一些还各自写着固定分钟：
//
//	workbuddy  10m（55 天寿命 ⇒ 已用 99.99% 才续，等于没有提前量）
//	minimax    自己抄了一份 life/2（数值巧合相同，判据却分散）
//	codearts   3m 常量（不透明 token，没有 iat）
//	trae       (0,true)（因为投影 ExpiresAt=0 会让"该刷了吗"恒真，改成不预检）
//
// 现在四处都并入同一条判据（见各自的文件注释），本文件把"以后不许再分叉"钉住：
//
//	① 行为：能造出凭证的三个上游，报出的窗口 == RefreshSkewAtRatio(times, 0.5)
//	② 源码：**任何**实现 RefreshSkew 并可能回正数窗口的包，都必须调用那两个共享函数
//	③ 例外必须**写明理由**，而且不许"过期"（理由消失后条目还在 = 红）
//
// ⚠ 为什么源码级也要有：行为级只能覆盖"我在测试里造得出凭证"的上游（3 个），
// 而漏掉一家写死常量的上游**不会报任何错** —— 那正是本次要防的形态。
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/minimax"
	"workbuddy2api/internal/trae"
)

// unsignedJWT 造一个只带 iat/exp 的 JWT（不签名 —— 这里只读时间戳）。
func unsignedJWT(iat, exp int64) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"iat":%d,"exp":%d}`, iat, exp)))
	return h + "." + p + ".sig"
}

// TestRefreshSkewUsesSharedRatio 行为：报告值 == 共享判据算出来的值。
func TestRefreshSkewUsesSharedRatio(t *testing.T) {
	now := time.Now().Unix()
	// 寿命 1 小时、已过半 ⇒ 窗口 30 分钟（三个上游同一期望值）。
	token := unsignedJWT(now-1800, now+1800)
	want := 30 * time.Minute

	for _, c := range []struct {
		name string
		cred gateway.Credential
	}{
		{"workbuddy", gateway.Credential{Provider: "workbuddy", UID: "ratio", Secret: &auth.Auth{UID: "ratio", AccessToken: token}}},
		{"trae", gateway.Credential{Provider: "trae", UID: "ratio", Secret: &trae.Auth{AccessToken: token, RefreshToken: "rt"}}},
		{"minimax", gateway.Credential{Provider: "minimax", UID: "ratio", Secret: &minimax.Auth{
			AccessToken: "t", RefreshToken: "r",
			IssuedAt:  fmt.Sprintf("%d", (now-1800)*1000),
			ExpiresAt: fmt.Sprintf("%d", (now+1800)*1000),
		}}},
	} {
		var p gateway.Provider
		for _, u := range allUpstreams(t) {
			if u.id == c.name {
				p = u.p
			}
		}
		if p == nil {
			t.Fatalf("%s: 装配层里没有这个上游", c.name)
		}
		skewExt, has := gateway.ExtOf[gateway.RefreshSkewExt](p)
		if !has {
			t.Fatalf("%s: 没有实现 RefreshSkewExt", c.name)
		}
		skew, ok2 := skewExt.RefreshSkew(c.cred)
		if !ok2 {
			t.Errorf("%s: 对可算寿命的凭证回了 ok=false（应当能算出比例窗口）", c.name)
			continue
		}
		if skew != want {
			t.Errorf("%s: 窗口 = %v，want %v（寿命 1 小时的一半）。\n"+
				"统一判据是 gateway.DefaultRefreshRatio(0.5)：已用寿命 ≥ 50%% 就续期。", c.name, skew, want)
		}
	}
}

// mustGetwd 当前目录（只为把失败信息写具体）。
func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "?"
	}
	return wd
}

// ratioHelperNames 共享判据的两个入口（上游只能通过它们表达"寿命过半"）。
var ratioHelperNames = []string{"RefreshSkewFromToken", "RefreshSkewAtRatio", "DefaultRefreshRatio"}

// ratioExceptions 允许**不**按比例报窗口的包 —— 每一项都必须给出理由。
//
// 判据：它在实现 RefreshSkew，但**没有**可算的寿命（或明确不需要预检）。
var ratioExceptions = map[string]string{
	"zcode": "JWT 通道官方没有 refresh 接口（刷不了）⇒ 恒回 (0,true) 明确声明不预检；" +
		"回 (0,false) 会让核心套用自己的兜底窗口，等于没关掉那条路径（见该文件的长注释）",
}

// TestRefreshSkewHelpersAreUsedEverywhere 源码：不许再有自己写死常量的窗口。
func TestRefreshSkewHelpersAreUsedEverywhere(t *testing.T) {
	// ⚠ 路径要能站住：go test 的 cwd 是**包目录**（cmd/server），但被 -C 或别的
	//   调用方式改动时不保证。所以逐个候选试，并明确报出"一个都没找到"——
	//   静默跳过会让这条守卫变成装饰品（本仓反复吃过这个形态的亏）。
	root := internalRoot(t)
	found := map[string]bool{}
	deviation := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		if !strings.Contains(src, "func (p *Provider) RefreshSkew(") {
			return nil
		}
		pkg := filepath.Base(filepath.Dir(path))
		found[pkg] = true
		for _, h := range ratioHelperNames {
			if strings.Contains(src, h) {
				return nil
			}
		}
		deviation[pkg] = filepath.ToSlash(path)
		return nil
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(found) < 6 {
		t.Errorf("只扫到 %d 个实现了 RefreshSkew 的包 —— 扫描本身失效了（守卫会假绿）", len(found))
	}
	for pkg, path := range deviation {
		if reason, ok := ratioExceptions[pkg]; ok {
			t.Logf("%s: 例外（%s）—— %s", pkg, reason, path)
			continue
		}
		t.Errorf("%s 自己实现了一个续期窗口，但**没有**用共享判据（%s）。\n"+
			"用户要求所有上游统一成「生命周期过半就续期」：\n"+
			"  · 能算寿命 → gateway.RefreshSkewFromToken(token) 或 RefreshSkewAtRatio(times, DefaultRefreshRatio)\n"+
			"  · 算不出来 → 用 ok=false 让核心回落，或用 (0,true) 明确声明不预检\n"+
			"  · 真的是例外 → 加进 ratioExceptions 并**写清理由**", pkg, path)
	}
	// 例外表不许"过期"：理由消失（包已并入共享判据）后条目还留着 = 红。
	for pkg, reason := range ratioExceptions {
		if _, still := deviation[pkg]; !still {
			t.Errorf("ratioExceptions 里的 %s 已经不再偏离共享判据（理由：%s）—— "+
				"把它从例外表里删掉：那份表应当是当前例外的清单，不是历史遗留。",
				pkg, reason)
		}
	}
}

// internalRoot 找到 internal/ 目录（源码级守卫都要扫它）。
//
// ⚠ 逐个候选试，一个都找不到就报错：静默跳过会让整组源码守卫退化成装饰品。
func internalRoot(t *testing.T) string {
	t.Helper()
	for _, cand := range []string{
		filepath.Join("..", "internal"),
		filepath.Join(".", "internal"),
		filepath.Join("..", "..", "internal"),
	} {
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
	}
	t.Fatalf("找不到 internal/ 目录（cwd=%s）", mustGetwd())
	return ""
}

// funcBody 用**大括号配对**取函数体。
//
// ⚠ 不能用非贪婪正则：它会在正文里第一个右花括号处停下，把后面的函数截断或吞掉
// （本仓在 tools/toast_sync.js 上真实踩过，那次把 quotaScopeToast 整块删掉了）。
func funcBody(src, header string) (string, bool) {
	at := strings.Index(src, header)
	if at < 0 {
		return "", false
	}
	open := strings.Index(src[at:], "{")
	if open < 0 {
		return "", false
	}
	depth := 0
	for i := at + open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[at : i+1], true
			}
		}
	}
	return "", false
}

// consultsRatio 报告这段函数体是否**问过统一判据**（允许一层委托）。
//
// # 为什么要跟一层委托
//
// 第一版只看入口函数体，于是 qoder 被误报：它的 runRefresh 把"该不该刷"交给同包的
// `p.shouldRefresh(a, now)`，判据在那个函数里。误报的代价不是"漏"而是"假红" ——
// 假红同样会让守卫被绕过（有人会去把这项从表里删掉）。
//
// 所以：入口自己问过 ⇒ 合格；否则看它调用的 `p.<name>(` 里有没有问过。只跟一层，
// 够覆盖"决策函数独立"这种常见形状，又不至于退化成"整包搜一遍"那种等于没查的判据。
func consultsRatio(src, body string) bool {
	if hasRatioMarker(body) {
		return true
	}
	for _, name := range calledProviderFuncs(body) {
		if inner, ok := funcBody(src, "func (p *Provider) "+name+"("); ok && hasRatioMarker(inner) {
			return true
		}
	}
	return false
}

// hasRatioMarker 这段代码里是否出现统一判据的入口。
func hasRatioMarker(code string) bool {
	for _, m := range append(append([]string{}, ratioHelperNames...), "dueForRefreshByRatio", "refreshSkewOf") {
		if strings.Contains(code, m) {
			return true
		}
	}
	return false
}

// calledProviderFuncs 从函数体里抽出 `p.foo(` 形态的被调用方法名（去重）。
func calledProviderFuncs(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, seg := range strings.Split(body, "p.") {
		if seg == "" {
			continue
		}
		i := 0
		for i < len(seg) && (seg[i] == '_' || seg[i] >= 'a' && seg[i] <= 'z' || seg[i] >= 'A' && seg[i] <= 'Z' || seg[i] >= '0' && seg[i] <= '9') {
			i++
		}
		if i == 0 || i >= len(seg) || seg[i] != '(' {
			continue
		}
		name := seg[:i]
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// bulkRefreshEntryPoints 批量续期入口 —— 这些函数必须**问过统一判据**才算合格。
//
// # 为什么后台入口也要守
//
// 用户要求统一续期判据时实测到的形态：请求路径已经按"寿命过半"报窗口，
// 而后台任务里还写着"无条件全量续"（codearts / workbuddy）或"固定 30 分钟窗口"（trae）——
// 同一个上游两套判据，**改一处漏一处不会报错**：改了请求路径、后台照旧把号全量刷一遍。
//
// ⚠ 入口改名/移动时必须同步改这张表；文件或函数找不到会**报错**，
//
//	不让守卫变成"看起来还在、其实什么都没查"。
var bulkRefreshEntryPoints = []struct{ file, header, what string }{
	{"internal/codearts/jobs.go", "func (p *Provider) runRefresh(ctx context.Context) error {", "codearts 后台续期"},
	{"internal/trae/extensions.go", "func (p *Provider) runRefresh(ctx context.Context) error {", "trae 后台续期"},
	{"internal/workbuddy/checkin.go", "func (p *Provider) RunKeepaliveAll(trigger string) {", "workbuddy 后台保活"},
	{"internal/cline/jobs.go", "func (p *Provider) runRefresh(ctx context.Context) error {", "cline 后台续期"},
	{"internal/raccoon/jobs.go", "func (p *Provider) runRefresh(ctx context.Context) error {", "raccoon 后台续期"},
	{"internal/qoder/jobs_refresh.go", "func (p *Provider) runRefresh(ctx context.Context) error {", "qoder 后台续期"},
}

// TestBulkRefreshEntryPointsConsultRatio 后台批量续期也必须走统一判据。
func TestBulkRefreshEntryPointsConsultRatio(t *testing.T) {
	root := internalRoot(t)
	for _, e := range bulkRefreshEntryPoints {
		rel := strings.TrimPrefix(filepath.ToSlash(e.file), "internal/")
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("%s 读不到（%v）—— 文件改名/移动了？把这行改成新路径，别让它静默失效", e.file, err)
			continue
		}
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		body, ok := funcBody(src, e.header)
		if !ok {
			t.Errorf("%s 里找不到入口 `%s` —— 改名了就同步改掉这张表"+
				"（否则这条守卫看起来还在、其实什么都没查）", e.what, e.header)
			continue
		}
		if !consultsRatio(src, body) {
			t.Errorf("%s 没有问过统一判据（%v）—— 它会按自己的节奏把账号全量刷一遍。\n"+
				"用户要求所有上游统一成「寿命过半就续期」：请求路径与后台任务必须共用同一份窗口计算。",
				e.what, ratioHelperNames)
		}
	}
}
