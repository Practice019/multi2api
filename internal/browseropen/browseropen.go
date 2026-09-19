// Package browseropen 用**无痕/隐私窗口**打开一个授权链接。
//
// # 为什么需要这一层（而不是让用户自己复制链接）
//
// 「添加账号」走的是 OAuth：上游给的授权页会把**当前浏览器里已登录的
// 账号**授权给网关。于是"用普通窗口打开授权链接"这件事有一个极隐蔽的
// 后果 —— 用户以为在加新号，实际加的是老号（甚至把别的项目的号串进来）。
// 更糟的是它**成功了**：账号池里多了一个账号，没有任何报错提示你
// 去看它是不是你想要的那个。
//
// 用户的要求是「默认打开的就是无痕模式」，所以打开动作必须由网关
// 自己完成（交给前端 `target=_blank` 就等于把"用哪个窗口"交给
// 用户当前的浏览器状态决定）。
//
// # 两条防线
//
//  1. **无痕参数**（`--inprivate` / `--incognito`）——不进历史、不进
//     普通窗口的会话。
//
//  2. **独立 user-data-dir**（默认开启，`Opts.Isolated`）—— 这是"真无痕"
//     与"看起来像无痕"的分界线：只给无痕参数时，无痕窗口仍属于**同一份
//     浏览器安装**，走 SSO 的站点可以直接用已登录账号跳过登录页完成授权。
//     独立 profile 让这次授权从一份全新、无任何 cookie 的目录开始。
//
//     代价是用户要重新输入账号密码（新 profile 里没有登录态）——
//     这正是本需求想要的语义。不想付这个代价就把 `Isolated` 关掉，
//     此时仍然是无痕窗口，只是复用用户自己的 profile。
//
// # 分层
//
// `Plan` 是**纯函数**：只做决策，不产生任何副作用（除建 profile 目录）。
// 所有"参数对不对"的守卫都钉在它上面，测试因此不需要真的开浏览器。
// `Open` = `Plan` + 启动，且**不等待**浏览器退出。
package browseropen

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// profileDirPrefix 独立 profile 目录的固定前缀。
//
// 与清理逻辑共用：清理**只**删带这个前缀的目录，绝不动用户临时目录里的
// 其它东西（那是别人（或用户自己）的文件）。
const profileDirPrefix = "wb2api-login-"

// profileTTL 独立 profile 的保留时长：超过它就在下次打开时清理。
//
// 为什么不是"用完立刻删"：授权可能还没完成（用户还在填密码），
// 删掉 profile 会让浏览器写回失败、甚至当场崩掉标签页。
// 48h 足够覆盖"用户中途去干别的、回来继续"的场景。
const profileTTL = 48 * time.Hour

// Opts 打开策略。零值 = 默认（无痕 + 独立 profile + 自动探测浏览器）。
type Opts struct {
	// Isolated 为 true 时给浏览器一份**独立的 user-data-dir**（临时目录），
	// 保证这次授权不复用任何已登录会话。默认应为 true。
	Isolated bool

	// TempDir 独立 profile 的父目录。空 = os.TempDir()。
	TempDir string

	// Explicit 显式指定的浏览器可执行文件路径（最高优先）。
	//
	// 非空时**不再探测**：用户点名了就用它。参数族按文件名判定
	//（msedge.exe → --inprivate，firefox.exe → -private-window，其余 → --incognito）。
	Explicit string

	// Lookup 探测可执行文件的钩子（测试注入）。
	//
	// 入参是候选 ID（"msedge" / "chrome" / "brave" / "chromium"），
	// 返回可执行文件绝对路径。nil = 按当前平台做真实探测。
	Lookup func(candidateID string) (string, bool)

	// Now 时间源（测试注入）。零值 = time.Now()。
	Now time.Time
}

// Cmd 一次打开动作的完整描述（**规划结果**，不含执行）。
//
// 拆出"规划"与"执行"两层是为了可测：参数向量的对错能被断言，
// 而不必真的在 CI 里弹出浏览器窗口。
type Cmd struct {
	// Browser 展示名（"Microsoft Edge" / "Google Chrome"…），回传给前端。
	Browser string
	// Path 可执行文件绝对路径。
	Path string
	// Args 参数向量（**不含 URL**，URL 由 Argv 追加到末位）。
	Args []string
	// URL 要打开的地址。
	URL string
	// Private 是否按无痕方式打开。恒为 true —— 保留它是为了让
	// "这条命令到底无痕不无痕"成为可断言的数据，而不是注释里的承诺。
	Private bool
	// Isolated 是否用了独立 profile。
	Isolated bool
	// ProfileDir 独立 profile 目录（Isolated 时非空）；清理逻辑用它。
	ProfileDir string
}

// Argv 返回最终要交给 exec 的完整参数向量（URL 在末位）。
//
// ⚠ URL 必须**最后**：放在中间时浏览器会把它当成某个开关的值吃掉，
// 表现成"窗口开了但停在首页"，看起来像授权链接没生成。
func (c Cmd) Argv() []string {
	out := make([]string, 0, len(c.Args)+1)
	out = append(out, c.Args...)
	if c.URL != "" {
		out = append(out, c.URL)
	}
	return out
}

// candidate 一个受支持的浏览器。
type candidate struct {
	id   string // 探测用的标识（Lookup 的入参）
	name string // 展示名
	flag string // 无痕参数
	// winRel Windows 安装路径（相对 Program Files / LocalAppData，按序探测）。
	winRel string
	// linux Linux 可执行名（走 PATH 探测）。
	linux string
	// mac macOS App 名（/Applications/<mac>.app）。
	mac string
}

// candidates 候选浏览器，**顺序即优先级**。
//
// Edge 排第一是因为 Windows 10/11 一定装了它，而 Chrome 不一定 ——
// 默认路径必须是"在这台机器上大概率能跑"的那条，否则用户看到的是
// "点了没反应"。
var candidates = []candidate{
	{
		id: "msedge", name: "Microsoft Edge", flag: "--inprivate",
		winRel: `Microsoft\Edge\Application\msedge.exe`,
		linux:  "microsoft-edge",
		mac:    "Microsoft Edge",
	},
	{
		id: "chrome", name: "Google Chrome", flag: "--incognito",
		winRel: `Google\Chrome\Application\chrome.exe`,
		linux:  "google-chrome",
		mac:    "Google Chrome",
	},
	{
		id: "brave", name: "Brave", flag: "--incognito",
		winRel: `BraveSoftware\Brave-Browser\Application\brave.exe`,
		linux:  "brave-browser",
		mac:    "Brave Browser",
	},
	{
		id: "chromium", name: "Chromium", flag: "--incognito",
		winRel: `Chromium\Application\chrome.exe`,
		linux:  "chromium",
		mac:    "Chromium",
	},
}

// Plan 决定用哪个浏览器、带什么参数。**纯决策**（唯一副作用是按需建
// 独立 profile 目录 —— 浏览器要求该目录先存在，否则它会自己改用默认 profile，
// 那样"独立"就静默失效了）。
func Plan(goos, rawURL string, opts Opts) (Cmd, error) {
	if err := validateURL(rawURL); err != nil {
		return Cmd{}, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	lookup := opts.Lookup

	// 1) 显式指定最高优先。
	if p := strings.TrimSpace(opts.Explicit); p != "" {
		return build(goos, p, nameFromPath(p), flagForPath(p), rawURL, opts, now)
	}

	if lookup == nil {
		lookup = defaultLookup
	}

	switch goos {
	case "windows", "linux":
		for _, c := range candidates {
			if p, ok := lookup(c.id); ok && p != "" {
				return build(goos, p, c.name, c.flag, rawURL, opts, now)
			}
		}
	case "darwin":
		for _, c := range candidates {
			if _, ok := lookup(c.id); ok {
				return buildDarwin(c, rawURL, opts, now)
			}
		}
	default:
		// 不认识的平台**报错**而不是猜一个：猜出来的结果是
		// "看起来能用"的假成功（命令跑了，窗口没出来，且没人知道为什么）。
		return Cmd{}, fmt.Errorf("不支持的平台 %q", goos)
	}

	return Cmd{}, errors.New("未找到可用于无痕打开的浏览器（Edge / Chrome / Brave / Chromium 均未安装）")
}

// Open = Plan + 启动，且**不等待**浏览器退出。
//
// 为什么不等：无痕窗口通常不会自己关。等待会让 /admin/login/start
// 一直挂到用户手动关掉浏览器，前端表现为"一直转圈"。
//
// 返回的 Cmd 供调用方回传展示名（前端要告诉用户"已用哪个无痕窗口打开"）。
// 启动失败时**原样冒泡** —— 吞掉它前端就会显示"已打开"而窗口不存在，
// 用户对着一个不会来的窗口等。
func Open(rawURL string, opts Opts) (Cmd, error) {
	cmd, err := Plan(runtime.GOOS, rawURL, opts)
	if err != nil {
		return Cmd{}, err
	}
	if _, err := starter(cmd.Path, cmd.Argv()); err != nil {
		return cmd, fmt.Errorf("启动 %s 失败: %w", cmd.Browser, err)
	}
	return cmd, nil
}

// starter 启动一个已规划好的命令。抽成包级变量是为了让测试注入桩：
// 参数向量的正确性由 Plan 的用例守，这里只需要验"传下去的是同一份"。
var starter = func(path string, args []string) (func() error, error) {
	c := exec.Command(path, args...)
	// 不接管道：本函数只负责把浏览器**放飞**，任何 stdout 采集都会
	// 让调用方跟着浏览器的生命周期走。
	if err := c.Start(); err != nil {
		return nil, err
	}
	_ = c.Process.Release()
	return func() error { return nil }, nil
}

// build 组装 Windows / Linux 形态的命令。
func build(goos, path, name, flag, rawURL string, opts Opts, now time.Time) (Cmd, error) {
	args := []string{}
	if flag != "" {
		args = append(args, flag)
	}
	cmd := Cmd{Browser: name, Path: path, Args: args, URL: rawURL, Private: flag != ""}
	if opts.Isolated {
		dir, err := makeProfileDir(opts.TempDir, now)
		if err != nil {
			return Cmd{}, err
		}
		cmd.ProfileDir = dir
		cmd.Isolated = true
	}
	// 顺序：无痕参数 → 独立 profile → 首次运行噪音压制 → (URL 由 Argv 追加)。
	// `--no-first-run` / `--no-default-browser-check` 在独立 profile 下是
	// **必须**的：那份 profile 在浏览器看来永远是首次运行，不压掉就会
	// 在授权页前面先弹欢迎/导入向导，用户以为链接打开错了。
	if cmd.ProfileDir != "" {
		cmd.Args = append(cmd.Args, "--user-data-dir="+cmd.ProfileDir,
			"--no-first-run", "--no-default-browser-check")
	} else if flag != "" {
		cmd.Args = append(cmd.Args, "--no-first-run", "--no-default-browser-check")
	}
	return cmd, nil
}

// buildDarwin 组装 macOS 形态的命令：经 `open -n -a <App> --args …`。
//
// 为什么不直接 exec `.app` 里的二进制：macOS 上绕过 LaunchServices
// 直接跑 bundle 内二进制会踩 Gatekeeper / 签名 / 注册路径的坑
// （浏览器认不出自己是哪个 bundle，扩展与默认浏览器状态都可能出错）。
func buildDarwin(c candidate, rawURL string, opts Opts, now time.Time) (Cmd, error) {
	// `-n` = 新实例（不复用已运行的 App 的窗口）。
	// ⚠ `--args` 是分界符：没有它，`--inprivate` 会被 `open` 自己吃掉
	// （open 把未知的 `--xxx` 当成自己的开关），浏览器什么都收不到。
	args := []string{"-n", "-a", c.mac, "--args", c.flag}
	cmd := Cmd{Browser: c.name, Path: "/usr/bin/open", Args: args, URL: rawURL, Private: true}
	if opts.Isolated {
		dir, err := makeProfileDir(opts.TempDir, now)
		if err != nil {
			return Cmd{}, err
		}
		cmd.ProfileDir = dir
		cmd.Isolated = true
		cmd.Args = append(cmd.Args, "--user-data-dir="+cmd.ProfileDir)
	}
	cmd.Args = append(cmd.Args, "--no-first-run", "--no-default-browser-check")
	return cmd, nil
}

// validateURL 只放行 http/https 且有主机名的地址。
//
// 调用方（admin）另外还有一道"只认本进程签发过的链接"的防线；
// 但那是**调用方**的防线。这个函数最终会把字符串交给 exec，
// 所以它自己也不接受 `file://` / `javascript:` 这类东西 ——
// 两层防线要各自独立成立，不能靠对方兜底。
func validateURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("授权链接为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("授权链接无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("授权链接必须是 http/https，实际 scheme=%q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("授权链接缺少主机名")
	}
	return nil
}

// flagForPath 按可执行文件名判断该用哪个无痕参数。
//
// 只认文件名而不是"猜平台"：显式路径给的是 msedge.exe 就不能塞
// `--incognito`（Edge 会把它当成要打开的页面名，实际变成普通窗口）。
func flagForPath(path string) string {
	base := strings.ToLower(filepath.Base(path))
	switch {
	case strings.Contains(base, "edge"):
		return "--inprivate"
	case strings.Contains(base, "firefox"):
		// Firefox 是唯一常见的非 Chromium 系。
		return "-private-window"
	default:
		// Chromium 系占绝对多数，缺省取通用参数。
		return "--incognito"
	}
}

// nameFromPath 从可执行文件名推展示名。
func nameFromPath(path string) string {
	base := filepath.Base(path)
	if base == "" || base == "." {
		return path
	}
	return base
}

// defaultLookup 按当前平台做真实探测。
func defaultLookup(id string) (string, bool) {
	var c candidate
	found := false
	for _, cand := range candidates {
		if cand.id == id {
			c, found = cand, true
			break
		}
	}
	if !found {
		return "", false
	}

	switch runtime.GOOS {
	case "windows":
		for _, root := range windowsRoots() {
			p := filepath.Join(root, c.winRel)
			if isFile(p) {
				return p, true
			}
		}
		// 便携版 / 非标准安装：退回 PATH。
		if p, err := exec.LookPath(id + ".exe"); err == nil {
			return p, true
		}
	case "darwin":
		app := "/Applications/" + c.mac + ".app"
		if fi, err := os.Stat(app); err == nil && fi.IsDir() {
			return app, true
		}
	case "linux":
		if p, err := exec.LookPath(c.linux); err == nil {
			return p, true
		}
	}
	return "", false
}

// windowsRoots 返回 Windows 上可能装浏览器的根目录（按探测顺序）。
func windowsRoots() []string {
	out := make([]string, 0, 3)
	for _, k := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// isFile 判断路径存在且是文件。
func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// makeProfileDir 建一个全新的独立 profile 目录（顺便清理过期的）。
func makeProfileDir(root string, now time.Time) (string, error) {
	if strings.TrimSpace(root) == "" {
		root = os.TempDir()
	}
	if strings.TrimSpace(root) == "" {
		return "", errors.New("无法确定临时目录，无法创建独立 profile")
	}
	// 先清理再创建：清理失败不影响本次打开（best effort），
	// 但**不能**因为清理失败就放弃独立 profile。
	pruneProfiles(root, now, profileTTL)

	dir := filepath.Join(root, fmt.Sprintf("%s%d", profileDirPrefix, now.UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建独立 profile 目录失败: %w", err)
	}
	return dir, nil
}

// pruneProfiles 删除 root 下**本程序建的**、超过 ttl 的 profile 目录。
//
// ⚠ 匹配规则必须严格：root 是用户（或系统）的临时目录，
// 一旦写成"清空 root"，删掉的就是别人的东西。
func pruneProfiles(root string, now time.Time, ttl time.Duration) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), profileDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < ttl {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}
