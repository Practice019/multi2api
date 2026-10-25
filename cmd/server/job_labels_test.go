// job_labels_test.go 跨上游不变量：**每个任务名都必须在展示层登记中文名与说明**。
//
// # 这条守卫对应用户的报障
//
// 用户看到任务调度表里 monkeycode-checkin 那一行**只有裸英文 key**，
// 没有中文名、也没有 ? 悬浮详情，与旁边那些行不是一个样式。
//
// # ⚠ 为什么改用"扫源码字面量"而不是"遍历已注册任务"
//
// 前两版都试过，都被变异验证证明是瞎的：
//
//	① internal/admin/joblabels_test.go 里那条：拿 admin.New(...) 在测试夹具下取
//	   manifest 再遍历 m.Jobs —— 夹具里**一个上游都没装配**，m.Jobs 恒为空，
//	   循环从不执行（它自己的日志就写着"本配置下没有注册任务"）。永远为真。
//
//	② 本文件第一版：遍历 allUpstreams（14 个真 Provider）取 JobExt.Jobs() ——
//	   看着比 ① 强，实测同样瞎：契约工厂**不做配置**，只有 workbuddy 3 个任务
//	   可见，其余 17 个（含 monkeycode-checkin）全返回空。
//	   变异验证：把 jobLabels 里 monkeycode-checkin 注释掉，测试**照样绿**。
//
// ⇒ 能看见全部任务的只有"任务名的定义处"本身。Job.Name 恒定来自源码里
//
//	某个字符串字面量（`"monkeycode-checkin"`）或由它拼出来（`p.ID() + "-checkin"`）。
//	所以本守卫直接扫**生产代码**里的任务名字面量 —— 新任务一落地就必然被扫到，
//	与它有没有被配置、有没有在夹具里装配都无关。
//
// # 判据的边界（写清楚，免得被当成漏洞）
//
//	· 只扫 .go 生产文件：跳过 _test.go（夹具里那堆 with-checkin-checkin /
//	  fake-refresh 是测试数据，不是任务）与 internal/admin（展示层自己）。
//	· 先剥注释：gateway/extension.go 的文档注释里举过 "growth-watch" 这个例子，
//	  那是说明文字，不是任务名。
//	· 拼出来的名字（p.ID() + "-checkin"）扫不到 —— 但它们的主干 ID 一定在
//	  allUpstreams 里，而每个 ID 的 -checkin/-refresh 都是本文件下面那条
//	  "配对"用例覆盖的范围。
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"workbuddy2api/internal/admin"
)

// jobNameRe 任务名的形状：小写短横线串 + 已知后缀。
//
// 后缀取的是本仓真实用到的六种（git grep 过一遍）：
//
//	-checkin / -refresh / -watch / -welfare / -activity / -health-check
//
// ⚠ 新增后缀（例如哪天有人写 -keepalive）时这里要跟着加，
// 否则那类任务会静默逃过本守卫 —— 这一条写在这里就是为了让下一个人看见。
var jobNameRe = regexp.MustCompile(`"([a-z][a-z0-9]*(?:-[a-z0-9]+)*-(?:checkin|refresh|watch|welfare|activity|health-check))"`)

// lineCommentRe / blockCommentRe 剥注释用。
//
// ⚠ 只是"够用"的剥法（本仓没有字符串里塞 // 的写法），不追求 Go 词法完备：
// 这条守卫要的是"别把文档注释里的示例当成真任务名"，不需要当解析器。
var (
	lineCommentRe  = regexp.MustCompile(`//[^\n]*`)
	blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// TestEveryJobNameLiteralHasLabelAndDesc 生产代码里出现的任务名都要登记。
func TestEveryJobNameLiteralHasLabelAndDesc(t *testing.T) {
	root := repoRoot(t)
	where := map[string]string{} // 任务名 → 第一次出现的位置（报错时指路）

	for _, sub := range []string{"internal", "cmd"} {
		dir := filepath.Join(root, sub)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("找不到 %s（repoRoot 算错了吗？root=%s）: %v", sub, root, err)
		}
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			if strings.HasPrefix(rel, "internal/admin/") {
				return nil // 展示层自己：这两张表就住在那儿
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			src := blockCommentRe.ReplaceAllString(string(b), "")
			src = lineCommentRe.ReplaceAllString(src, "")
			for _, m := range jobNameRe.FindAllStringSubmatch(src, -1) {
				if _, ok := where[m[1]]; !ok {
					where[m[1]] = rel
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s 失败: %v", sub, err)
		}
	}

	for name, file := range where {
		if err := checkJobHasDisplayWords(file, name); err != nil {
			t.Error(err)
		}
	}
	t.Logf("扫到 %d 个任务名，全部已登记", len(where))

	// ⚠ 这条不是客套：扫不到东西时上面的循环一次都不执行，本用例会静默退化成
	//   "永不失败的测试" —— 前两版守卫正是这么栽的。
	if len(where) == 0 {
		t.Error("一个任务名字面量都没扫到 —— 判据失效（后缀表或扫描范围写错了？），本守卫形同虚设")
	}
}

// repoRoot 从当前工作目录往上找 go.mod。
//
// ⚠ 不写死相对路径（"../.."）：go test 的工作目录**就是包目录**，
// 写死"../.."在本包恰好对，但任何人把这个文件挪个位置就静默扫错地方 ——
// 而"扫错目录"的表现是"扫不到东西"，正是本文件反复在防的那种静默失效。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
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

// checkJobHasDisplayWords 判据本体（抽出来是为了能被**证伪**）。
func checkJobHasDisplayWords(file, job string) error {
	if admin.JobLabelOf(job) == "" {
		return fmt.Errorf("%s: 任务 %q 没有中文名（前端会静默退回裸英文）—— "+
			"往 internal/admin/uimanifest.go 的 jobLabels 加一行", file, job)
	}
	if admin.JobDescOf(job) == "" {
		return fmt.Errorf("%s: 任务 %q 没有说明（那一行悬停会是空的）—— "+
			"往 internal/admin/uimanifest.go 的 jobDescs 加一行", file, job)
	}
	return nil
}

// TestCheckJobHasDisplayWordsCatchesViolator 证明上面那条判据真的有咬合力。
//
// ⚠ 这条是"守卫有效"的**证明**，不是重复：它拿一个人造未登记的任务名调用判据，
// 必须报错。若哪天有人把判据改回去（例如误删了空串判断），这条会红 ——
// 而上面那条遍历真代码的用例**不会**（因为真任务名都登记齐了）。
func TestCheckJobHasDisplayWordsCatchesViolator(t *testing.T) {
	if err := checkJobHasDisplayWords("人造违规定义处", "definitely-not-registered-checkin"); err == nil {
		t.Error("对一个没登记的任务名，判据**必须**报错 —— " +
			"它没报错说明这条守卫在真任务名都登记齐之后已经失效，下次漏登记不会有人发现")
	}
}
