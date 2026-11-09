package logbuf

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shortUIDTable 各上游 uid 的**真实形状**（不是编的）。
//
// ⚠ 跨上游**允许**压缩后相同（前缀已经把上游区分开了）；**同一上游内必须不同**。
// 所以下面按上游分组断言，而不是把 29 条混在一起比唯一性。
var shortUIDTable = []struct {
	upstream string
	uid      string
}{
	{"loomy", "loomy-260915174611208423"},
	{"loomy", "loomy-260915174611301268"},
	{"loomy", "loomy-260915174611104551"},
	{"loomy", "loomy-260915174611409672"},
	{"loomy", "loomy-260915174611502880"},
	{"loomy", "loomy-260927001031181940"},
	{"monkeycode", "monkeycode-002d3463-53d0-4a11-9c1a-77e01b2c8f33"},
	{"monkeycode", "monkeycode-002d3463-53d0-4a11-9c1a-77e01b2c8f44"},
	{"monkeycode", "monkeycode-002d3463-53d0-4a11-9c1a-88f01b2c8f55"},
	{"monkeycode", "monkeycode-002d3464-11ab-4c22-bd33-99f01b2c8f66"},
	{"zcode", "zcode-79e98e60-5be9-4f21-8a77-112233445566"},
	{"zcode", "zcode-79e98e60-5be9-4f21-8a77-112233445577"},
	{"zcode", "zcode-79e98e61-6cf0-5a32-9b88-223344556688"},
	{"codar", "user_15083583411_7985"},
	{"codar", "user_15083583411_7986"},
	{"codar", "user_15083583411_8123"},
	{"codar", "user_15083583412_7985"},
	{"trae", "3929003642848586"},
	{"trae", "3929003642848587"},
	{"trae", "3929003642849001"},
	{"trae", "3929003642851234"},
	{"trae", "3929003643000001"},
	{"cline", "usr-01M3MJDCGSD05W4Y83JQC71XCT"},
	{"cline", "usr-01M3MJDCGSD05W4Y83JQC71XCU"},
	{"cline", "usr-01M3MJDCGSD05W4Y83JQD82YDT"},
	{"cline", "usr-01M3MJDCGSD05W4Y83JQE93ZEU"},
	{"workbuddy", "0234c548-963c-4d43-8a11-112233445566"},
	{"workbuddy", "0234c548-963c-4d43-8a11-112233445577"},
	{"workbuddy", "0234c548-963c-4d43-8a11-998877665544"},
	// 短 id（lobsterai / raccoon）—— ≤12 必须原样，且不参与"压缩"比较之外的特殊处理
	{"lobsterai", "103668"},
	{"raccoon", "7497524"},
}

// TestPrintShortUIDTable 直出压缩对照表（T-404）。
//
// ⚠ 用 fmt.Println 而不是 t.Logf 的原因：t.Logf 的输出带缩进与时间戳，
// 用脚本抓取容易失败 —— 上一轮就是这么把表格弄丢的（所以这条单独存在）。
// 运行：go test ./internal/logbuf/ -run TestPrintShortUIDTable -v
func TestPrintShortUIDTable(t *testing.T) {
	fmt.Println("=== uid 压缩表（T-403 规则：前 head 位 + … + 尾部 8 位；有分隔符 head=6，无分隔符 head=4）===")
	fmt.Printf("%-46s %-22s %s\n", "原值", "压缩后", "长度")
	for _, e := range shortUIDTable {
		s := ShortUID(e.uid)
		mark := ""
		if s == e.uid {
			mark = "（≤12 原样）"
		}
		fmt.Printf("%-46s %-22s %2d %s\n", e.uid, s, len([]rune(s)), mark)
	}
	// 碰撞汇总
	fmt.Println("=== 同上游碰撞检查 ===")
	byUp := map[string]map[string]string{}
	for _, e := range shortUIDTable {
		if byUp[e.upstream] == nil {
			byUp[e.upstream] = map[string]string{}
		}
		s := ShortUID(e.uid)
		if prev, dup := byUp[e.upstream][s]; dup {
			fmt.Printf("  [%s] ✗ 碰撞 %q 与 %q → %q\n", e.upstream, prev, e.uid, s)
		}
		byUp[e.upstream][s] = e.uid
	}
	for up, m := range byUp {
		fmt.Printf("  [%-11s] %d 个号 → %d 个不同压缩值\n", up, 0, len(m))
	}
}

// TestShortUIDDistinguishesWithinUpstream seam①：同一上游压缩后两两不同。
func TestShortUIDDistinguishesWithinUpstream(t *testing.T) {
	byUp := map[string]map[string]string{}
	for _, e := range shortUIDTable {
		if byUp[e.upstream] == nil {
			byUp[e.upstream] = map[string]string{}
		}
		s := ShortUID(e.uid)
		if prev, dup := byUp[e.upstream][s]; dup {
			t.Errorf("[%s] ShortUID 碰撞：%q 与 %q 都压成 %q —— 日志里无法区分是哪个号",
				e.upstream, prev, e.uid, s)
		}
		byUp[e.upstream][s] = e.uid
	}
	if len(byUp) < 7 {
		t.Errorf("只覆盖了 %d 个上游（应 ≥7）", len(byUp))
	}
	if len(shortUIDTable) < 29 {
		t.Errorf("样本只有 %d 个（应 ≥29）", len(shortUIDTable))
	}
	// 长 id 必须被压缩（否则这条断言等于没测）
	long := ShortUID("loomy-260915174611208423")
	if long == "loomy-260915174611208423" {
		t.Error("长 uid 没有被压缩")
	}
	// 尾部 8 位必须可见（尾部才是区分位）
	if !strings.HasSuffix(long, "11208423") {
		t.Errorf("压缩后看不到尾部 8 位：%q", long)
	}
}

// TestShortUIDShortIDsPassThrough seam②：≤12 原样返回。
func TestShortUIDShortIDsPassThrough(t *testing.T) {
	for _, u := range []string{"103668", "7497524", "abc", "", "abcdefghijkl", "loomy-260915"} {
		if got := ShortUID(u); got != u {
			t.Errorf("短 uid（≤12）被改动了：%q → %q —— 截它只会丢掉全部信息", u, got)
		}
	}
}

// TestShortUIDHasExactlyOneImplementation seam③：全仓**只有一处**实现（T-404）。
//
// # 为什么要跨包扫源码
//
// T-403 之前有 **8 份**几乎逐字相同的 `func shortUID`（cmd/server + 7 个上游包），
// 每份都"取前 8 位"，在 loomy 的 44 个号上全撞成 `loomy-26`。
// 共享化之后必须有一条**会失败的命令**守住它 —— 否则下一个人加包时又会顺手复制一份
// （本仓已有 8 次先例，这不是推测）。
//
// 判据：全仓 `^func shortUID\(` 与 `^func ShortUID\(` 的出现次数**必须 == 1**，且那一处是 logbuf。
// ⚠ 大小写敏感 + 跳过 *_test.go（本文件自己就含这两个正则字面量，不跳会自匹配）。
func TestShortUIDHasExactlyOneImplementation(t *testing.T) {
	root := moduleRoot(t)
	var hits []string
	reLower := regexp.MustCompile(`(?m)^func shortUID\(`)
	reUpper := regexp.MustCompile(`(?m)^func ShortUID\(`)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "bin" || base == "node_modules" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		src := string(b)
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if reLower.MatchString(src) {
			hits = append(hits, "LOWER|"+rel)
		}
		if reUpper.MatchString(src) {
			// ⚠ 允许 internal/gateway 的**转发**（T-403 已批准：7 个上游包的 60+ 调用点不必改名）。
			//   但转发**必须真的转发**到 logbuf —— 否则它就悄悄变成第二份实现。
			if !strings.HasSuffix(rel, "internal/logbuf/shortuid.go") && !strings.Contains(src, "logbuf.ShortUID(") {
				t.Errorf("%s 里定义了 func ShortUID( 但**没有**转发到 logbuf.ShortUID "+
					"—— 那是第二份实现（T-403 收敛掉的 8 份复制品不许回流）", rel)
			}
			hits = append(hits, "UPPER|"+rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历仓库失败：%v", err)
	}
	var lower, upper []string
	for _, h := range hits {
		if strings.HasPrefix(h, "LOWER|") {
			lower = append(lower, strings.TrimPrefix(h, "LOWER|"))
		} else {
			upper = append(upper, strings.TrimPrefix(h, "UPPER|"))
		}
	}
	// ① 私有复制品（小写 func shortUID）必须**一个都没有**。
	//    这正是 T-403 之前 8 份复制品的形态 —— 也是变异验证里我插回去的那种。
	if len(lower) != 0 {
		t.Errorf("仍有 %d 份私有 shortUID 复制品：%v\n"+
			"  ⇒ 必须删掉、改用 logbuf.ShortUID（T-403 收敛掉的正是 8 份这种复制品）", len(lower), lower)
	}
	// ② `func ShortUID(` 的定义**恰好 2 处**：logbuf 的实现 + gateway 的转发（都经批准）。
	if len(upper) != 2 {
		t.Errorf("func ShortUID( 的定义处 = %d（%v）—— 应为 **2**：\n"+
			"  · internal/logbuf/shortuid.go（唯一实现）\n"+
			"  · internal/gateway/shortuid.go（3 行转发，已批准）", len(upper), upper)
		return
	}
	foundImpl := false
	for _, f := range upper {
		if f == "internal/logbuf/shortuid.go" {
			foundImpl = true
		}
	}
	if !foundImpl {
		t.Errorf("唯一实现不在 internal/logbuf/shortuid.go 里：%v", upper)
	}
}

// moduleRoot 从本包目录往上找到含 go.mod 的目录（测试的 CWD 是包目录）。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败：%v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("从 %s 往上找不到 go.mod", dir)
	return ""
}
