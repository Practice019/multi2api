// logstyle 全仓日志规范的**棘轮门禁**。
//
// # 为什么是棘轮，不是"一次全改"
//
// 本仓还有 ~500 处 `log.Printf("中文整句 %s", x)` 形态的历史日志
// （`cmd/server/main.go` 一个文件就 147 处）。一次性全改会引入大量无谓风险 ——
// 日志改动看着无害，但它是**排障的唯一线索**：改错一处，下次出事就查不出来。
//
// 所以用棘轮：
//
//	① 现状冻结进 baseline.json（按包统计违规数）
//	② 任一包的违规数**超过**基线 ⇒ 失败
//	③ 新增的包必须为 0
//
// 这样新代码写不合规立刻红，历史遗留可以分批收敛，
// 且**基线只准降**——改完一批就 `-update` 收紧一次。
//
// # 规范正文
//
// 见 `docs/log-style.md`。这里只做机器判定，不重复解释"为什么"。
package logstyle

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "重写 baseline.json（只在完成一批改造后使用；基线只准降）")

// baselinePath 基线文件。
const baselinePath = "baseline.json"

// baseline 包 → 允许的违规数。
type baseline map[string]int

// logCall 匹配所有日志调用点（`log.Printf` 与 `logx.*`）。
var logCall = regexp.MustCompile(`\b(?:log\.(?:Printf|Println|Fatalf|Panicf)|logx\.(?:Infof|Warnf|Errorf|Debugf|Logf|Event|Eventf|Coalesce))\s*\(`)

// hasCJK 判断是否含中日韩字符。
var hasCJK = regexp.MustCompile(`[\x{4e00}-\x{9fff}]`)

// msgFieldCall `logx.Event` / `Eventf` / `Coalesce` 带了 msg=（允许 CJK 的唯一形态）。
var msgFieldCall = regexp.MustCompile(`"msg"`)

// structuredCall 已经结构化的调用（Event/Eventf/Coalesce/RefreshFail/RefreshSummary/Batch）。
var structuredCall = regexp.MustCompile(`logx\.(?:Event|Eventf|Coalesce|RefreshFail|RefreshSummary)\s*\(`)

// violation 一条违规。
type violation struct {
	File string
	Line int
	Kind string
	Text string
}

// scanFile 扫一个源文件里的违规。
//
// # 什么叫"违规"
//
//	V1 裸 log.Printf 里含 CJK 整句        ← 最该改的：整句中文，机器无法聚合
//	V2 logx.Infof/Warnf/Errorf 里含 CJK 且没有 msg=  ← 中文在值位上
//
// ⚠ **不算**违规：
//   - `logx.Event/Eventf/Coalesce`（结构化；Eventf 的 msg= 是允许 CJK 的位置）
//   - `logx.Batch` 系列
//   - 注释行
func scanFile(path string, src string) []violation {
	var out []violation
	lines := strings.Split(src, "\n")
	for i, raw := range lines {
		ln := strings.TrimSpace(raw)
		if ln == "" || strings.HasPrefix(ln, "//") {
			continue // 注释不算（本仓踩过没剥注释的坑）
		}
		if !logCall.MatchString(ln) {
			continue
		}
		if !hasCJK.MatchString(ln) {
			continue
		}
		// 结构化调用放行。
		if structuredCall.MatchString(ln) || msgFieldCall.MatchString(ln) {
			continue
		}
		kind := "V2"
		if strings.Contains(ln, "log.Printf") || strings.Contains(ln, "log.Println") {
			kind = "V1"
		}
		out = append(out, violation{File: path, Line: i + 1, Kind: kind, Text: ln})
	}
	return out
}

// collect 扫全仓，返回 包 → 违规列表。
func collect(t *testing.T) map[string][]violation {
	t.Helper()
	root := filepath.Join("..", "..")
	res := map[string][]violation{}
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			vs := scanFile(path, string(raw))
			if len(vs) == 0 {
				return nil
			}
			// 按包归组（用相对仓库根的目录路径作为 key）。
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			key := "./" + filepath.ToSlash(rel)
			res[key] = append(res[key], vs...)
			return nil
		})
		if err != nil {
			t.Fatalf("扫 %s 失败：%v", base, err)
		}
	}
	return res
}

// ★ ① 违规数不得超过基线（棘轮）。
func TestLogStyleRatchet(t *testing.T) {
	got := collect(t)
	cur := baseline{}
	for pkg, vs := range got {
		cur[pkg] = len(vs)
	}

	if *update {
		writeBaseline(t, cur)
		t.Log("已重写 baseline.json")
		return
	}

	base := readBaseline(t)

	var regressed []string
	for pkg, n := range cur {
		allow, ok := base[pkg]
		if !ok {
			// 新出现的包（基线里没有）必须为 0。
			if n > 0 {
				regressed = append(regressed, pkg+"：新包出现 "+itoa(n)+" 处违规（基线无此包，必须为 0）")
			}
			continue
		}
		if n > allow {
			regressed = append(regressed, pkg+"：违规 "+itoa(n)+" 处，基线 "+itoa(allow)+" 处（增加 "+itoa(n-allow)+"）")
		}
	}
	if len(regressed) > 0 {
		sort.Strings(regressed)
		t.Fatalf("★ 日志规范回归（规范见 docs/log-style.md）：\n  %s\n\n"+
			"改法：把中文整句换成 事件名 + 蛇形字段，中文只进 msg=。\n"+
			"例：log.Printf(\"loomy: 池已填充 %%d/%%d\", a, b)\n"+
			"  → logx.Event(logx.LevelInfo, \"loomy\", \"freepool.filled\", \"filled\", a, \"total\", b)",
			strings.Join(regressed, "\n  "))
	}
}

// ② 基线只准降：某个包已经改好了，就必须收紧基线。
//
// # 为什么这条重要
//
// 棘轮只拦"变差"。如果某包从 20 降到 0 却没人 `-update`，
// 基线里那个 20 就成了**永久豁免** —— 下次有人在那个包里新写 20 处违规，
// 门禁一声不响。所以"改善了却不收紧"本身就是缺陷。
func TestBaselineIsTight(t *testing.T) {
	if *update {
		t.Skip("正在更新基线")
	}
	got := collect(t)
	base := readBaseline(t)

	var slack []string
	for pkg, allow := range base {
		n := len(got[pkg])
		if n < allow {
			slack = append(slack, pkg+"：基线 "+itoa(allow)+"，实际 "+itoa(n)+"（富余 "+itoa(allow-n)+" 处可被新违规占用）")
		}
	}
	if len(slack) > 0 {
		sort.Strings(slack)
		t.Fatalf("★ 基线不紧（改善了就要收紧，否则基线变成永久豁免）：\n  %s\n\n"+
			"跑 `go test ./tests/logstyle -update` 收紧。",
			strings.Join(slack, "\n  "))
	}
}

// ③ 规范文档必须存在（防止"规则只在某人脑子里"）。
func TestStyleDocExists(t *testing.T) {
	p := filepath.Join("..", "..", "docs", "log-style.md")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("缺 docs/log-style.md：%v", err)
	}
	for _, want := range []string{"事件名", "蛇形", "msg=", "逐账号", "折叠"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("docs/log-style.md 缺少关键条目 %q —— 规范文档要能独立读懂", want)
		}
	}
}

// ---- 基线读写 ----

func readBaseline(t *testing.T) baseline {
	t.Helper()
	raw, err := os.ReadFile(baselinePath)
	if err != nil {
		if os.IsNotExist(err) {
			return baseline{}
		}
		t.Fatalf("读基线：%v", err)
	}
	var b baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("基线不是合法 JSON：%v", err)
	}
	return b
}

func writeBaseline(t *testing.T, b baseline) {
	t.Helper()
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatalf("序列化基线：%v", err)
	}
	if err := os.WriteFile(baselinePath, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("写基线：%v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
