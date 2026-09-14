// clientstore_test.go —— 「读本机 Loomy 客户端 Local Storage」的守卫。
//
// # 这组测试守的是什么
//
// 本文件测的代码是**扫描式解析**：在一坨二进制里找两个键、再把它们后面的
// JSON 抠出来。这类代码的失败形态是**静默**的 —— 解出一个字段错位的凭证、
// 或者少读一个字段（把"今日额度 4950"读成 0），界面上都看不出来。
//
// 所以每条断言都对着一个**具体的、真实数据里出现过的形状**：
//
//	键与值之间隔着 \x01 等不可打印字节（手册第 5.1 节的坑）
//	JSON 里有嵌套花括号 / 字符串里带 `}`
//	写入过程中的半截记录（leveldb 追加写的常态）
//	同一键的多条历史（旧值不会被物理删除）
//	积分摘要的**部分更新**（有的记录只有 balance，没有 dailyBalance）
package loomy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// storeRecord 造一条贴近真实 leveldb 的记录：键 + 间隔噪声 + 值。
//
// # 为什么必须有那个"间隔噪声"
//
// 真实的 leveldb 在 key 与 value 之间插入 `\x01` 等不可打印字节。
// 手册第 5.1 节实测发现用 `\s*` 匹配**一个都读不出来**，
// 所以才改用 `.{0,20}`。
//
// 如果测试里把键与值直接拼起来（零间隔），那种"只认零间隔"的实现
// 会**全绿通过** —— 而它在真机上什么都读不到。所以这里的噪声是
// 必须的，不是装饰。
func storeRecord(key, value string) string {
	// 4 字节模拟 checksum/seq + 1 字节类型 + 若干 0x01 分隔
	return "\x00\x00\x00\x00\x01" + strings.Repeat("\x01", 8) + key +
		"\x01\x01\x01" + value + "\x00"
}

// storeSessionJSON 造一份客户端登录态 JSON（字段与实测原样一致：全小写 userid）。
func storeSessionJSON(session, userid, maskedPhone, loggedInAt string) string {
	return `{"phone":"` + maskedPhone + `","maskedPhone":"` + maskedPhone +
		`","session":"` + session + `","userid":"` + userid +
		`","loggedInAt":"` + loggedInAt + `"}`
}

// writeStore 把记录写成一个 leveldb 数据文件，返回目录。
func writeStore(t *testing.T, records ...string) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	for _, r := range records {
		b.WriteString(r)
	}
	if err := os.WriteFile(filepath.Join(dir, "000003.log"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestReadLocalAuthReadsSessionAcrossBinaryGap 键与值之间有二进制噪声也能读到。
func TestReadLocalAuthReadsSessionAcrossBinaryGap(t *testing.T) {
	dir := writeStore(t,
		storeRecord(keyAuthSession,
			storeSessionJSON(fixtureSession, "260911173523492332", "150****3411",
				"2026-09-11T09:35:22.593Z")))
	a, _, err := ReadLocalAuth(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatal("★ 读不到登录态 —— 键与值之间的 \\x01 分隔被当成了不可跨越的间隔" +
			"（手册第 5.1 节实测过的坑）")
	}
	if a.Session != fixtureSession {
		t.Errorf("session = %q，want %q", a.Session, fixtureSession)
	}
	if a.UID != "260911173523492332" {
		t.Errorf("UID = %q（应当取 userid）", a.UID)
	}
	if a.Nickname != "150****3411" {
		t.Errorf("Nickname = %q（应当取 maskedPhone）", a.Nickname)
	}
}

// TestReadLocalAuthPicksNewestLogin 同一键有多条历史时取**最新**的一条。
//
// 客户端在同一个键上追加写入（旧值不会被物理删除），所以数据文件里
// 会同时存在多个版本的登录态。取错版本意味着用一个**已经作废的 session**
// 去发请求 —— 表现是"明明刚登录过，却一直 401"。
func TestReadLocalAuthPicksNewestLogin(t *testing.T) {
	old := "11111111111111111111111111111111"
	newer := "22222222222222222222222222222222"
	dir := writeStore(t,
		storeRecord(keyAuthSession, storeSessionJSON(old, "OLD", "100****0001",
			"2026-09-11T09:35:22.593Z")),
		storeRecord(keyAuthSession, storeSessionJSON(newer, "NEW", "100****0002",
			"2026-09-14T08:15:28.627Z")),
	)
	a, _, err := ReadLocalAuth(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a == nil || a.UID != "NEW" {
		t.Fatalf("应当取 loggedInAt 最新的那条（NEW），实际 %+v", a)
	}
	if a.Session != newer {
		t.Errorf("session = %q，want %q（取错了版本）", a.Session, newer)
	}
}

// TestReadLocalAuthSkipsTruncatedRecords 半截记录必须被跳过，而不是解出残骸。
//
// # 为什么这条重要
//
// leveldb 是**追加写**的：随时可能读到某次写入的中间状态。
// 一个"尽力解析"的实现会把 `{"session":"e094ffa6` 这种残片当成凭证
// （session 字段确实存在！），于是池子里出现一个 session 被截断的账号 ——
// 而它在第一次请求前看起来完全正常。
func TestReadLocalAuthSkipsTruncatedRecords(t *testing.T) {
	good := "33333333333333333333333333333333"

	t.Run("只有半截记录 → 读不到，但不是错误", func(t *testing.T) {
		dir := writeStore(t,
			storeRecord(keyAuthSession,
				`{"phone":"150","session":"`+good[:10]))
		a, _, err := ReadLocalAuth(dir)
		if err != nil {
			t.Fatalf("半截记录不该报错（它是追加写的常态）：%v", err)
		}
		if a != nil {
			t.Fatalf("★ 从半截记录里解出了凭证：%+v —— "+
				"那会往池子里塞一个 session 被截断的账号", a)
		}
	})

	t.Run("半截 + 完整 → 用完整的那条", func(t *testing.T) {
		dir := writeStore(t,
			storeRecord(keyAuthSession, `{"session":"`+good[:10]),
			storeRecord(keyAuthSession, storeSessionJSON(good, "GOOD", "150****3411",
				"2026-09-11T09:35:22.593Z")))
		a, _, err := ReadLocalAuth(dir)
		if err != nil {
			t.Fatal(err)
		}
		if a == nil || a.Session != good || a.UID != "GOOD" {
			t.Fatalf("应当忽略半截、用完整的那条，实际 %+v", a)
		}
	})
}

// TestExtractJSONAfterHandlesBracesInsideStrings 字符串里的 `}` 不能提前结束对象。
//
// # 这是"为什么不用正则"的直接证据
//
// 手册的 python 版用 `(\{.*?"session".*?\})`；`.*?` 的终止条件由**内容**决定，
// 所以只要 JSON 里出现一个 `}`（哪怕在字符串里 —— 比如一个错误消息、
// 一个昵称），匹配就会在那里**提前结束**，产出一个语法合法但内容被切断的片段。
func TestExtractJSONAfterHandlesBracesInsideStrings(t *testing.T) {
	// 值里带 `}` 和转义引号 —— 两者都会骗过朴素的正则。
	value := `{"session":"aa","note":"a } b","q":"say \"}\" now","tail":"ok"}`
	text := "prefix" + keyAuthSession + "\x01\x01" + value + "suffix"
	blobs := extractJSONAfter(text, keyAuthSession, keyValueGap)
	if len(blobs) != 1 {
		t.Fatalf("应当抠出 1 个对象，得到 %d 个：%v", len(blobs), blobs)
	}
	if blobs[0] != value {
		t.Errorf("抠出来的片段被截断了：\n  got  %s\n  want %s", blobs[0], value)
	}
}

// TestExtractJSONAfterIgnoresUnrelatedKeyPrefix 键名不能靠**子串**命中。
//
// 数据文件里还有 `loomy-auth-session-xxx` 这类相邻键（leveldb 的内键
// 前缀让同名键相邻）。本实现按 `strings.Index` 找子串，所以这里必须
// 明确：命中的键后面若没有 `{`，就跳过 —— 不能把下一个键的值算到它头上。
func TestExtractJSONAfterUnmatchedKeyIsSkipped(t *testing.T) {
	// 第一个键后面紧接着另一个键（没有值），第二个键才带值。
	text := keyAuthSession + "\x01\x01" + "loomy-other-key" + "\x01\x01" +
		`{"x":1}`
	blobs := extractJSONAfter(text, keyAuthSession, 8)
	if len(blobs) != 0 {
		t.Errorf("间隔超过上限时不该把下一个键的值算成它的值，得到 %v", blobs)
	}
}

// TestReadLocalAuthMissingDirIsError / 空目录不是错误 —— 这两种必须分开。
//
// "读不到"与"读失败"对用户是**两件不同的事**（前者要打开客户端登录，
// 后者要查路径/权限）。混成一个的话，提示只能退化成泛泛的一句。
func TestReadLocalAuthMissingDirIsError(t *testing.T) {
	_, _, err := ReadLocalAuth(filepath.Join(t.TempDir(), "不存在的目录"))
	if err == nil {
		t.Error("目录不存在时应当返回错误（用于区分「路径不对」与「还没登录」）")
	}
}

func TestReadLocalAuthEmptyDirIsNotError(t *testing.T) {
	a, source, err := ReadLocalAuth(t.TempDir())
	if err != nil {
		t.Fatalf("空目录不是错误：%v", err)
	}
	if a != nil {
		t.Errorf("空目录不该读出凭证，得到 %+v", a)
	}
	if source != "" {
		t.Errorf("没有任何数据文件时来源描述应当是空串，得到 %q（调用方据此区分"+
			"「目录空的」与「有文件但没读到」）", source)
	}
}

func TestReadLocalAuthEmptyDirArgIsNotError(t *testing.T) {
	// 空串直接返回，**不做 Glob** ——
	// `filepath.Glob(filepath.Join("", "loomy*.json"))` 会退化成相对 CWD 的模式，
	// 把当前目录下的任意文件当凭证读进来（credential.go 的同款守卫）。
	a, _, err := ReadLocalAuth("")
	if err != nil || a != nil {
		t.Errorf("空目录参数应当直接返回 (nil,nil)，得到 (%v, %v)", a, err)
	}
}

// ---------------------------------------------------------------------------
// 积分摘要
// ---------------------------------------------------------------------------

// TestReadLocalPointsFoldsPartialUpdates 部分更新不能把没写的字段抹成 0。
//
// # 这是本文件最重要的一条断言
//
// 实测的真实序列（本机 leveldb）：
//
//	{"balance":5500,"dailyBalance":4950,"updatedAt":"…09:38:46Z"}
//	{"balance":5500,                    "updatedAt":"…03:04:34Z"}  ← 只有 balance
//	{"balance":8000,"dailyBalance":5000,"updatedAt":"…08:15:28Z"}
//	{"balance":8000,                    "updatedAt":"…08:15:35Z"}  ← 只有 balance
//
// 若实现是"取 updatedAt 最新的一条直接解"，最后一条会把 DailyBalance
// 覆盖成 **0** —— 而 0 在展示语义上是「今日额度已用完」，一个**确定的、
// 且与事实相反**的结论（真实值是 5000 满额）。
//
// 期望：叠加式折叠 —— 没被写过的字段保持上一次的值。
func TestReadLocalPointsFoldsPartialUpdates(t *testing.T) {
	dir := writeStore(t,
		storeRecord(keyPointsSummary,
			`{"balance":5500,"dailyBalance":4950,"updatedAt":"2026-09-11T09:38:46.268Z"}`),
		// 更新、但**没有** dailyBalance（部分更新）
		storeRecord(keyPointsSummary,
			`{"balance":8000,"dailyBalance":5000,"updatedAt":"2026-09-14T08:15:28.627Z"}`),
		storeRecord(keyPointsSummary,
			`{"balance":8000,"updatedAt":"2026-09-14T08:49:20.671Z"}`),
	)
	pts, err := ReadLocalPoints(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pts == nil {
		t.Fatal("应当读到积分摘要")
	}
	if pts.Balance != 8000 {
		t.Errorf("balance = %d，want 8000", pts.Balance)
	}
	if pts.DailyBalance != 5000 {
		t.Errorf("★ dailyBalance = %d，want 5000 —— "+
			"最新的那条记录没写这个字段，被当成 0 覆盖了。"+
			"0 会被界面渲染成「今日额度已用完」，而事实是满额", pts.DailyBalance)
	}
	if pts.UpdatedAt != "2026-09-14T08:49:20.671Z" {
		t.Errorf("updatedAt = %q，应当取最新一条的时间", pts.UpdatedAt)
	}
}

// TestReadLocalPointsIgnoresForeignJSON 别的键的 JSON 不会被算成积分摘要。
func TestReadLocalPointsIgnoresForeignJSON(t *testing.T) {
	dir := writeStore(t,
		storeRecord(keyAuthSession, storeSessionJSON(fixtureSession, "U1", "150****3411",
			"2026-09-11T09:35:22.593Z")))
	pts, err := ReadLocalPoints(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pts != nil {
		t.Errorf("没有 loomy-points-summary 时应当读不到，得到 %+v", pts)
	}
}

// TestReadLocalPointsSkipsZeroFieldBlobs 一个数值字段都没有的片段不算摘要。
func TestReadLocalPointsSkipsZeroFieldBlobs(t *testing.T) {
	dir := writeStore(t,
		storeRecord(keyPointsSummary, `{"updatedAt":"2026-09-14T08:15:28.627Z"}`))
	pts, err := ReadLocalPoints(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pts != nil {
		t.Errorf("没有任何数值字段的片段不该被当成积分摘要：%+v", pts)
	}
}

// ---------------------------------------------------------------------------
// 自然日判断
// ---------------------------------------------------------------------------

// TestStoreDayChanged 跨日判断必须按 **CST**（服务端重置的时区）。
//
// # 为什么不能用本机时区
//
// 每日额度的重置是**服务端**行为，服务端在 CST。用本机时区判断会在
// 跨时区部署时得出"还没跨天"，而服务端那边已经重置了 ——
// 于是界面上展示的是**昨天**的每日额度。
func TestStoreDayChanged(t *testing.T) {
	// 2026-09-14 08:15 CST == 2026-09-14 00:15 UTC
	now := time.Date(2026, 9, 14, 8, 0, 0, 0, cstZone)

	cases := []struct {
		name string
		at   string
		want bool
	}{
		{"同一天（CST）", "2026-09-14T00:15:28.627Z", false},
		{"前一天（CST）", "2026-09-13T00:15:28.627Z", true},
		// 2026-09-14T16:30Z == 09-15 00:30 CST → 已经是"次日"
		{"UTC 看是同日、CST 看已跨日", "2026-09-14T16:30:00Z", true},
		{"解析不了 → 一律当跨日", "不是时间", true},
		{"空 → 当跨日", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := storeDayChanged(c.at, now); got != c.want {
				t.Errorf("storeDayChanged(%q) = %v，want %v", c.at, got, c.want)
			}
		})
	}
}

// TestJsonObjectEnd 配平扫描的边界。
func TestJsonObjectEnd(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{`{}`, 2},
		{`{"a":1}tail`, 7},
		{`{"a":{"b":2}}tail`, 13},
		{`{"a":"}"}tail`, 9},   // 字符串里的 } 不计深度
		{`{"a":"\\"}tail`, 10}, // 转义的反斜杠后引号仍结束字符串
		{`{"a":1`, -1},         // 截断
		{`x`, -1},              // 不是对象开头
	}
	for _, c := range cases {
		if got := jsonObjectEnd(c.s, 0); got != c.want {
			t.Errorf("jsonObjectEnd(%q) = %d，want %d", c.s, got, c.want)
		}
	}
}
