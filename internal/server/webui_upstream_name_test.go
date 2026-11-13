package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestWebUIHasNoUpstreamNameBranch 钉住本项目最硬的那条判据：
//
//	**前端不得按上游名分流** —— 加第三个上游时前端 0 改动。
//
// # 为什么判据是"分支"而不是"出现过上游名"
//
// 第一版我写的是"剥注释后不许出现 codearts/workbuddy"，实测**121 处命中**，
// 但逐条看下来**没有一处是违规**：
//
//	3 处  静态面板的 data-provider="workbuddy"（改造前就写死的既有形态）
//	4 处  「WorkBuddy 客户端」—— 产品名文案（成长/本机登录的提示语）
//	其余  是我那个剥注释脚本自己造的假阳性：它把 `https://x` 里的 `//`
//	      当成注释起点，于是 URL 后半截被当成"代码"
//
// 一条守着 121 个假阳性的断言，下场一定是被人删掉。
// 而真正会让"加第三个上游要改前端"的形态只有一种：**拿上游名做比较/查表**。
// 所以判据落在那个形态上。
//
// # 为什么不留着"出现过名字"当参考
//
// 留了就会有人把它当硬判据，然后为了让它变绿去改产品文案 —— 那是本末倒置。
// 这里只钉真形态。
func TestWebUIHasNoUpstreamNameBranch(t *testing.T) {
	// ⚠★ 2026-11-13（T-911）：这条护栏里原有**两处假绿**，一起修掉。
	//
	//	① 名字表写死 codearts|workbuddy ⇒ 拿 phanthywork / loomy / trae / codar … 当分支条件时
	//	   **抓不到** —— "加第 N 个上游前端 0 改动"这条判据**只对那两个名字成立**。
	//	   现在名字表**从各上游包自己的源码扫**（upstreamIDsFromSources）：新增一个上游就自动覆盖。
	//	② 扫描对象原先只有 string(webuiHTML)（外壳）—— 前端拆包后 JS 在 ui/app.js，
	//	   于是那 13k 行**从来没被这条护栏扫过**（同一个坑 frontend_source_test.go:49 已记过一次）。
	//	   现在扫 frontendSourceString()（外壳 + ui/app.js）。
	//
	// ⚠ 判据形态**一处没改**：仍是那五种"拿名字做比较/查表/switch/字面量映射"的形态，
	//   只是名字表变全、扫描面变全 —— 原来那两处是**假绿**，不是"判据太严"。
	raw := frontendSourceString()
	if strings.TrimSpace(raw) == "" {
		t.Fatal("前端源码读到了空内容 —— 守卫失效（fail-open）")
	}
	// 非空匹配守卫：扫描对象接错（只拿到外壳、或只拿到 JS）时立刻红，别静默扫个寂寞。
	if !strings.Contains(raw, "function ") {
		t.Fatal("扫到的内容里没有 JS 函数 —— 扫描对象接错了（fail-open）")
	}
	code := stripJSCommentsForNames(raw)

	ids := upstreamIDsFromSources(t)
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, regexp.QuoteMeta(id))
	}
	alt := strings.Join(quoted, "|")
	t.Logf("名字表 = %d 个（扫自各上游包源码）：%v", len(ids), ids)

	// 违规形态：拿上游名（或 provider 字面量）做比较 / 查表 / switch 分派。
	patterns := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"与上游名字面量比较", regexp.MustCompile(`[=!]==?\s*['"](` + alt + `)['"]`)},
		{"按上游名查表", regexp.MustCompile(`\[\s*['"](` + alt + `)['"]\s*\]`)},
		{"按上游名 switch", regexp.MustCompile(`case\s+['"](` + alt + `)['"]`)},
		{"按 provider 字面量分流", regexp.MustCompile(`(provider|pid|providerOf\([^)]*\))\s*[=!]==?\s*['"][a-z]`)},
		{"按上游名建白名单/映射", regexp.MustCompile(`(` + alt + `)['"]\s*:`)},
	}

	var bad []string
	for i, line := range strings.Split(code, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, p := range patterns {
			if p.re.MatchString(line) {
				bad = append(bad, p.name+" @ 行 "+itoa(i+1)+": "+strings.TrimSpace(line))
				break
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("前端出现了**按上游名分流**的代码（加第三个上游时前端就不再是 0 改动）：\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// upstreamIDFloor 名字表下限（fail-closed）：扫不到足够多的 id 就 Fatal ——
// 名字表变空 = 这条护栏变成**永远绿**，比没有护栏更坏。
const upstreamIDFloor = 10

// upstreamIDsFromSources 扫各上游包自己声明的 provider id。
//
// # 为什么用"扫源码"而不是从注册表取（T-911 的选择与理由）
//
// 注册表在 cmd/server 装配（测试进程里拿不到那份完整注册），而 provider id 在每个包自己的源码里
// 是**唯一声明**：const providerID = "..."（多数包还导出 ProviderID = providerID）。
// 扫它有两个好处：
//
//	· 新增一个上游 ⇒ 名字表自动多一项，本文件**不用改**（这正是 T-911 要的效果）；
//	· 与这条护栏的既有形态一致（同文件本来就是"读前端源码"做判据）。
//
// ⚠ 代价（如实写在这儿）：哪天有人改用别的写法声明 id（例如从配置读），这里会扫不到 ——
//
//	所以下面有下限 + 两个历史 id 的存在性检查，扫不到就 Fatal，绝不静默变绿。
//
// ⚠ 只把 id 当**名字表**用，不断言"前端必须出现它"：前端合法地提到上游名（面板锚点、产品文案）
//
//	很正常，那会造成"删一句文案就红"的假红。
func upstreamIDsFromSources(t *testing.T) []string {
	t.Helper()
	// ⚠ 大小写都要认：[Pp]roviderID —— 本仓两种写法都有（minimax/monkeycode 用导出的 `const ProviderID = "x"`，
	//   其余包用包内 `const providerID = "x"`）。第一版只认小写 ⇒ 少扫到 2 个 id（PowerShell 那次是**大小写不敏感**，
	//   所以核对时看着是齐的）。这里只是"名字表要尽量全"，不是判据本身；漏名字=漏覆盖。
	re := regexp.MustCompile(`(?m)^[ \t]*(?:const[ \t]+)?[Pp]roviderID[ \t]*=[ \t]*"([a-z0-9][a-z0-9_-]*)"`)
	seen := map[string]bool{}
	var ids []string
	roots := []string{filepath.Join("..", "..", "internal"), filepath.Join("..", "..", "cmd")}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				if !seen[m[1]] {
					seen[m[1]] = true
					ids = append(ids, m[1])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫 %s 失败: %v（名字表取不到 ⇒ 这条护栏会变成永远绿）", root, err)
		}
	}
	sort.Strings(ids)

	// fail-closed 保险：下限 + 两个历史 id 必须在场。
	if len(ids) < upstreamIDFloor {
		t.Fatalf("只扫到 %d 个上游 id（%v）—— 低于下限 %d：扫描逻辑坏了，名字表会变空 ⇒ 护栏永远绿",
			len(ids), ids, upstreamIDFloor)
	}
	for _, must := range []string{"codearts", "workbuddy"} {
		if !seen[must] {
			t.Fatalf("名字表里没有 %q —— 它曾是这份表的全部；丢了说明扫描逻辑坏了（fail-closed）", must)
		}
	}
	return ids
}

// stripJSCommentsForNames 剥掉注释，但**不碰 URL 里的 `//`**。
//
// ⚠ 这一点是实测踩出来的：早先版本用 `l[:idx]`（第一个 `//`）剥注释，
// 于是 `https://example/ui` 被截成 `https:` —— 后半截当成代码参与匹配。
// 121 处假阳性里绝大多数是这么来的。
func stripJSCommentsForNames(src string) string {
	lf := strings.ReplaceAll(src, "\r\n", "\n")
	lf = strings.ReplaceAll(lf, "\r", "\n")

	// HTML 注释
	lf = regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(lf, "")
	// 块注释
	lf = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(lf, "")

	var out []string
	for _, line := range strings.Split(lf, "\n") {
		cut := -1
		for k := 0; k+1 < len(line); k++ {
			if line[k] == '/' && line[k+1] == '/' {
				// `://` 是 URL，不是注释起点
				if k > 0 && line[k-1] == ':' {
					continue
				}
				cut = k
				break
			}
		}
		if cut >= 0 {
			line = line[:cut]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
