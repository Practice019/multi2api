package browseropen

// browseropen_test.go —— 「默认用无痕窗口打开授权页」的**决策层**守卫。
//
// # 为什么这些断言必须存在
//
// 打开浏览器这件事有三类错误，界面上全都看不出来：
//
//  1. **静默降级成普通窗口** —— 授权页在用户已登录的浏览器里打开，
//     OAuth 会把**当前已登录的账号**授权给网关。用户以为加的是新号，
//     实际加的是老号（甚至串号）。这是最贵的一种错：
//     它"成功"了，所以没有任何报错提示你去看。
//  2. **复用已有 profile** —— 只给 `--incognito` 参数、不给独立
//     `--user-data-dir` 时，无痕窗口开出来了，但它仍属于同一个浏览器
//     进程/同一份安装，某些站点会把「已登录的账号」带进来（实测有站点
//     走 SSO 跳过登录页，直接用已登录的账号完成授权）。
//  3. **参数顺序错** —— URL 不在最后、或参数被合成一个字符串，
//     浏览器要么忽略 URL（打开空白页），要么把 URL 当参数吃掉。
//
// 所以下面钉的是**参数向量本身**（顺序、成对出现、URL 在末位），
// 而不是"调用没报错"。Plan 是纯函数，这些断言不需要真的开浏览器。

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// lookupOnly 造一个只认某些候选名的探测钩子。
func lookupOnly(names ...string) func(string) (string, bool) {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(id string) (string, bool) {
		if set[id] {
			return `C:\fake\` + id + `.exe`, true
		}
		return "", false
	}
}

// hasArgPrefix 判断参数向量里有没有某个前缀的参数，并返回它的值。
func hasArgPrefix(args []string, prefix string) (string, bool) {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix), true
		}
	}
	return "", false
}

const sampleURL = "https://www.workbuddy.ai/oauth/authorize?state=ST-abc"

// TestPlanWindowsPrefersEdgeInPrivate 有 Edge 时首选 Edge 的 --inprivate。
//
// 为什么首选 Edge 而不是 Chrome：Windows 10/11 **一定**装了 Edge，
// 而 Chrome 不一定有。默认路径必须是"在这台机器上大概率能跑"的那条，
// 否则用户看到的是"点了没反应"。
func TestPlanWindowsPrefersEdgeInPrivate(t *testing.T) {
	cmd, err := Plan("windows", sampleURL, Opts{Lookup: lookupOnly("msedge", "chrome")})
	if err != nil {
		t.Fatalf("有 Edge 时应能规划出命令，实际报错: %v", err)
	}
	if cmd.Browser != "Microsoft Edge" {
		t.Errorf("应首选 Microsoft Edge，实际 %q", cmd.Browser)
	}
	if !strings.Contains(strings.ToLower(cmd.Path), "msedge") {
		t.Errorf("Path 应指向 msedge.exe，实际 %q", cmd.Path)
	}
	if !containsArg(cmd.Args, "--inprivate") {
		t.Errorf("Edge 的无痕参数是 --inprivate，实际 args=%v —— "+
			"少了它就是在用户的普通窗口里打开，会串号", cmd.Args)
	}
}

// TestPlanWindowsFallsBackToChrome 只有 Chrome 时用 --incognito。
func TestPlanWindowsFallsBackToChrome(t *testing.T) {
	cmd, err := Plan("windows", sampleURL, Opts{Lookup: lookupOnly("chrome")})
	if err != nil {
		t.Fatalf("有 Chrome 时应能规划出命令，实际报错: %v", err)
	}
	if cmd.Browser != "Google Chrome" {
		t.Errorf("应回落到 Google Chrome，实际 %q", cmd.Browser)
	}
	if !containsArg(cmd.Args, "--incognito") {
		t.Errorf("Chrome 的无痕参数是 --incognito，实际 args=%v", cmd.Args)
	}
	if containsArg(cmd.Args, "--inprivate") {
		t.Errorf("给 Chrome 传 --inprivate 是无效参数（会被当成要打开的页面名），实际 args=%v", cmd.Args)
	}
}

// TestPlanWindowsNoBrowserIsAnError 一个都没有时**必须报错**。
//
// ⚠ 不能"没有就算了"地返回空命令：调用方会把 nil 当成"已打开"，
// 用户在弹窗里等一个永远不会出现的窗口。必须让调用方拿到一个
// 可展示的错误，好回落成"复制链接"。
func TestPlanWindowsNoBrowserIsAnError(t *testing.T) {
	_, err := Plan("windows", sampleURL, Opts{Lookup: lookupOnly()})
	if err == nil {
		t.Fatal("本机没有任何受支持的浏览器时应报错，实际返回了 nil —— " +
			"调用方会以为已打开，用户对着空白等")
	}
}

// TestPlanIsolatedForcesFreshProfile 默认（Isolated）必须给独立 profile。
//
// 这是"真无痕"与"看起来像无痕"的分界线：
// 只给 --incognito 时，无痕窗口仍属于**同一个浏览器安装**，
// 走 SSO 的站点可以直接拿已登录账号完成授权（实测存在）。
// 独立 --user-data-dir 让这次授权从一份**全新、无任何 cookie**的
// profile 开始，授权对象只可能是用户当场输入的那个账号。
func TestPlanIsolatedForcesFreshProfile(t *testing.T) {
	root := t.TempDir()
	cmd, err := Plan("windows", sampleURL, Opts{
		Isolated: true,
		TempDir:  root,
		Lookup:   lookupOnly("msedge"),
	})
	if err != nil {
		t.Fatal(err)
	}
	val, ok := hasArgPrefix(cmd.Args, "--user-data-dir=")
	if !ok {
		t.Fatalf("Isolated=true 时必须带 --user-data-dir（否则会复用已登录 profile），实际 args=%v", cmd.Args)
	}
	if !strings.HasPrefix(val, root) {
		t.Errorf("独立 profile 必须落在配置的 TempDir(%q) 下，实际 %q", root, val)
	}
	if !strings.Contains(filepath.Base(val), profileDirPrefix) {
		t.Errorf("独立 profile 目录名应带前缀 %q（便于识别与清理），实际 %q", profileDirPrefix, filepath.Base(val))
	}
	if fi, err := os.Stat(val); err != nil || !fi.IsDir() {
		t.Errorf("独立 profile 目录必须**先建出来**再交给浏览器，实际 stat err=%v", err)
	}
	if cmd.ProfileDir != val {
		t.Errorf("Cmd.ProfileDir=%q 应与参数一致 (%q)，否则清理时删不掉", cmd.ProfileDir, val)
	}
}

// TestPlanNonIsolatedHasNoProfileDir Isolated=false 时不带 --user-data-dir。
//
// 这是一条**反向**断言：没有它，"永远加 --user-data-dir"也能让上面的
// 用例全绿 —— 而那会让"用我自己的浏览器、只是开个无痕窗口"这个
// 配置项彻底失效（每次都要重新登录全部站点）。
func TestPlanNonIsolatedHasNoProfileDir(t *testing.T) {
	cmd, err := Plan("windows", sampleURL, Opts{TempDir: t.TempDir(), Lookup: lookupOnly("msedge")})
	if err != nil {
		t.Fatal(err)
	}
	if val, ok := hasArgPrefix(cmd.Args, "--user-data-dir="); ok {
		t.Errorf("Isolated=false 时不该带 --user-data-dir，实际 %q", val)
	}
	if cmd.ProfileDir != "" {
		t.Errorf("Isolated=false 时 ProfileDir 应为空，实际 %q", cmd.ProfileDir)
	}
	// 但无痕参数必须仍在 —— 非隔离 ≠ 非无痕。
	if !containsArg(cmd.Args, "--inprivate") {
		t.Errorf("Isolated=false 只表示复用 profile，**不应**连无痕一起去掉，实际 args=%v", cmd.Args)
	}
}

// TestPlanExplicitBrowserWins 显式配置的路径最高优先。
func TestPlanExplicitBrowserWins(t *testing.T) {
	exe := `D:\portable\msedge.exe`
	cmd, err := Plan("windows", sampleURL, Opts{
		Explicit: exe,
		Lookup:   lookupOnly("chrome"), // 本机有 Chrome，但显式指定优先
	})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != exe {
		t.Errorf("显式路径应最高优先，实际 %q", cmd.Path)
	}
	// 参数族必须按**文件名**判：显式路径给的是 Edge，就不能塞 --incognito。
	if !containsArg(cmd.Args, "--inprivate") {
		t.Errorf("显式给的 msedge.exe 应配 --inprivate，实际 args=%v", cmd.Args)
	}
	if containsArg(cmd.Args, "--incognito") {
		t.Errorf("显式给的 msedge.exe 不该配 --incognito，实际 args=%v", cmd.Args)
	}
}

// TestPlanExplicitUnknownBrowserStillPrivate 显式给一个陌生浏览器也不能变普通窗口。
//
// 缺省取 --incognito：它是 Chromium 系的通用参数，而 Chromium 系
// 占绝对多数。宁可给 Firefox（会忽略未知参数并**仍开普通窗口**）
// 也不能不给 —— 但那种情况必须能被识别，所以 Cmd.Private 显式记录。
func TestPlanExplicitUnknownBrowserStillPrivate(t *testing.T) {
	cmd, err := Plan("windows", sampleURL, Opts{Explicit: `D:\weird\mybrowser.exe`})
	if err != nil {
		t.Fatal(err)
	}
	if !cmd.Private {
		t.Error("显式指定浏览器时仍应标记为无痕打开（Private=true）")
	}
	if !containsArg(cmd.Args, "--incognito") {
		t.Errorf("未知浏览器应缺省给 --incognito，实际 args=%v", cmd.Args)
	}
}

// TestPlanDarwinUsesOpenWithArgs macOS 必须经 open --args 传参。
//
// 直接 exec 到 .app 内部的二进制在 macOS 上是不可靠的（会被
// Gatekeeper/LaunchServices 的注册路径绕开），正确做法是 open -na。
func TestPlanDarwinUsesOpenWithArgs(t *testing.T) {
	cmd, err := Plan("darwin", sampleURL, Opts{Lookup: lookupOnly("chrome")})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cmd.Path) != "open" {
		t.Errorf("macOS 应经 open 启动，实际 %q", cmd.Path)
	}
	if !containsArg(cmd.Args, "--args") {
		t.Errorf("macOS 必须用 --args 把浏览器参数分界，实际 args=%v —— "+
			"少了它 --incognito 会被 open 自己吃掉", cmd.Args)
	}
	// App 名必须作为 -a 的值成对出现。
	found := false
	for i := 0; i+1 < len(cmd.Args); i++ {
		if cmd.Args[i] == "-a" {
			found = true
		}
	}
	if !found {
		t.Errorf("macOS 需要用 -a <AppName> 指定浏览器，实际 args=%v", cmd.Args)
	}
	if !containsArg(cmd.Args, "--incognito") {
		t.Errorf("macOS 的 Chrome 无痕参数同样是 --incognito，实际 args=%v", cmd.Args)
	}
}

// TestPlanLinuxUsesChromiumFamilyFlag Linux 直接用可执行文件 + --incognito。
func TestPlanLinuxUsesChromiumFamilyFlag(t *testing.T) {
	// 注入的 Lookup 拿到的是**候选 ID**（"chrome"），返回的是该平台上
	// 真实的可执行文件路径 —— 与 defaultLookup 的分工一致。
	lookup := func(id string) (string, bool) {
		if id == "chrome" {
			return "/usr/bin/google-chrome", true
		}
		return "", false
	}
	cmd, err := Plan("linux", sampleURL, Opts{Lookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cmd.Path) != "google-chrome" {
		t.Errorf("Linux 应直接用 google-chrome，实际 %q", cmd.Path)
	}
	if !containsArg(cmd.Args, "--incognito") {
		t.Errorf("Linux 的 Chrome 无痕参数是 --incognito，实际 args=%v", cmd.Args)
	}
}

// TestPlanRejectsNonHTTPURL 只允许 http/https。
//
// 这个函数最终会把用户可控的字符串交给 exec。虽然 admin 层已经用
// "只认自己签发过的链接"挡了一道，但那是**调用方**的防线；
// 决策层自己也不该接受 file:// / javascript: 这类东西。
func TestPlanRejectsNonHTTPURL(t *testing.T) {
	bad := []string{
		"",
		"file:///C:/Windows/System32/calc.exe",
		"javascript:alert(1)",
		"ms-settings:",
		"ftp://example.com/x",
		"https://",
	}
	for _, u := range bad {
		if _, err := Plan("windows", u, Opts{Lookup: lookupOnly("msedge")}); err == nil {
			t.Errorf("URL %q 应被拒绝，实际通过了 —— "+
				"这是把任意字符串交给 exec 的口子", u)
		}
	}
}

// TestPlanURLIsLastArg URL 必须在参数向量末位。
//
// 参数顺序错的表现是"浏览器开了但停在首页/空白页"，看起来像
// "授权链接没生成"，实际是参数被吃掉了 —— 极难定位。
func TestPlanURLIsLastArg(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		cmd, err := Plan(goos, sampleURL, Opts{
			TempDir: t.TempDir(),
			Lookup:  lookupOnly("msedge", "chrome", "google-chrome"),
		})
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		argv := cmd.Argv()
		if argv[len(argv)-1] != sampleURL {
			t.Errorf("%s: URL 必须是最后一个参数（否则会被浏览器当成开关吃掉），实际 argv=%v", goos, argv)
		}
		if len(cmd.Args) == 0 {
			t.Errorf("%s: Args 为空会让 Argv 只有 URL，参数全丢", goos)
		}
	}
}

// TestPlanArgsAreSelfContained 每个参数必须是**独立**元素。
//
// 曾经见过的写法是把参数拼成一个字符串再整体传：
// `"--incognito --new-window https://..."` —— Go 不会替你分词，
// 浏览器收到的是一个以 `--incognito ` 开头的**单个**未知参数，
// 于是既不开无痕也不开页面，静默变成"普通窗口打开默认页"。
func TestPlanArgsAreSelfContained(t *testing.T) {
	cmd, err := Plan("windows", sampleURL, Opts{Lookup: lookupOnly("msedge")})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, " ") {
			t.Errorf("参数 %q 里含空格 —— 参数必须逐元素传，拼接会被当成单个未知开关", a)
		}
	}
}

// TestPlanSkipsNoFirstRunNoise 首次运行提示必须被压掉。
//
// 独立 profile（Isolated）在浏览器看来永远是"首次运行"，
// 不压掉就会先在授权页前面弹一个欢迎/导入向导 ——
// 用户以为链接打开错了。
func TestPlanIsolatedSuppressesFirstRunWizard(t *testing.T) {
	cmd, err := Plan("windows", sampleURL, Opts{
		Isolated: true, TempDir: t.TempDir(), Lookup: lookupOnly("msedge"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"--no-first-run", "--no-default-browser-check"} {
		if !containsArg(cmd.Args, a) {
			t.Errorf("独立 profile 必须带 %s（否则授权页前面先弹欢迎向导），实际 args=%v", a, cmd.Args)
		}
	}
}

// TestPlanUnknownGOOSIsAnError 不认识的平台必须报错，不能猜。
func TestPlanUnknownGOOSIsAnError(t *testing.T) {
	if _, err := Plan("plan9", sampleURL, Opts{Lookup: lookupOnly("chrome")}); err == nil {
		t.Error("未知平台应报错（猜一个来跑是「看起来能用」的假成功）")
	}
}

// TestPruneProfilesRemovesOnlyOldOnes 清理只动**过期的**自己建的目录。
//
// 为什么必须钉住"只动自己建的"：这个函数跑在用户临时目录里，
// 一旦匹配规则写宽（比如 removeAll(root)），就会删掉别人的东西。
func TestPruneProfilesRemovesOnlyOldOnes(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	old := filepath.Join(root, profileDirPrefix+"old")
	fresh := filepath.Join(root, profileDirPrefix+"fresh")
	stranger := filepath.Join(root, "somebody-elses-dir")
	for _, d := range []string{old, fresh, stranger} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldT := now.Add(-72 * time.Hour)
	if err := os.Chtimes(old, oldT, oldT); err != nil {
		t.Fatal(err)
	}

	pruneProfiles(root, now, 48*time.Hour)

	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("超过 48h 的旧 profile 应被删除，实际仍在（err=%v）", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("新鲜 profile 不该被删，实际 err=%v", err)
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("非本程序建的目录**绝不能**删（匹配规则写宽了），实际 err=%v", err)
	}
}

// TestOpenStartsWithoutWaiting Open 必须**不等待**浏览器退出。
//
// 等待会让 /admin/login/start 卡住直到用户关掉浏览器
// （无痕窗口常常不会自己关），前端表现为"一直转圈"。
func TestOpenStartsWithoutWaiting(t *testing.T) {
	var gotPath string
	var gotArgs []string
	restore := swapStarter(func(path string, args []string) (func() error, error) {
		gotPath, gotArgs = path, args
		return func() error { return nil }, nil
	})
	defer restore()

	cmd, err := Open(sampleURL, Opts{Lookup: lookupOnly("msedge")})
	if err != nil {
		t.Fatalf("Open 应成功，实际 %v", err)
	}
	if gotPath != cmd.Path {
		t.Errorf("启动的路径 %q 与返回的 Cmd.Path %q 不一致", gotPath, cmd.Path)
	}
	if len(gotArgs) == 0 || gotArgs[len(gotArgs)-1] != sampleURL {
		t.Errorf("启动参数应含 URL 且位于末位，实际 %v", gotArgs)
	}
}

// TestOpenPropagatesStarterError 启动失败必须冒泡（前端要据此回落复制链接）。
func TestOpenPropagatesStarterError(t *testing.T) {
	boom := errors.New("spawn denied")
	restore := swapStarter(func(string, []string) (func() error, error) {
		return nil, boom
	})
	defer restore()

	if _, err := Open(sampleURL, Opts{Lookup: lookupOnly("msedge")}); !errors.Is(err, boom) {
		t.Errorf("启动失败应原样冒泡，实际 %v —— "+
			"吞掉它前端就会显示「已用无痕窗口打开」而窗口不存在", err)
	}
}

// containsArg 判断参数向量里有没有某个**逐字**参数。
func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// swapStarter 用桩替换真实的启动函数，返回还原函数。
//
// 为什么需要它：`Open` 会真的 exec 一个进程。CI / 开发机上弹出一个
// 浏览器窗口既慢又吵，而且无法断言"传下去的到底是什么"。
// 换成桩之后，断言的对象从"窗口有没有出来"变成"交给 exec 的
// 路径与参数向量"—— 后者才是这次改动可能写错的地方。
func swapStarter(f func(string, []string) (func() error, error)) func() {
	old := starter
	starter = f
	return func() { starter = old }
}

// TestPlanUsesRealDefaultsWhenLookupNil 不注入 Lookup 时不能 panic。
//
// 这条只验"默认探测路径能跑通且不炸"：真机上有没有浏览器不由测试决定，
// 所以允许 err 非 nil，但**不允许 panic**（nil map / nil func 调用）。
func TestPlanUsesRealDefaultsWhenLookupNil(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Lookup=nil（真实探测）时 panic 了：%v", r)
		}
	}()
	_, _ = Plan(runtime.GOOS, sampleURL, Opts{})
}
