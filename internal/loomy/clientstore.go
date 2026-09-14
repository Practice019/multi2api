// clientstore.go 读**本机 Loomy 客户端**的 Local Storage。
//
// # 为什么本包需要直接读客户端的数据目录
//
// 这是三次实测逼出来的，不是"顺手加的便利功能"：
//
//  1. **额度**（用户报"额度没做"）：Loomy 的积分余额**不在模型代理端点**
//     （`loomyad.xunfei.cn/api/v1`）上 —— 实测打了 17 条候选路径
//     （/points、/user/points、/balance、/quota…）**全部 404**。
//     它的真实来源是客户端渲染层通过 IPC（`window.electronAPI.points.*`）
//     问 Electron **主进程**要的，HTTP 细节在主进程 bundle 里。
//     而客户端把结果**明文缓存**在本机 Local Storage 的
//     `loomy-points-summary` 键下。
//
//  2. **添加账号**（用户报"没有添加账号的流程"）：登录发生在桌面客户端里，
//     网关这一侧没有可发起的 OAuth 流程（没有授权页、没有回调）。
//     唯一可行的入口就是**读客户端已经登录好的那一份 session**。
//
//  3. **Token 到期**（用户报"token 到期时间没做"）：这个键的 JSON 里
//     **没有**任何 expires/ttl 字段 —— 所以那个问题的正确答案不是"补一列"，
//     而是"如实说它不过期"（见 accountview.go 的 NeverExpires）。
//
// 三件事共用同一个数据源，所以读取逻辑收在本文件里。
//
// # 数据在哪
//
// Electron 的 Local Storage 就是 leveldb。登录态与积分摘要都以 JSON 字符串
// 直接存在值里（明文，未加密），键名分别是：
//
//	loomy-auth-session     {"phone","maskedPhone","session","userid","loggedInAt"}
//	loomy-points-summary   {"balance","dailyBalance","updatedAt",…}
//
// Windows 实测路径：`%APPDATA%\Loomy\Local Storage\leveldb\000003.log`
// （`os.UserConfigDir()` 在三个平台上分别给出 %APPDATA% / ~/Library/Application Support
//
//	/ ~/.config，所以同一个拼接式在 Windows/macOS/Linux 上都成立）。
//
// # 为什么是"扫原始字节"而不是用一个 leveldb 库
//
// 三条理由，按重要性排：
//
//  1. **不能给一个纯 HTTP 转发上游加一个存储引擎依赖**。本包现在的依赖只有
//     standard library；leveldb 会带来 cgo 或一大坨纯 Go 实现。
//  2. **不能对客户端的数据目录加锁**。真正的 leveldb 打开会创建 LOCK 文件并
//     **持锁**，而客户端自己随时可能在跑 —— 用一个只读扫描器不会干扰它。
//  3. 客户端写的就是**明文 JSON**，没有需要解析的编码（这点与 cookie 库
//     不同：`Network\Cookies` 是加密的，手册第 2.2 节实测确认它为空）。
//
// 代价是：正则/扫描会看到写入过程中的**半截记录**。所以下面的解析器对
// 每一个候选片段都要求"是一个结构完整的 JSON 对象"，不完整就跳过 ——
// 宁可少一条候选，也不要解出一份字段错位的凭证。
package loomy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Local Storage 里的键名。
//
// 抽成常量而不是各处写字面量：它们是与**客户端**之间的契约
// （上游改键名时这里必须跟着改），只有一份才查得到。
const (
	keyAuthSession   = "loomy-auth-session"
	keyPointsSummary = "loomy-points-summary"
)

// clientStoreRel 客户端数据目录相对「用户配置目录」的路径段。
//
// 三个平台共用：见文件头注释。
var clientStoreRel = []string{"Loomy", "Local Storage", "leveldb"}

// storeFileSuffixes leveldb 数据文件的候选后缀。
//
//	刻意**不**包含裸 `LOG` / `LOG.old`：那是 leveldb 自己的运行日志
//
// （记录 compaction 事件），里面没有用户数据。`*.log` 的 glob 只匹配
// 以 `.log` 结尾的名字，恰好不会命中它们 —— 这条依赖是隐式的，
// 所以写在这里说明白。
var storeFileSuffixes = []string{"*.log", "*.ldb", "*.sst"}

const (
	// maxStoreFileBytes 单文件读取上限。
	//
	// 实测 `000003.log` 是 11 KB；`.ldb` 在 compaction 之后可能大得多，
	// 但对"找两个键"来说没有读完整文件的必要。
	// 设一个上界是为了避免在一个病态大文件上把内存吃光 ——
	// 本函数会被账号列表渲染路径（每个账号一次）调用。
	maxStoreFileBytes = 32 << 20
	// keyValueGap 键与值之间允许的最大字节距离。
	//
	// # 为什么必须是"距离"而不是 `\s*`
	//
	// leveldb 在 key 与 value 之间插入 `\x01` 等**不可打印分隔符**
	//（手册第 5.1 节实测记录：所以 python 版用的是 `.{0,20}`）。
	// `\s*` 匹配不到 `\x01`，会一个都读不出来。
	//
	// 64 字节足够宽松（实测间隔在 20 以内），又不会让"某个键名恰好出现在
	// 一段无关数据里"捞出十万八千里外的一个 JSON。
	keyValueGap = 64
)

// DefaultClientStoreDir 返回本机 Loomy 客户端的 Local Storage 目录。
//
// 目录不存在时返回**空串**（而不是一个不存在的路径）：
// 调用方据此区分"本机没装客户端"与"装了但读不到"，
// 前者是正常状态（网关可以跑在另一台机器上），后者才值得记日志。
func DefaultClientStoreDir() string {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		return ""
	}
	p := filepath.Join(append([]string{base}, clientStoreRel...)...)
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return ""
	}
	return p
}

// LocalPoints 客户端缓存的积分摘要。
//
// 字段名与客户端原文**逐字对应**（`dailyBalance` 等），刻意不做改名：
// 这是同一份数据的两个投影，改名只会让"两边是不是同一个东西"
// 变成需要对照才能回答的问题。
//
// # 两个账本（手册第 4.3 节实测）
//
//	Balance      总余额 —— **不随自然日重置**（邀请/活动/充值累积）
//	DailyBalance 每日额度 —— 服务端按自然日**自动**重置为 5000
//
// 两者都在消耗时下降（实测 09-11 09:38：balance 5000→5500、
// dailyBalance 5000→4950）。选哪个展示见 quota_ext.go 的说明。
type LocalPoints struct {
	Balance              int64  `json:"balance"`
	DailyBalance         int64  `json:"dailyBalance"`
	DailyConsumedPoints  int64  `json:"dailyConsumedPoints"`
	DailyRemainingPoints int64  `json:"dailyRemainingPoints"`
	UpdatedAt            string `json:"updatedAt"`
}

// pointsWire 是积分摘要的**逐字段可选**形态。
//
// # 为什么用指针而不是直接解进 LocalPoints
//
// 实测同一天里客户端会写入**部分更新**的记录：
//
//	{"balance":5500,"dailyBalance":4950,"updatedAt":"…09:38:46…"}
//	{"balance":5500,"updatedAt":"…03:04:34…"}          ← 只有 balance
//
// 直接 `json.Unmarshal` 到 LocalPoints 时，"字段缺失"与"字段是 0"
// 无法区分：后一条会把 DailyBalance 覆盖成 0，而 0 在展示语义上
// 是"今日额度已用完" —— 一个**确定的、且错误**的结论。
//
// 指针让"缺失"在类型层面与 0 分开，于是可以只覆盖真正写了的字段。
// （与 admin.AccountView.TokenExpireSec 用指针的理由同源。）
type pointsWire struct {
	Balance              *int64 `json:"balance"`
	DailyBalance         *int64 `json:"dailyBalance"`
	DailyConsumedPoints  *int64 `json:"dailyConsumedPoints"`
	DailyRemainingPoints *int64 `json:"dailyRemainingPoints"`
	UpdatedAt            string `json:"updatedAt"`
}

// pointsFold 把多条候选按时间**升序**叠加成一个视图。
//
// # 为什么要"叠加"而不是"取最新一条"
//
// 因为更新是**部分**的（见 pointsWire 的注释）。取最新一条会丢掉
// 上一条里还没被覆盖的字段 —— 实测序列里最新那条可能只有 balance。
//
// 叠加的语义正是客户端的真实行为：每次写入只改它知道的那几个字段。
// 同一字段被后来的写入覆盖，没写过的保持上一次的值。
func pointsFold(list []pointsWire) *LocalPoints {
	if len(list) == 0 {
		return nil
	}
	// 按 UpdatedAt 升序（ISO8601 同格式下字符串序即时间序，
	// 与 Auth.LoggedInAt 的排序口径一致）。空时间视为最旧。
	sorted := make([]pointsWire, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].UpdatedAt < sorted[j].UpdatedAt
	})

	out := &LocalPoints{}
	var seen bool
	for _, w := range sorted {
		seen = true
		if w.Balance != nil {
			out.Balance = *w.Balance
		}
		if w.DailyBalance != nil {
			out.DailyBalance = *w.DailyBalance
		}
		if w.DailyConsumedPoints != nil {
			out.DailyConsumedPoints = *w.DailyConsumedPoints
		}
		if w.DailyRemainingPoints != nil {
			out.DailyRemainingPoints = *w.DailyRemainingPoints
		}
		if w.UpdatedAt != "" {
			out.UpdatedAt = w.UpdatedAt
		}
	}
	if !seen {
		return nil
	}
	return out
}

// ReadLocalAuth 从 dir 读客户端登录态。
//
// 返回 (凭证, 扫描来源描述, error)。第二个返回值是**参与扫描的文件名**
// （逗号分隔），用于日志与失败提示；一个候选片段都读到时才非空，
// 所以调用方必须把"空串"读成"数据文件里没有可读内容"，
// 而不是"目录是空的"（两者排障方向完全不同）。
//
//	目录为空 / 没有该键        → (nil, "", nil)   —— **不是错误**
//	键在但没有任何完整 JSON     → (nil, "", nil)   —— 同上（写入中的半截记录）
//	读目录本身失败              → (nil, "", err)
//
// 同一键多份时取 `loggedInAt` **最新**的一份 —— 与 LoadDir 的 pickWinners
// 同一条判据（客户端在同一键上追加写入，旧值并不会被物理删除）。
func ReadLocalAuth(dir string) (*Auth, string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, "", nil
	}
	text, files, err := readStoreText(dir)
	if err != nil {
		return nil, "", err
	}
	blobs := extractJSONAfter(text, keyAuthSession, keyValueGap)
	if len(blobs) == 0 {
		return nil, "", nil
	}
	best := (*Auth)(nil)
	for _, raw := range blobs {
		a, perr := ParseCredential([]byte(raw))
		if perr != nil {
			// 半截记录/无关片段：跳过。**不记日志** ——
			// 这是扫描式读取的常态（leveldb 里有历史与中间状态），
			// 每个都打一行会把真正的故障淹掉。
			continue
		}
		if best == nil || a.LoggedInAt > best.LoggedInAt {
			best = a
		}
	}
	if best == nil {
		return nil, "", nil
	}
	return best, strings.Join(files, ","), nil
}

// ReadLocalPoints 从 dir 读客户端缓存的积分摘要。
//
// 返回 (摘要, error)；读不到时返回 (nil, nil) —— 与 ReadLocalAuth 同口径，
// "读不到"是正常状态而不是故障。
func ReadLocalPoints(dir string) (*LocalPoints, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	text, _, err := readStoreText(dir)
	if err != nil {
		return nil, err
	}
	var wires []pointsWire
	for _, raw := range extractJSONAfter(text, keyPointsSummary, keyValueGap) {
		var w pointsWire
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			continue // 半截记录：跳过
		}
		// 至少要有一个数值字段，否则它只是名字像而已。
		if w.Balance == nil && w.DailyBalance == nil &&
			w.DailyConsumedPoints == nil && w.DailyRemainingPoints == nil {
			continue
		}
		wires = append(wires, w)
	}
	return pointsFold(wires), nil
}

// readStoreText 把 dir 下所有 leveldb 数据文件读成一段文本。
//
// 返回 (拼接后的文本, 参与拼接的文件名列表, error)。
//
// # 为什么把多个文件拼在一起
//
// 键的历史可能横跨 `.log`（memtable 的预写日志）与 `.ldb`（已落盘的 SST）。
// 必须一起看，只看 `.log` 会在客户端做过一次 compaction 之后**突然读不到**
// 任何东西 —— 那种"昨天还好今天没了"的表现最难归因。
//
// 拼接顺序按**文件修改时间升序**：新文件在后，与 extractJSONAfter
// 内部"后出现者更晚"的取值方向一致。
//
// # 单个文件读失败怎么办
//
// 跳过并继续（记一行日志）。客户端在跑的时候 leveldb 会滚动/重命名文件，
// 遇到一个刚被删掉的名字是常态，不该让整次读取失败。
func readStoreText(dir string) (string, []string, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return "", nil, fmt.Errorf("loomy: 读取客户端数据目录 %s 失败: %w", dir, err)
	}
	if !st.IsDir() {
		return "", nil, fmt.Errorf("loomy: 客户端数据目录 %s 不是目录", dir)
	}

	var paths []string
	for _, suf := range storeFileSuffixes {
		matches, gerr := filepath.Glob(filepath.Join(dir, suf))
		if gerr != nil {
			continue
		}
		paths = append(paths, matches...)
	}
	if len(paths) == 0 {
		return "", nil, nil
	}
	// 按修改时间升序；时间相同则按路径序，保证结果可复现。
	sort.SliceStable(paths, func(i, j int) bool {
		si, ei := os.Stat(paths[i])
		sj, ej := os.Stat(paths[j])
		if ei != nil || ej != nil {
			return paths[i] < paths[j]
		}
		if si.ModTime().Equal(sj.ModTime()) {
			return paths[i] < paths[j]
		}
		return si.ModTime().Before(sj.ModTime())
	})

	var b strings.Builder
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		f, oerr := os.Open(p)
		if oerr != nil {
			continue
		}
		//  用 LimitReader 而不是"先开一个 32 MiB 的 buf 再 Read"。
		//
		// 两次分配 32 MiB 的函数（ReadLocalAuth + ReadLocalPoints 各一次）
		// 会被账号列表的额度刷新路径按账号调用 —— 那是一台机器上
		// 一瞬间几百兆的**无意义**分配（实测数据文件只有 11 KB）。
		// LimitReader 按需增长，正常路径上分配量就是文件大小。
		buf, rerr := io.ReadAll(io.LimitReader(f, maxStoreFileBytes))
		_ = f.Close()
		if len(buf) == 0 {
			continue
		}
		// 无效字节按原样保留：JSON 片段本身是连续的，
		// 解码成字符串只是为了能对字节做子串查找。
		b.Write(buf)
		b.WriteByte('\n') // 文件之间插一个换行，避免两个文件的字节黏成一个假 JSON
		names = append(names, filepath.Base(p))
		_ = rerr // 到 EOF/被截断都按已有内容处理
	}
	return b.String(), names, nil
}

// extractJSONAfter 找出所有"紧跟在 key 之后"的完整 JSON 对象。
//
// # 为什么不用正则
//
// 手册的 python 版用的是 `r'loomy-auth-session.{0,20}(\{.*?"session".*?\})'`。
// 那条正则依赖"对象里没有嵌套花括号、且 `"session"` 出现在第一个字段附近"——
// 对当前的客户端数据成立，但 `.*?` 的终止条件由**内容**决定：
// 一旦 JSON 里出现 `}`（哪怕在字符串里），匹配就会**提前结束**，
// 产出一个语法合法但内容被切断的片段。
//
// 而"解出一份字段错位的凭证"是本项目最贵的一类缺陷（看起来成功，
// 实际用错值）。所以这里改用**配平扫描**：从 `{` 开始数括号，
// 并且识别字符串与转义 —— 与 JSON 词法一致，不依赖字段顺序。
func extractJSONAfter(text, key string, maxGap int) []string {
	var out []string
	from := 0
	for {
		i := strings.Index(text[from:], key)
		if i < 0 {
			break
		}
		i += from
		from = i + len(key)

		limit := from + maxGap
		if limit > len(text) {
			limit = len(text)
		}
		if from >= limit {
			continue
		}
		j := strings.IndexByte(text[from:limit], '{')
		if j < 0 {
			continue
		}
		start := from + j
		end := jsonObjectEnd(text, start)
		if end < 0 {
			continue
		}
		out = append(out, text[start:end])
	}
	return out
}

// jsonObjectEnd 返回从 start（必须是 '{'）起的第一个**配平** JSON 对象的结束下标+1。
//
// 配平规则与 JSON 词法一致：字符串内的花括号不计入深度，
// `\` 转义的下一个字符被跳过（否则 `"\\"` 之后的引号会被误判为字符串结束）。
// 找不到配平点（截断的记录）时返回 -1。
func jsonObjectEnd(s string, start int) int {
	if start >= len(s) || s[start] != '{' {
		return -1
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// storeDayChanged 报告 updatedAt 是否**不属于** now 所在的 CST 自然日。
//
// # 为什么按 CST 而不是本机时区
//
// 每日额度的重置是**服务端**行为，而服务端在 CST（与 errorclassifier.go
// 的 ResetAt 同一个时区常量）。用本机时区判断会在跨时区部署时
// 得出"还没跨天"的结论，而服务端那边已经重置了 —— 显示出来的
// dailyBalance 就是**昨天**的数。
//
// 解析失败（字段缺失/格式变了）一律视为"跨天"：
// 拿不到时间戳时不能假装它是今天的（那会把一份不知道多旧的数
// 当成当前额度展示）。
func storeDayChanged(updatedAt string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(updatedAt))
	if err != nil {
		return true
	}
	y1, m1, d1 := t.In(cstZone).Date()
	y2, m2, d2 := now.In(cstZone).Date()
	return y1 != y2 || m1 != m2 || d1 != d2
}
