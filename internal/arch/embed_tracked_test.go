// Package arch 本仓的"结构守卫"用例（**只有测试文件**）。
//
// # 为什么单独一个包
//
// 这条守卫要在**仓库根**的尺度上扫描全部 `*.go` 并问 git "这个文件受追踪吗"。
// 放在 `internal/gateway`（那里已有 arch_test.go）会与既有的测试辅助函数撞名，
// 而它和 gateway 契约并没有关系 —— 它守的是**构建前提**。
package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// embedDirective `//go:embed` 指令：指令后面是**一个或多个**以空白分隔的模式。
//
// ⚠ 只认行内形态：`//go:embed a.txt b/*.txt`。`//go:embed` 不支持跨行续写
// （多行要写多条指令），所以这里逐行扫就够。
var embedDirective = regexp.MustCompile(`^\s*//go:embed\s+(.+?)\s*$`)

// skipDir 扫描时跳过的目录（不是源码，或体积巨大）。
var skipDir = map[string]bool{
	".git": true, "node_modules": true, "_scratch": true, "bin": true,
	"dist": true, "data": true, "data-test": true, "auths": true, "auths-test": true,
	"logs": true, "shots": true, "artifacts": true, ".iso-tooltip": true,
}

// TestGoEmbedTargetsAreGitTracked `//go:embed` 引用的每个文件都必须受 git 追踪。
//
// # 这条守卫为什么存在（同一事故在本仓发生过两次）
//
// `.gitignore` 里有一条按扩展名一刀切的 `*.md`，把 `internal/prompt/` 下的
// **go:embed 载荷**一起吃掉了：
//
//	默认提示词 defaultprompt.md   （见 .gitignore:116-126 的记录）
//	破甲载荷   armor-payload.md   （2026-11-02：CI 与 Release 连续全红）
//
// 症状非常有欺骗性：**本机永远正常**（文件就在磁盘上），只有 CI 上 `go build`
// 报 `pattern armor-payload.md: no matching files found`。
// `.gitignore:123` 把这条教训写成了注释 —— 但注释不会在下次挡住它，这条守卫会。
//
// # ⚠ 为什么不硬编码文件名
//
// 硬编码那 11 个引用等于把"将来新增的 embed"漏掉，而那正是最需要被挡的时刻。
// 所以这里**动态扫**全仓的 `//go:embed`。
//
// # ⚠ git 不可用时 Skip（不是通过、也不是失败）
//
// 这条断言依赖 `git ls-files`。本地开发可能在非 git 目录、或环境里没有 git ——
// 那时**跳过并打印原因**，而不是假装通过（静默通过 = 守卫失效）。
func TestGoEmbedTargetsAreGitTracked(t *testing.T) {
	root, tracked, ok, why := gitTracked()
	if !ok {
		t.Skipf("跳过：%s（这条守卫依赖 git；CI 上一定有 git）", why)
	}
	t.Logf("仓库根=%s，受追踪文件=%d 个", root, len(tracked))

	type ref struct {
		goFile string // 相对仓库根
		dir    string
		pat    string
	}
	var refs []ref

	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 读不动的目录不该让守卫失败
		}
		if info.IsDir() {
			if skipDir[info.Name()] || strings.HasPrefix(info.Name(), "_backup-") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		dir := filepath.Dir(rel)
		for _, line := range strings.Split(string(raw), "\n") {
			m := embedDirective.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			for _, pat := range strings.Fields(m[1]) {
				refs = append(refs, ref{goFile: filepath.ToSlash(rel), dir: dir, pat: pat})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描源码失败：%v", err)
	}
	// ⚠ 扫不到任何 embed 说明扫描逻辑坏了（而不是"仓库没有 embed"）——
	//   那种"守卫静默失效"正是最坏的结果，所以这里直接判失败。
	if len(refs) == 0 {
		t.Fatal("没有扫到任何 //go:embed 指令 —— 扫描逻辑失效了（本仓确实有若干处）")
	}
	t.Logf("扫到 %d 处 go:embed 引用", len(refs))

	var problems []string
	for _, r := range refs {
		targets, matched := resolveEmbedPattern(root, r.dir, r.pat)
		if !matched {
			problems = append(problems, problemText(r.goFile, r.dir, r.pat, "(模式没有匹配到任何文件)"))
			continue
		}
		for _, tg := range targets {
			// 目录本身不需要被追踪（git 不追踪目录），要看它下面的文件
			if fi, serr := os.Stat(filepath.Join(root, filepath.FromSlash(tg))); serr == nil && fi.IsDir() {
				continue
			}
			if !tracked[tg] {
				problems = append(problems, problemText(r.goFile, r.dir, r.pat, tg))
			}
		}
	}
	if len(problems) > 0 {
		t.Fatalf("有 %d 处 go:embed 引用的文件不受 git 追踪：\n%s\n\n%s",
			len(problems), strings.Join(problems, "\n"), fixHint)
	}
}

const fixHint = `# 修法（按顺序）
#   ① 在 .gitignore 里加**否定规则**，必须放在那条 *.md 之类的规则**之后**：
#        !internal/prompt/*.md
#   ② 强制加入（否定规则对"已经被忽略"的文件仍然需要 -f）：
#        git add -f <上面列出的每一个文件>
#   ③ 提交后再看：
#        git ls-files <该文件>          # 有输出 = 好了
#   ④ 本地复核（本机正常、CI 红的那种事故只能靠这条守卫发现）：
#        go test ./internal/arch/ -run TestGoEmbedTargetsAreGitTracked -v`

// problemText 拼一条**可操作**的失败信息（照抄 CI 上会看到的报错，便于对上号）。
func problemText(goFile, dir, pat, missing string) string {
	return "  ✗ " + missing + " 被 go:embed 引用，但没有被 git 追踪\n" +
		"     引用处：" + goFile + "  （模式 `//go:embed " + pat + "`，相对 " + dir + "/）\n" +
		"     ⇒ CI 上 go build 会报：pattern " + pat + ": no matching files found\n" +
		"     ⇒ 本机不会报（文件就在磁盘上）—— 这正是本仓踩过两次的那类事故"
}

// resolveEmbedPattern 把一个 go:embed 模式解析成"相对仓库根"的路径列表。
//
// 语义（与 go:embed 一致）：
//
//	单文件        internal/prompt/armor-payload.md
//	多文件        空格分隔（调用方已拆分）
//	通配符        dir/*.md  → 展开成实际文件
//	目录          dir/      → 该目录**及其子孙**下的文件都算
//
// 第二个返回值 matched：模式是否**匹配到了任何东西**。
// false 就意味着 CI 上必然 `no matching files found`（本机上文件被删/改名也会是 false）。
func resolveEmbedPattern(root, dir, pat string) (paths []string, matched bool) {
	rel := filepath.ToSlash(filepath.Join(dir, pat))
	abs := filepath.Join(root, filepath.FromSlash(rel))

	// 目录形态（以 / 结尾，或文件系统上就是个目录）：目录下所有文件都算。
	if strings.HasSuffix(pat, "/") {
		if _, err := os.Stat(abs); err != nil {
			return nil, false
		}
		var out []string
		_ = filepath.Walk(abs, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			r, rerr := filepath.Rel(root, p)
			if rerr == nil {
				out = append(out, filepath.ToSlash(r))
			}
			return nil
		})
		return out, len(out) > 0
	}

	// 含元字符 ⇒ 按 glob 展开。
	if strings.ContainsAny(pat, "*?[") {
		hits, err := filepath.Glob(abs)
		if err != nil {
			return nil, false
		}
		var out []string
		for _, h := range hits {
			fi, serr := os.Stat(h)
			if serr != nil {
				continue
			}
			if fi.IsDir() {
				_ = filepath.Walk(h, func(p string, info os.FileInfo, err error) error {
					if err != nil || info.IsDir() {
						return nil
					}
					r, rerr := filepath.Rel(root, p)
					if rerr == nil {
						out = append(out, filepath.ToSlash(r))
					}
					return nil
				})
				continue
			}
			r, rerr := filepath.Rel(root, h)
			if rerr == nil {
				out = append(out, filepath.ToSlash(r))
			}
		}
		return out, len(out) > 0
	}

	// 单文件：**文件不存在也要返回它**（那正是 CI 上的失败形态），
	// 让调用方给出"没被追踪"那条可操作的提示，而不是静默略过。
	return []string{rel}, true
}

// gitTracked 返回 (仓库根, 受追踪文件集合, 可用?, 不可用原因)。
//
// ⚠ 判据是"git 能用且能列出文件"，而不是"git 命令存在"：
//
//	在非仓库目录里 `git ls-files` 会输出空 —— 那也算不可用（否则守卫会假绿）。
func gitTracked() (root string, set map[string]bool, ok bool, why string) {
	wd, err := os.Getwd()
	if err != nil {
		return "", nil, false, "取工作目录失败"
	}
	root = findRepoRoot(wd)
	if root == "" {
		return "", nil, false, "找不到仓库根（没有 .git / go.mod）"
	}
	// ⚠⚠ 必须用 `-C <仓库根>`：`git ls-files` 在**子目录**里跑时会把当前目录
	// 当隐式 pathspec，只列那个目录下的文件。而本包（internal/arch）本身就是
	// 新建、尚未提交的目录 ⇒ 输出为空 ⇒ 守卫"假绿"（实测踩到）。
	// 实测：仓库根跑得 41823 字符，internal/arch 里跑得 0 字符；加 -C 后一致。
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return root, nil, false, "git ls-files 不可用（环境里没有 git？）: " + strings.TrimSpace(stderr.String())
	}
	if len(out) == 0 {
		return root, nil, false, "git ls-files 没有输出（不在仓库里?，root=" + root + "）"
	}
	set = make(map[string]bool, 4096)
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		set[filepath.ToSlash(p)] = true
	}
	return root, set, true, ""
}

// findRepoRoot 从 dir 往上找含 .git 或 go.mod 的目录。
//
// ⚠ 找到 .git 就够（CI 的 actions/checkout 一定有）；go.mod 作为退路，
//
//	因为有些环境（worktree / 打包）里 .git 可能是个文件或不存在。
func findRepoRoot(dir string) string {
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
