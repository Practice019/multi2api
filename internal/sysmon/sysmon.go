// sysmon.go —— 系统资源采样器：一次调用返回一个快照，不碰全局可变状态。
//
// # 为什么需要这个包（PLAN-sql-unify `3 T9 的前置）
//
// 系统状态页要显示「资源与入口」那一组：CPU / 内存 / 磁盘 / goroutine / 堆 / GC / uptime。
// 改造前这些数**一个都拿不到**：本仓没有任何采集代码，也没有 gopsutil 这类依赖。
//
// # 三条硬约束（都来自「单二进制 + 零依赖」这条部署优势）
//
//  1. **只用 stdlib**。go.mod 里没有 golang.org/x/sys（只有 redis / wazero / crypto），
//     所以 Unix 侧用 stdlib 的 `syscall.Statfs` + 读 `/proc`，Windows 侧用
//     stdlib 的 `syscall.NewLazyDLL` 调 kernel32。**不新增任何依赖**
//     —— 这是 PLAN-sql-unify D1 铁律（不许 cgo）的延伸：一个"看资源"的小功能
//     不该把依赖图撑大。
//  2. **一次调用一个快照**。不返回"最新的全局状态"，因为那需要一把全局锁 + 一个后台
//     goroutine + 一个"多久算过期"的约定；三者都是可以出错的地方（hyxapi 的
//     `systemStatusMaxAge` 就是为了兜住"监控 goroutine 挂了但读者还在读旧值"）。
//     直接同步采样只花几微秒，不值得为它引入一条后台生命周期。
//  3. **单项失败不拖垮整体**。每项自带一个 OK 布尔。页面可以把失败的项显示成"—"，
//     而不是把一个 0 当成"CPU 0%"或"磁盘满了"。⚠ 这是本文件与"尽量少字段"的取舍中
//     刻意选择的一边：**宁可字段多一个 OK，也不要让读者去猜 0 是什么意思**。
//
// # 为什么 CPU 必须由调用方喂两次采样
//
// 整机 CPU 占用率**没有瞬时值**：内核只提供累计的 CPU 时间计数器
// （Linux 的 `/proc/stat`、Windows 的 `GetSystemTimes`）。占用率只能是
// "两次读数之间的差值比"。所以：
//
//	第一次 Sample()  → CPUOK=false（只建立了基线，没有可比的区间）
//	第二次及以后      → CPUOK=true
//
// 常见错误写法是"在采样函数里 sleep 100ms 再读一次" —— 那会让**每一次**
// 请求/页面刷新都多花 100ms，而状态页往往每秒都在刷。本包不 sleep：
// 基线存在 Sampler 实例里，两次 Sample 的时间间隔就是调用方的自然节奏
// （状态页 2 秒一次、日志按需触发），差值天然成立。
//
// # 为什么基线放在实例里而不是包级变量
//
// 包级可变基线会让**两个读者互相偷走对方的时间窗口**：页面每秒采一次、
// 巡检每分钟采一次，巡检那次算出来的 60 秒窗口就被页面那次切成两半，
// 得到一个既不是 1 秒也不是 60 秒的数字，而且**没有任何迹象**表明它错了。
// 实例隔离后，每个读者按自己的节奏算自己的差值（有测试钉住：TestSamplerIsolation）。
package sysmon

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultDataDir 默认的"数据盘"探测路径。
//
// 与 cmd/server 的约定一致（main.go 里所有可写状态都在 `data/` 下：
// accounts.json / sessions.json / apikeys.json / sticky-bindings.json）。
// 磁盘口径按 PLAN 的要求取 **data/ 所在卷** 的可用空间，而不是进程 CWD
// —— 两者在多盘机器上常常不是同一个卷。
const DefaultDataDir = "data"

// processStart 进程启动时刻，uptime 的基准。
//
// ⚠ 这是本包**唯一**的包级状态，且**只写一次**（包初始化赋值，此后无任何函数写它）。
// 因此它不构成"全局可变状态"：不存在并发写，也不存在测试之间互相污染。
// 之所以不做成"调用方传入"：那会让每个调用点各自记一份启动时间，
// 而其中一份必然会记错（记在 New() 里而不是 main() 里，uptime 就少算了整个初始化阶段）。
var processStart = time.Now()

// Snapshot 是某一时刻的整机资源快照。
//
// # 为什么字段是扁平的（而不是 CPU{...} Mem{...} 嵌套）
//
// 这份结构会被直接编成 JSON 喂给状态页，字段名就是页面上的绑定名
// （cpu_pct / mem_pct / disk_pct …）。嵌套会让前端多一层 data.cpu.pct 的路径，
// 而这一层**没有任何信息量**：每项本来就只有一个数。
// 扁平的代价是字段多，但那正是这份数据的本质。
//
// # 零值的含义
//
// 某项 OK=false 时，它的数值字段**保持零值**，且**不代表真实读数**。
// ⚠ 不允许把 0 解释成"CPU 空闲"或"磁盘空"：调用方必须先看 OK 再决定怎么显示。
type Snapshot struct {
	// At 采样时刻（Unix 毫秒）。状态页的"上次更新 N 秒前"倒计时由它反推
	// （PLAN-sql-unify `3 T9 要求"倒计时从 dataUpdatedAt 反推"，不另设定时器）。
	At int64 `json:"at"`

	// CPUPct 整机 CPU 占用率（0..100）。
	//
	// ⚠ **非容器口径**：这里读的是整机计数器。容器里它会包含同宿主机其它进程的负载，
	// 因而可能远高于本进程的真实占用。要容器口径得读 cgroup（hyxapi 的
	// common/system_monitor.go:112 那套），那是另一件事，本包刻意不做 ——
	// 混在一个字段里会让"到底谁在忙"永远说不清。
	CPUPct float64 `json:"cpu_pct"`
	// CPUOK 见包注释：第一次采样没有基线，这里是 false。
	CPUOK bool `json:"cpu_ok"`

	MemPct        float64 `json:"mem_pct"`
	MemTotalBytes uint64  `json:"mem_total_bytes"`
	// MemUsedBytes = 总量 - 可用量（**不是** 总量 - 空闲量）。
	//
	// 用"可用"（Linux 的 MemAvailable、Windows 的 ullAvailPhys）而不是"空闲"，
	// 是因为空闲量不含可回收的页缓存，会让一台正常的机器看起来内存爆满。
	MemUsedBytes uint64 `json:"mem_used_bytes"`
	MemOK        bool   `json:"mem_ok"`

	DiskPct        float64 `json:"disk_pct"`
	DiskTotalBytes uint64  `json:"disk_total_bytes"`
	// DiskFreeBytes 口径 = freeBytesAvailable（Windows） / Bavail（unix），
	// 即**本进程可用**的空间，而不是"整卷空闲"。
	//
	// ⚠ 二者在配额环境下不同：比如卷只剩 10GB，但本用户配额只剩 500MB 时，
	// 报 10GB 会让"写盘失败"看起来毫无征兆。用可用量才能提前预警。
	DiskFreeBytes uint64 `json:"disk_free_bytes"`
	// DiskPath 实际探测的路径。
	//
	// 数据目录不存在时（首次启动、data/ 还没建）会退回上级或临时目录，
	// 那时**可能探测到了另一个卷**。把这个路径一起返回，页面/日志才能看出
	// "这是一次降级采样"，而不是把一个错盘的数当成数据盘（见 probeDiskPath）。
	DiskPath string `json:"disk_path,omitempty"`
	DiskOK   bool   `json:"disk_ok"`

	// Goroutines 当前 goroutine 数。趋势比绝对值有用：
	// 稳态下它应当围绕一个数小幅波动，**单调上涨**意味着有泄漏（通常是忘了 cancel 的流）。
	Goroutines int `json:"goroutines"`
	// HeapAllocBytes 当前存活堆（runtime.MemStats.HeapAlloc）。
	//
	// ⚠ 用它而不是 HeapSys：后者是"向 OS 要了多少"，只增不减，
	// 一条平滑上涨的直线看不出任何问题。
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	// GCCount 累计 GC 次数。
	GCCount uint32 `json:"gc_count"`
	// GCPauseNS **最近一次** GC 的暂停时长（纳秒）。
	//
	// 为什么取"最近一次"而不是累计总量：累计值只会随时间上涨，
	// 看它无法回答"刚才那次 GC 卡了多久" —— 而后者才是状态页要回答的问题。
	// 累计值另放 GCPauseTotalNS。
	GCPauseNS uint64 `json:"gc_pause_ns"`
	// GCPauseTotalNS 自进程启动以来的 GC 暂停累计（纳秒）。
	//
	// 与 GCPauseNS 并存是有意的：比值（total/uptime）才是"GC 拖了多少后腿"的指标，
	// 而单次值是"刚才那一跳卡不卡"的指标。两个问题，两个字段。
	GCPauseTotalNS uint64 `json:"gc_pause_total_ns"`

	// UptimeSec 进程启动至今（秒，浮点以保留亚秒精度）。
	UptimeSec float64 `json:"uptime_sec"`
}

// FailedSections 返回采样失败的项（"cpu" / "mem" / "disk"），按固定顺序。
//
// # 为什么需要它
//
// 页面拿到一个 cpu_ok=false 的零值快照，要么写一堆 if 分支，
// 要么（更常见、更糟）忘了判 OK，把 0 显示成"CPU 0%"。
// 让调用方一次拿到"哪些项不可信"，就能统一渲染成"—"并附一句"采集不可用"。
func (s Snapshot) FailedSections() []string {
	out := make([]string, 0, 3)
	if !s.CPUOK {
		out = append(out, "cpu")
	}
	if !s.MemOK {
		out = append(out, "mem")
	}
	if !s.DiskOK {
		out = append(out, "disk")
	}
	return out
}

// Sampler 采样器。**不要复制**（内含 mutex 与 CPU 基线）。
//
// # 为什么是结构体而不是一组包级函数
//
// 需要跨调用记住两件事：CPU 基线（见包注释）、以及探测到的磁盘路径
// （探测要 stat 若干候选路径，没必要每次采样都重来）。这两件事都是
// **每个读者自己**的状态，所以放实例里。
type Sampler struct {
	mu       sync.Mutex
	dataDir  string
	prevCPU  *cpuTimes
	diskPath string // 首次 Sample 时惰性探测并缓存
}

// New 用默认数据目录（data/）构造采样器。
func New() *Sampler { return NewAt(DefaultDataDir) }

// NewAt 指定"数据盘"探测起点（通常是 data/ 的绝对路径）。
//
// 传空串等价于 New()。
func NewAt(dataDir string) *Sampler {
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	return &Sampler{dataDir: dataDir}
}

// Sample 采一次快照。**不 sleep、不发网络、不启动 goroutine**。
//
// 耗时由若干次 syscall / 小文件读构成（微秒级）。
// 唯一的例外是磁盘探测：若 data/ 落在网络盘上，os.Stat 可能阻塞到超时。
// 这是"要报数据盘"这个需求本身带来的成本，本包不掩盖它（见 probeDiskPath）。
func (s *Sampler) Sample() Snapshot {
	now := time.Now()
	snap := Snapshot{
		At:        now.UnixMilli(),
		UptimeSec: now.Sub(processStart).Seconds(),
	}

	// ── runtime 指标：先采，因为它不受 OS 接口影响，任何平台都有 ──
	sampleRuntime(&snap)

	// ── CPU：需要基线，第一次必然拿不到 ──
	s.mu.Lock()
	if cur, ok := readCPUTimes(); ok {
		if s.prevCPU != nil {
			snap.CPUPct, snap.CPUOK = cpuPercent(*s.prevCPU, cur)
		}
		// ⚠ 基线**无论如何**都要更新（即使这次算不出百分比）。
		// 只在"算成功"时更新会让一个偶发的计数器异常把基线永久钉在很久以前，
		// 之后那次采样会覆盖一个超长的窗口，得到一个平滑到失真的平均值。
		c := cur
		s.prevCPU = &c
	}
	diskPath := s.diskPathLocked()
	s.mu.Unlock()

	// ── 内存 ──
	if total, used, ok := readMemory(); ok && total > 0 && used <= total {
		snap.MemOK = true
		snap.MemTotalBytes = total
		snap.MemUsedBytes = used
		snap.MemPct = percent(used, total)
	}

	// ── 磁盘（data/ 所在卷）──
	snap.DiskPath = diskPath
	if total, free, ok := readDisk(diskPath); ok && total > 0 && free <= total {
		snap.DiskOK = true
		snap.DiskTotalBytes = total
		snap.DiskFreeBytes = free
		// ⚠ 磁盘百分比报的是**已用**，与内存一致（页面两条阈值条同一套语义）。
		snap.DiskPct = percent(total-free, total)
	}

	return snap
}

// diskPathLocked 惰性探测并缓存磁盘探测路径。调用方须持有 s.mu。
func (s *Sampler) diskPathLocked() string {
	if s.diskPath == "" {
		s.diskPath = probeDiskPath(s.dataDir)
	}
	return s.diskPath
}

// probeDiskPath 找出"最接近 data/ 的那个**存在**的目录"。
//
// # 为什么不直接把 dataDir 交给系统调用
//
// GetDiskFreeSpaceExW / Statfs 对一个**不存在**的路径会失败（Windows 上尤其如此），
// 而首次启动时 data/ 往往还没建（它在第一次写 JSON 时才出现）。
// 于是这里按 data/ → . → 临时目录 的顺序找一个存在的目录：
//
//	能找到 data/      → 精确符合"data 所在卷"的口径（正常情况）
//	只能找到 .        → 同一卷的概率极高（data/ 就在 CWD 下）
//	只能到临时目录    → ⚠ 可能**是另一个卷**，所以结果里带 DiskPath 让读者可见
//
// 最后这步刻意不返回失败：一个"可能是别的卷"的磁盘数，比"状态页磁盘项永远 —"
// 有用得多（前者至少能看出"这台机器磁盘快满了"，后者什么都看不出）。
func probeDiskPath(dataDir string) string {
	cands := make([]string, 0, 3)
	if dataDir != "" {
		cands = append(cands, dataDir)
	}
	cands = append(cands, DefaultDataDir, ".")

	for _, c := range cands {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			return abs
		}
	}
	return os.TempDir()
}

// sampleRuntime 填 goroutine / 堆 / GC / uptime 这些"进程自身"的指标。
//
// # ReadMemStats 的 STW 成本（为什么仍然接受）
//
// runtime.ReadMemStats 会触发一次极短的 stop-the-world（微秒量级）。
// 严格说这与"采样不许阻塞"相冲突，但：
//
//   - 这是**唯一**能拿到 HeapAlloc / NumGC / PauseNs 的 stdlib 途径；
//   - 它的 STW 与 GC 周期成比例，不在分配热路径上时通常 < 10us；
//   - 调用频率由调用方决定（状态页秒级），不是每请求一次。
//
// ⚠ 若将来有人把它接到**每个请求**上，请改成 runtime/metrics（无 STW）
// 或加采样间隔闸门 —— 这条不是假设，是给下一个改这里的人的提醒。
func sampleRuntime(snap *Snapshot) {
	snap.Goroutines = runtime.NumGoroutine()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	snap.HeapAllocBytes = ms.HeapAlloc
	snap.GCCount = ms.NumGC
	snap.GCPauseTotalNS = ms.PauseTotalNs
	if ms.NumGC > 0 {
		// PauseNs 是长度 256 的**环形缓冲**：最近一次暂停在 (NumGC-1)%256。
		// ⚠ 直接读 PauseNs[0] 是初始化阶段的旧值，而不是最近一次 ——
		// 那会永远显示进程刚启动时那次 GC 的耗时。
		snap.GCPauseNS = ms.PauseNs[(ms.NumGC+255)%256]
	}
}

// cpuTimes 是整机 CPU 时间计数器的一次读数（单位由平台决定，只需要可比）。
//
// idle 必须是 total 的**子集**：
//
//	Linux  total = 各状态字段之和（含 idle 与 iowait）
//	Windows total = kernel + user（GetSystemTimes 的 kernel **已含 idle**）
//
// 两个平台都满足这个前提，所以百分比用同一个公式算（见 cpuPercent）。
type cpuTimes struct {
	idle  uint64
	total uint64
}

// cpuPercent 由两次读数算整机占用率。
//
// # 全部分支都返回 ok=false，而不是"猜一个值"
//
//	cur.total <= prev.total   计数器没前进（或回绕）⇒ 没有区间可比
//	cur.idle < prev.idle      计数器回绕 ⇒ 差值无意义
//	idle 增量 > total 增量   两个计数器的步长不一致 ⇒ 读数不可信
//
// ⚠ 这些情况在实际运行里很少见（多半是虚拟机时钟跳变、或 32 位计数器回绕），
// 但一旦出现，返回 0 会让页面显示"CPU 0%"——一个**看起来正常**的错误结论。
// 返回 ok=false 才会显示"—"，那才说得上是"不瞎猜"。
func cpuPercent(prev, cur cpuTimes) (float64, bool) {
	if cur.total <= prev.total {
		return 0, false
	}
	if cur.idle < prev.idle {
		return 0, false
	}
	totalDelta := cur.total - prev.total
	idleDelta := cur.idle - prev.idle
	if idleDelta > totalDelta {
		return 0, false
	}
	pct := (1 - float64(idleDelta)/float64(totalDelta)) * 100
	return clampPct(pct), true
}

// parseProcStat 从 /proc/stat 全文里解析第一行 `cpu`（不是 `cpu0`）。
//
// 拆成独立函数是为了能在**任何平台**上用固定的文本测试（Windows 开发机上
// 没有 /proc/stat，但解析逻辑必须被测到 —— 见 sysmon_test.go 的
// TestParseProcStat）。
func parseProcStat(content string) (cpuTimes, bool) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // 去掉 "cpu"
		if len(fields) < 4 {
			// 至少要有 user/nice/system/idle 四个。更少的行说明格式变了，
			// 宁可 ok=false 也不要用半份数据算出一个错数。
			return cpuTimes{}, false
		}
		var vals [8]uint64
		for i := 0; i < 8 && i < len(fields); i++ {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return cpuTimes{}, false
			}
			vals[i] = v
		}
		var total uint64
		for _, v := range vals {
			total += v
		}
		// vals[3]=idle, vals[4]=iowait（后者在老内核上可能缺，缺则视为 0）
		idle := vals[3] + vals[4]
		if idle > total {
			return cpuTimes{}, false
		}
		return cpuTimes{idle: idle, total: total}, true
	}
	return cpuTimes{}, false
}

// parseMeminfo 解析 /proc/meminfo，返回 (总量, 可用量)，单位为字节。
//
// /proc/meminfo 的数值单位是 **kB**（不是字节，也不是 KiB —— 但实际是 1024），
// 所以统一乘 1024。⚠ 这是最容易漏的一步：忘了乘，磁盘/内存会显示成 1/1024。
func parseMeminfo(content string) (total, avail uint64, ok bool) {
	var memFree, buffers, cached uint64
	var haveTotal, haveAvail bool

	for _, line := range strings.Split(content, "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(val)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		const kb = 1024
		switch key {
		case "MemTotal":
			total, haveTotal = n*kb, true
		case "MemAvailable":
			avail, haveAvail = n*kb, true
		case "MemFree":
			memFree = n * kb
		case "Buffers":
			buffers = n * kb
		case "Cached":
			cached = n * kb
		}
	}
	if !haveTotal {
		return 0, 0, false
	}
	if !haveAvail {
		// 老内核（< 3.14）没有 MemAvailable。
		// 近似式对齐内核自己的算法：Free + Buffers + Cached。
		// ⚠ 刻意**不**减 Shmem（那是 tmpfs 占用的，不可回收），
		// 保守方向：宁可少报可用量（更早预警），不要多报（掩盖内存压力）。
		avail = memFree + buffers + cached
	}
	return total, avail, true
}

// percent 算 a/b*100 并夹到 [0,100]。
//
// ⚠ 夹取不是多余的：内核报告的"可用量"在极端情况下会略大于"总量"
// （采样跨了一次热插拔 / 计数器来自不同时刻），不夹会得到 100.3% 这种
// 出现在页面上的怪数字。
func percent(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return clampPct(float64(a) / float64(b) * 100)
}

// clampPct 把百分比夹到 [0,100]。
// 刻意**不四舍五入**：精度由展示层决定（不同页面要 1 位或 2 位小数），
// 在这里 round 一次就再也拿不回来了。
func clampPct(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
