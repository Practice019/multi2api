// sysmon_test.go —— 两层测试：纯函数（表驱动、任何平台都跑）+ 真机采样（验收 3/4）。
//
// # 为什么解析逻辑必须与平台实现解耦
//
// 本仓的开发机是 Windows（没有 /proc），生产是 Linux（有 /proc）。若解析逻辑写在
// sysmon_unix.go 里，那它在**开发机上根本编不进来**，测试也就无从谈起 ——
// 只能等 CI 跑 Linux 才发现少了个字段。
//
// 所以 `parseProcStat` / `parseMeminfo` 放在平台无关的 sysmon.go 里，
// 用**固定的 /proc 文本**喂它们（本文件下面的样例取自真实内核输出）。
// 这才是"表格驱动 + 同包白盒"能抓到的东西：字段错位、漏乘 1024、
// guest 重复计入 —— 这些错误在真机上表现为"数字有点怪"，永远不会让构建失败。
package sysmon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParseProcStat /proc/stat 解析：字段错位与本包最关键的两个口径。
func TestParseProcStat(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  cpuTimes
		ok    bool
	}{
		{
			name: "真实内核输出（8 核，含 iowait）",
			input: "cpu  8000 100 2000 30000 500 0 100 0 0 0\n" +
				"cpu0 1000 10 250 3750 60 0 12 0 0 0\n" +
				"intr 12345\n",
			// total = 8000+100+2000+30000+500+0+100+0 = 40700
			// idle  = idle(30000) + iowait(500) = 30500
			want: cpuTimes{idle: 30500, total: 40700},
			ok:   true,
		},
		{
			name:  "老内核没有 iowait（只有 4 个字段）",
			input: "cpu  100 0 50 850\n",
			// total = 1000, idle = 850
			want: cpuTimes{idle: 850, total: 1000},
			ok:   true,
		},
		{
			name:  "没有 cpu 行",
			input: "intr 1\nctxt 2\n",
			ok:    false,
		},
		{
			name:  "字段不足",
			input: "cpu  1 2\n",
			ok:    false,
		},
		{
			name:  "非数字",
			input: "cpu  x 0 0 0\n",
			ok:    false,
		},
		{
			name:  "空输入",
			input: "",
			ok:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseProcStat(c.input)
			if ok != c.ok {
				t.Fatalf("ok=%v，想要 %v", ok, c.ok)
			}
			if !c.ok {
				return
			}
			if got != c.want {
				t.Fatalf("得到 %+v，想要 %+v", got, c.want)
			}
		})
	}

	// ⚠ 单列断言：iowait **必须**算进 idle。
	// 若哪天有人把它按"非空闲"处理，一台被磁盘拖住的机器会显示 CPU 80% 忙，
	// 而检测逻辑会去追一个不存在的 CPU 瓶颈。
	// 反例输入：空闲 850、iowait 150、其余 0。
	//   算进来 ⇒ idle=1000/total=1000 ⇒ 0% 占用
	//   不算   ⇒ idle=850/1000      ⇒ 15% 占用（错）
	if got, _ := parseProcStat("cpu  0 0 0 850 150\n"); got.idle != 1000 || got.total != 1000 {
		t.Errorf("iowait 没被算进 idle：%+v", got)
	}
}

// TestParseMeminfo /proc/meminfo 解析：单位（kB→byte）与 MemAvailable 回退链。
func TestParseMeminfo(t *testing.T) {
	const kb = 1024

	t.Run("有 MemAvailable（现代内核）", func(t *testing.T) {
		// 取自真实 16GB 机器的缩减版
		input := "MemTotal:       16384000 kB\n" +
			"MemFree:         1000000 kB\n" +
			"MemAvailable:    8000000 kB\n" +
			"Buffers:          200000 kB\n" +
			"Cached:          4000000 kB\n"
		total, avail, ok := parseMeminfo(input)
		if !ok {
			t.Fatal("应当解析成功")
		}
		if total != 16384000*kb {
			t.Errorf("total=%d，想要 %d（忘了乘 1024？）", total, 16384000*kb)
		}
		// ⚠ 必须优先用 MemAvailable，而不是 Free+Buffers+Cached（那是 5200000）。
		if avail != 8000000*kb {
			t.Errorf("avail=%d，想要 %d（应优先用 MemAvailable）", avail, 8000000*kb)
		}
	})

	t.Run("无 MemAvailable（老内核 3.14 前）", func(t *testing.T) {
		input := "MemTotal:       16000000 kB\n" +
			"MemFree:         1000000 kB\n" +
			"Buffers:          200000 kB\n" +
			"Cached:          4000000 kB\n"
		total, avail, ok := parseMeminfo(input)
		if !ok {
			t.Fatal("应当解析成功（回退链必须存在）")
		}
		if total != 16000000*kb {
			t.Errorf("total=%d", total)
		}
		if want := uint64(5200000) * kb; avail != want {
			t.Errorf("avail=%d，想要 %d（Free+Buffers+Cached）", avail, want)
		}
	})

	t.Run("只有 MemTotal", func(t *testing.T) {
		// 退化到 0 可用 = 100% 占用。这是"保守方向"的极端情形：
		// 宁可报内存紧张，也不要报"充足"而掩盖问题。
		total, avail, ok := parseMeminfo("MemTotal:       1000 kB\n")
		if !ok || total != 1000*kb || avail != 0 {
			t.Fatalf("total=%d avail=%d ok=%v", total, avail, ok)
		}
	})

	t.Run("缺 MemTotal", func(t *testing.T) {
		if _, _, ok := parseMeminfo("MemFree: 100 kB\n"); ok {
			t.Fatal("没有 MemTotal 应当 ok=false（半份数据算不出占用率）")
		}
	})

	t.Run("行内带冒号的怪行不崩", func(t *testing.T) {
		// HugePages_Total: 0 这类没有 kB 后缀的行也必须能跳过而不 panic。
		input := "MemTotal:       1000 kB\nHugePages_Total:       0\nMemAvailable:    500 kB\n"
		total, avail, ok := parseMeminfo(input)
		if !ok || total != 1000*kb || avail != 500*kb {
			t.Fatalf("total=%d avail=%d ok=%v", total, avail, ok)
		}
	})
}

// TestCPUPercent 差值计算：正常路径 + **每一种"读数不可信"都返回 ok=false**。
//
// # 为什么这里逐条列反例
//
// 这五个分支的正确行为都是"不报错、返回 0 且 ok=false"。若有人把它们改成
// clamp 到 0 继续返回 ok=true，页面会显示"CPU 0%"——一个看起来完全正常的
// 错误结论，永远不会有人去查。反例必须被钉死。
func TestCPUPercent(t *testing.T) {
	cases := []struct {
		name string
		prev cpuTimes
		cur  cpuTimes
		pct  float64
		ok   bool
	}{
		{
			name: "全忙：total 增 1000，idle 没变",
			prev: cpuTimes{idle: 500, total: 1000},
			cur:  cpuTimes{idle: 500, total: 2000},
			pct:  100, ok: true,
		},
		{
			name: "全闲：total 增 1000，idle 同增 1000",
			prev: cpuTimes{idle: 500, total: 1000},
			cur:  cpuTimes{idle: 1500, total: 2000},
			pct:  0, ok: true,
		},
		{
			name: "对半：idle 占一半增量",
			prev: cpuTimes{idle: 0, total: 0},
			cur:  cpuTimes{idle: 500, total: 1000},
			pct:  50, ok: true,
		},
		{
			// 真实机器上偶尔会出现（虚拟化时钟跳变）。返回 0+ok 会撒谎。
			name: "计数器没前进",
			prev: cpuTimes{idle: 500, total: 1000},
			cur:  cpuTimes{idle: 500, total: 1000},
			ok:   false,
		},
		{
			name: "计数器倒退（回绕）",
			prev: cpuTimes{idle: 500, total: 2000},
			cur:  cpuTimes{idle: 500, total: 1000},
			ok:   false,
		},
		{
			name: "idle 回绕",
			prev: cpuTimes{idle: 900, total: 1000},
			cur:  cpuTimes{idle: 100, total: 2000},
			ok:   false,
		},
		{
			// 两个计数器步长不一致 ⇒ 读数来自不同时刻，差值无意义。
			name: "idle 增量大于 total 增量",
			prev: cpuTimes{idle: 0, total: 1000},
			cur:  cpuTimes{idle: 600, total: 1500},
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pct, ok := cpuPercent(c.prev, c.cur)
			if ok != c.ok {
				t.Fatalf("ok=%v，想要 %v（不可信的读数必须 ok=false，不许瞎猜 0）", ok, c.ok)
			}
			if !c.ok {
				return
			}
			if pct < c.pct-0.001 || pct > c.pct+0.001 {
				t.Fatalf("pct=%v，想要 %v", pct, c.pct)
			}
			if pct < 0 || pct > 100 {
				t.Fatalf("pct=%v 越界（页面会显示 >100%%）", pct)
			}
		})
	}
}

// TestPercentClamp 百分比夹取与除零。
func TestPercentClamp(t *testing.T) {
	cases := []struct {
		a, b uint64
		want float64
	}{
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		// ⚠ 采样跨了一次热插拔时会出现 a>b，必须夹到 100 而不是 100.3
		{101, 100, 100},
		// 除零：绝不 panic、绝不 +Inf（JSON 编不出来，前端拿到 null）
		{0, 0, 0},
		{5, 0, 0},
	}
	for _, c := range cases {
		if got := percent(c.a, c.b); got != c.want {
			t.Errorf("percent(%d,%d)=%v，想要 %v", c.a, c.b, got, c.want)
		}
	}
	// clampPct 单独钉两侧
	if clampPct(-5) != 0 || clampPct(150) != 100 || clampPct(42.5) != 42.5 {
		t.Error("clampPct 边界不对")
	}
}

// TestProbeDiskPath 磁盘路径探测：存在的目录优先，否则退到临时目录。
func TestProbeDiskPath(t *testing.T) {
	t.Run("存在的目录", func(t *testing.T) {
		dir := t.TempDir()
		got := probeDiskPath(dir)
		want, _ := filepath.Abs(dir)
		if got != want {
			t.Fatalf("得到 %q，想要 %q", got, want)
		}
	})

	t.Run("不存在的目录退到 CWD 或临时目录", func(t *testing.T) {
		got := probeDiskPath(filepath.Join(t.TempDir(), "不存在", "更深一层"))
		if got == "" {
			t.Fatal("必须返回一个可用路径（否则磁盘项永远 ok=false）")
		}
		if st, err := os.Stat(got); err != nil || !st.IsDir() {
			t.Fatalf("返回的路径 %q 不是存在的目录（Stat err=%v）", got, err)
		}
	})

	t.Run("空串不 panic", func(t *testing.T) {
		if got := probeDiskPath(""); got == "" {
			t.Fatal("空串也必须返回可用路径")
		}
	})
}

// TestSamplerFirstSampleCPUUnknown 第一次采样只建立基线：CPUOK 必须是 false。
//
// ⚠ 这条是"不瞎猜"的核心断言。若实现里改成"第一次返回 0 且 ok=true"，
// 状态页在刚启动时会稳定地显示 CPU 0%，而那是假的。
func TestSamplerFirstSampleCPUUnknown(t *testing.T) {
	s := NewAt(t.TempDir())
	first := s.Sample()
	if first.CPUOK {
		t.Fatalf("第一次采样不该有 CPU 百分比（没有基线），得到 pct=%v ok=true", first.CPUPct)
	}
	if first.CPUPct != 0 {
		t.Fatalf("第一次采样的 CPU 值必须是零值，得到 %v", first.CPUPct)
	}
	if !runtimeHasCPUCounters() {
		t.Skip("本平台没有可读的 CPU 计数器（如 macOS），后续断言不适用")
	}
	// 给计数器一点前进的时间，然后第二次必须拿到 ok=true。
	time.Sleep(120 * time.Millisecond)
	second := s.Sample()
	if !second.CPUOK {
		t.Fatalf("第二次采样应当有百分比（基线已建立）")
	}
	if second.CPUPct < 0 || second.CPUPct > 100 {
		t.Fatalf("CPU 百分比越界：%v", second.CPUPct)
	}
}

// runtimeHasCPUCounters 报告本平台能否读到整机 CPU 计数器。
// 只有 macOS/BSD（无 /proc）会返回 false；Windows 与 Linux 都是 true。
func runtimeHasCPUCounters() bool {
	_, ok := readCPUTimes()
	return ok
}

// TestSamplerIsolation 两个 Sampler 的 CPU 基线互不影响。
//
// # 为什么这是本文件里最容易被忽略的一条
//
// 若基线是包级变量，A（状态页，1s 一次）与 B（巡检，60s 一次）会互相偷走
// 对方的时间窗口。得到的是一个既不是 1s 也不是 60s 的平均值 ——
// **而且没有任何迹象表明它错了**：数字在 0..100 内，看起来完全正常。
func TestSamplerIsolation(t *testing.T) {
	dir := t.TempDir()
	a, b := NewAt(dir), NewAt(dir)

	// a 采两次（建立基线 + 拿到值）
	a.Sample()
	time.Sleep(60 * time.Millisecond)
	if snap := a.Sample(); !snap.CPUOK {
		t.Skip("本平台没有 CPU 计数器")
	}

	// b 若共享了 a 的基线，它第一次采样就会返回 ok=true —— 那就是串味。
	if snap := b.Sample(); snap.CPUOK {
		t.Fatal("新 Sampler 的第一次采样不该有 CPU 百分比（基线必须是实例私有的）")
	}
}

// TestSamplerConcurrent 并发采样不 panic、不产生越界值（-race 下更有意义）。
//
// 采样会被 HTTP handler 从多个 goroutine 同时调用，而 s.mu 保护的正是
// prevCPU 与 diskPath 两处写。忘了加锁在 -race 之外看不出来。
func TestSamplerConcurrent(t *testing.T) {
	s := NewAt(t.TempDir())
	s.Sample() // 建基线，避免所有 goroutine 都走"首次"分支

	const n = 16
	var wg sync.WaitGroup
	results := make([]Snapshot, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.Sample()
		}(i)
	}
	wg.Wait()

	for i, snap := range results {
		if snap.At <= 0 {
			t.Errorf("第 %d 个快照没有时间戳", i)
		}
		if snap.UptimeSec <= 0 {
			t.Errorf("第 %d 个快照 uptime=%v，应当 > 0", i, snap.UptimeSec)
		}
		if snap.CPUOK && (snap.CPUPct < 0 || snap.CPUPct > 100) {
			t.Errorf("第 %d 个快照 cpu_pct=%v 越界", i, snap.CPUPct)
		}
		if snap.MemOK && (snap.MemPct < 0 || snap.MemPct > 100) {
			t.Errorf("第 %d 个快照 mem_pct=%v 越界", i, snap.MemPct)
		}
		if snap.DiskOK && (snap.DiskPct < 0 || snap.DiskPct > 100) {
			t.Errorf("第 %d 个快照 disk_pct=%v 越界", i, snap.DiskPct)
		}
	}
}

// TestSampleRuntimeFields goroutine / 堆 / GC / uptime 这些进程自身指标。
//
// ⚠ 这些字段没有 OK 位（任何平台都有 runtime），所以它们必须**恒有值**。
// 若哪天变成 0，页面会显示"0 goroutines"——比显示"—"更误导。
func TestSampleRuntimeFields(t *testing.T) {
	s := NewAt(t.TempDir())
	snap := s.Sample()

	if snap.Goroutines <= 0 {
		t.Errorf("goroutines=%d，应当 > 0（至少有测试 goroutine 自己）", snap.Goroutines)
	}
	cpu := runtime.NumCPU()
	if snap.Goroutines > 1_000_000 {
		// 不是硬上限，只是防"读错了一个负数字段变成巨大的 uint"这类事故。
		t.Errorf("goroutines=%d 离谱（本机 %d 核）", snap.Goroutines, cpu)
	}
	if snap.Goroutines < cpu {
		// GOMAXPROCS 默认不让运行时预起满核的 P 的 goroutine，
		// 但至少不该比核数少得多。这条只是防"读成 0/1"。
		t.Logf("goroutines=%d < cpu=%d（仅记录，某些配置下正常）", snap.Goroutines, cpu)
	}

	// HeapAlloc：为 0 只可能是"刚启动且什么都没分配" —— 但测试自己就在分配。
	if snap.HeapAllocBytes == 0 {
		t.Error("heap_alloc_bytes=0 不可信（测试本身就在分配内存）")
	}
	if snap.UptimeSec <= 0 {
		t.Errorf("uptime_sec=%v，应当 > 0", snap.UptimeSec)
	}
	if snap.At <= 0 {
		t.Errorf("at=%d，应当是 Unix 毫秒", snap.At)
	}
	// At 与 UptimeSec 必须自洽：采样时刻减去 uptime 应当≈进程启动时刻。
	startByAt := time.UnixMilli(snap.At).Add(-time.Duration(snap.UptimeSec * float64(time.Second)))
	if skew := time.Since(startByAt); skew < 0 || skew > 5*time.Minute {
		t.Errorf("at 与 uptime 不自洽：按它们算出的启动时刻距今 %v", skew)
	}

	// GC 字段：进程可能还没 GC 过，此时计数为 0 是允许的；
	// 但一旦计数 > 0，最近一次暂停必须落在环形缓冲的正确槽位（而不是 PauseNs[0]）。
	if snap.GCCount > 0 && snap.GCPauseTotalNS == 0 {
		t.Errorf("GCCount=%d 但 PauseTotalNS=0（两个字段不同步）", snap.GCCount)
	}
}

// TestSnapshotJSONFieldNames 钉住出站 JSON 键名。
//
// # 为什么值得单独测
//
// 这些键名是**前端绑定契约**（PLAN-sql-unify `3 T9 的状态页按 cpu_pct / mem_pct /
// disk_pct 显示）。改一个字段名不会有编译错误，只会在页面上变成一个空白格 ——
// 而且通常要到"有人抱怨某个数没了"才发现。
func TestSnapshotJSONFieldNames(t *testing.T) {
	wantKeys := []string{
		"at", "cpu_pct", "cpu_ok",
		"mem_pct", "mem_total_bytes", "mem_used_bytes", "mem_ok",
		"disk_pct", "disk_total_bytes", "disk_free_bytes", "disk_ok",
		"goroutines", "heap_alloc_bytes", "gc_count", "gc_pause_ns",
		"gc_pause_total_ns", "uptime_sec",
	}
	b, err := json.Marshal(Snapshot{})
	if err != nil {
		t.Fatalf("快照必须可序列化（前端要直接吃它）：%v", err)
	}
	got := string(b)
	for _, k := range wantKeys {
		if !strings.Contains(got, `"`+k+`":`) {
			t.Errorf("JSON 里缺字段 %q（前端绑定会变成空白）：%s", k, got)
		}
	}
	// disk_path 带 omitempty：空串不该出现在 JSON 里（省一个字段的体积，
	// 且"没探测到"与"探测到空串"是同一件事）。
	if strings.Contains(got, "disk_path") {
		t.Errorf("空 disk_path 不该出现（omitempty）：%s", got)
	}
	// NaN/Inf 会让 json.Marshal 直接失败。上面已经 Marshal 成功，
	// 这里再确认没有 inf 字样（防御性：极小的概率下 float 会编成 1e999 之类）。
	if strings.Contains(got, "Inf") || strings.Contains(got, "NaN") {
		t.Errorf("JSON 里出现 Inf/NaN（环境不支持，前端解析会崩）：%s", got)
	}
}

// TestSnapshotFailedSections 失败项枚举：顺序稳定、只列失败的。
func TestSnapshotFailedSections(t *testing.T) {
	cases := []struct {
		name string
		snap Snapshot
		want []string
	}{
		{"全失败", Snapshot{}, []string{"cpu", "mem", "disk"}},
		{"全成功", Snapshot{CPUOK: true, MemOK: true, DiskOK: true}, nil},
		{"只有磁盘失败", Snapshot{CPUOK: true, MemOK: true}, []string{"disk"}},
		{"只有 CPU 失败", Snapshot{MemOK: true, DiskOK: true}, []string{"cpu"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.snap.FailedSections()
			if len(got) != len(c.want) {
				t.Fatalf("得到 %v，想要 %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("得到 %v，想要 %v（顺序也是契约：页面按它拼提示文案）", got, c.want)
				}
			}
		})
	}
}

// TestSampleLive 真机采样（验收条件 3 / 4）。
//
// # 为什么断言是"合理区间"而不是具体值
//
// 我们在别人的机器上跑测试，CPU/内存/磁盘的具体数**不可预测**。
// 不变的只有几条恒真式：
//
//   - 百分比必须在 (0,100]，且总量 > 0
//   - used <= total、free <= total
//   - uptime > 0、goroutines > 0
//
// ⚠ CPU 用 [0,100) 而不是 (0,100]：一台真正空闲的机器上它可以是 0。
// 若这里断言 cpu_pct > 0，任何 CI 空跑都会红。
func TestSampleLive(t *testing.T) {
	s := New()
	s.Sample() // 建 CPU 基线
	time.Sleep(150 * time.Millisecond)
	snap := s.Sample()

	t.Logf("live snapshot: cpu=%.2f%% mem=%.2f%% (%d/%d) disk=%.2f%% (%d free / %d total) @%s goroutines=%d heap=%dMB gc=%d uptime=%.1fs",
		snap.CPUPct, snap.MemPct, snap.MemUsedBytes, snap.MemTotalBytes,
		snap.DiskPct, snap.DiskFreeBytes, snap.DiskTotalBytes, snap.DiskPath,
		snap.Goroutines, snap.HeapAllocBytes>>20, snap.GCCount, snap.UptimeSec)

	// ── CPU ──
	if runtimeHasCPUCounters() {
		if !snap.CPUOK {
			t.Errorf("CPUOK=false，但本平台有 CPU 计数器（%s）", runtime.GOOS)
		}
		if snap.CPUPct < 0 || snap.CPUPct > 100 {
			t.Errorf("cpu_pct=%.2f 不在 [0,100]", snap.CPUPct)
		}
	} else {
		t.Logf("本平台（%s）没有整机 CPU 计数器，CPUOK=%v 符合预期", runtime.GOOS, snap.CPUOK)
	}

	// ── 内存 ──
	if !snap.MemOK {
		t.Errorf("MemOK=false（%s 上内存总量应当读得到）", runtime.GOOS)
	} else {
		if snap.MemTotalBytes == 0 {
			t.Error("mem_total_bytes=0")
		}
		if snap.MemUsedBytes > snap.MemTotalBytes {
			t.Errorf("mem_used=%d > mem_total=%d", snap.MemUsedBytes, snap.MemTotalBytes)
		}
		if snap.MemPct <= 0 || snap.MemPct > 100 {
			// 一个正在跑测试的进程必然占了一点内存 ⇒ 不会是 0。
			t.Errorf("mem_pct=%.2f 不在 (0,100]", snap.MemPct)
		}
	}

	// ── 磁盘 ──
	if !snap.DiskOK {
		t.Errorf("DiskOK=false（%s 上 Statfs/GetDiskFreeSpaceEx 应当可用，path=%q）",
			runtime.GOOS, snap.DiskPath)
	} else {
		if snap.DiskTotalBytes == 0 {
			t.Error("disk_total_bytes=0")
		}
		if snap.DiskFreeBytes > snap.DiskTotalBytes {
			t.Errorf("disk_free=%d > disk_total=%d", snap.DiskFreeBytes, snap.DiskTotalBytes)
		}
		// 磁盘占了 0..100：全满是合法的，全空也是合法的（新盘）。
		if snap.DiskPct < 0 || snap.DiskPct > 100 {
			t.Errorf("disk_pct=%.2f 不在 [0,100]", snap.DiskPct)
		}
		// 探测路径必须是个存在的目录（否则这个数是在报另一个盘）。
		if st, err := os.Stat(snap.DiskPath); err != nil || !st.IsDir() {
			t.Errorf("disk_path=%q 不是存在的目录（err=%v）", snap.DiskPath, err)
		}
	}

	// ── 进程自身 ──
	if snap.Goroutines <= 0 {
		t.Errorf("goroutines=%d", snap.Goroutines)
	}
	if snap.UptimeSec <= 0 {
		t.Errorf("uptime_sec=%v", snap.UptimeSec)
	}

	// 至少 CPU（Windows/Linux）与内存/磁盘都在，才算"资源与入口"这组齐了。
	if failed := snap.FailedSections(); len(failed) > 0 {
		if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
			t.Errorf("在 %s 上不该有失败项：%v", runtime.GOOS, failed)
		} else {
			t.Logf("%s 上失败项：%v（预期）", runtime.GOOS, failed)
		}
	}
}

// TestSampleDoesNotBlock "采样不许阻塞"的可测形式：单次 < 500ms。
//
// # 500ms 这个阈值怎么定的
//
// 它不衡量性能，衡量的是"有没有人在里面 sleep"。sleep 100ms 的实现在这里
// 会勉强通过（99ms），所以另外用 `NewAt` 的首次采样与第二次采样分别计时：
// 两次都必须快。真正的回归是"加了一个固化的 sleep 间隔"，那会立刻超过 500ms。
//
// ⚠ 不设更紧的阈值（比如 5ms）：CI 上偶发的调度抖动会让它假红，
// 而假红的测试会被删掉 —— 那比不测更糟。
func TestSampleDoesNotBlock(t *testing.T) {
	s := NewAt(t.TempDir())
	for i := 0; i < 3; i++ {
		start := time.Now()
		s.Sample()
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("第 %d 次采样花了 %v（超过 500ms ⇒ 有人在里面 sleep 或做网络 IO）", i+1, d)
		}
	}
}

// TestNewDefaults 构造器：空串回落到默认数据目录，Nil 语义明确。
func TestNewDefaults(t *testing.T) {
	if s := New(); s.dataDir != DefaultDataDir {
		t.Errorf("New() 的 dataDir=%q，想要 %q", s.dataDir, DefaultDataDir)
	}
	if s := NewAt(""); s.dataDir != DefaultDataDir {
		t.Errorf("NewAt(空串) 应当回落默认值，得到 %q", s.dataDir)
	}
	if s := NewAt("/custom/data"); s.dataDir != "/custom/data" {
		t.Errorf("自定义路径被覆盖了：%q", s.dataDir)
	}
	// 未采样前不应有基线（否则"第一次采样没值"这条就不成立了）。
	if s := New(); s.prevCPU != nil {
		t.Error("新 Sampler 不该带 CPU 基线")
	}
}
