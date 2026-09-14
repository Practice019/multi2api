package server

import (
	"regexp"
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
	raw := string(webuiHTML)
	if strings.TrimSpace(raw) == "" {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}
	code := stripJSCommentsForNames(raw)

	// 违规形态：拿上游名（或 provider 字面量）做比较 / 查表 / switch 分派。
	patterns := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"与上游名字面量比较", regexp.MustCompile(`[=!]==?\s*['"](codearts|workbuddy)['"]`)},
		{"按上游名查表", regexp.MustCompile(`\[\s*['"](codearts|workbuddy)['"]\s*\]`)},
		{"按上游名 switch", regexp.MustCompile(`case\s+['"](codearts|workbuddy)['"]`)},
		{"按 provider 字面量分流", regexp.MustCompile(`(provider|pid|providerOf\([^)]*\))\s*[=!]==?\s*['"][a-z]`)},
		{"按上游名建白名单/映射", regexp.MustCompile(`(codearts|workbuddy)['"]\s*:`)},
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
