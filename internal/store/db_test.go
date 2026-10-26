// db_test.go 数据层地基的门禁：建表、幂等、无 cgo、模型清单一致、PRAGMA 真的生效。
//
// # 为什么这几条是「门禁」而不是「覆盖率」
//
// 它们各自守着一类**静默失败**：
//
//  1. 少建一张表      → 编译过、测试过，几天后某个后台任务才报 no such table
//  2. Open 不幂等     → 换库时静默沿用旧库（数据写到了以为是新库的地方）
//  3. 引入 cgo 驱动   → 本机 CI 全绿，发布时才发现在目标平台编不出来
//  4. allModels 漏登记 → 同上第 1 条，但成因是「加了 struct 忘了加一行」
//  5. PRAGMA 没生效   → busy_timeout=0 表现为随机的 database is locked，
//     日志里看不出规律（这是最难查的一类）
//
// 每条都写成「坏掉就一定红」的形式，而不是「跑一遍看有没有 panic」。
package store

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// openTemp 在临时目录开一个新的库并迁移，测试结束自动关闭。
//
// ⚠ 不用 t.Parallel()：DB 是包级单例（见 db.go 的取舍），并行会让用例互相
// 关掉对方的句柄。这条约束是全局的，所以只在这里说明一次。
func openTemp(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gw.db")
	if err := Open(path); err != nil {
		t.Fatalf("Open(%q) 失败: %v", path, err)
	}
	t.Cleanup(func() { _ = Close() })
	if err := Migrate(); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	return path
}

// TestMigrateCreatesExpectedTables 门禁 1：迁移后**恰好**是契约里的那张表集合。
//
// # 为什么用 sqlite_master 而不是相信 Migrate 的返回值
//
// Migrate 返回 nil 只说明 GORM 没报错。表到底建出来了没有，要问数据库本身。
// 这也是验收口径「SQLite 能列出契约里的全部表名」的实现方式。
//
// ⚠ 断言是**集合相等**，不是「至少包含」：多出来的表说明有人偷偷建了表而没
// 登记进 Tables()（那就绕过了「库里有哪些表」这份唯一清单）。
//
// 名字里**刻意不带数字**（原 TestMigrateCreates12Tables）：表集合会长大
// （A1/A2 起 13 张，见下面的 wantTableCount），把数字写进函数名会让每份报告
// 都带一个过期的契约值。
func TestMigrateCreatesExpectedTables(t *testing.T) {
	openTemp(t)

	got := sqliteTableNames(t)
	want := append([]string{}, Tables()...)
	sort.Strings(got)
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("库里的表与 Tables() 不一致\n got: %v\nwant: %v\n"+
			"（多出来的表 ⇒ 有人建表却没登记；少了的表 ⇒ 迁移漏了一张）", got, want)
	}
	// 表数的**显式契约**（不是从 Tables() 推出来的循环断言）：改表集合必须改这里。
	// Wave A1/A2 起 = 13（A2 的 tokens 表）。
	const wantTableCount = 13
	if len(got) != wantTableCount {
		t.Fatalf("表数 = %d，契约要求 %d（新增表要同时改 allModels()、Tables() 与这里）", len(got), wantTableCount)
	}
}

// sqliteTableNames 返回库里的用户表名（排除 sqlite 内部表）。
func sqliteTableNames(t *testing.T) []string {
	t.Helper()
	var names []string
	err := DB.Raw(
		"SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
	).Scan(&names).Error
	if err != nil {
		t.Fatalf("查询 sqlite_master 失败: %v", err)
	}
	return names
}

// TestOpenIsIdempotent 门禁 2：同一路径 Open 两次不报错，且数据仍在。
//
// # 为什么必须验证「数据仍在」，而不只是「没报错」
//
// 「先关旧的再开新的」如果实现错（比如把关闭写成了删除，或者误用了内存 DSN），
// 第二次 Open 一样不会报错 —— 但它把数据弄丢了。只断言 err == nil 会漏掉
// 这一类最严重的回归。
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gw.db")

	if err := Open(path); err != nil {
		t.Fatalf("第一次 Open 失败: %v", err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("第一次 Migrate 失败: %v", err)
	}
	if err := DB.Create(&Option{Key: "seed_key", Value: "seed_value"}).Error; err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 第二次 Open 同一路径：必须成功（验收口径第 5 条）。
	if err := Open(path); err != nil {
		t.Fatalf("第二次 Open 同一路径失败: %v（Open 必须幂等）", err)
	}
	// 幂等也包括「重复迁移」：AutoMigrate 在已有表上再跑一遍不该报错。
	if err := Migrate(); err != nil {
		t.Fatalf("第二次 Migrate 失败: %v（AutoMigrate 必须可重复执行）", err)
	}

	var got Option
	if err := DB.First(&got, "key = ?", "seed_key").Error; err != nil {
		t.Fatalf("重开后读不到之前写的行: %v（说明第二次 Open 弄丢了数据或换成了别的库）", err)
	}
	if got.Value != "seed_value" {
		t.Fatalf("重开后值 = %q，want seed_value", got.Value)
	}
	_ = Close()
}

// TestNoCgoDriver 门禁 3：go.mod 里**没有**需要 cgo 的 SQLite 驱动。
//
// # 为什么扫 go.mod 而不是看编译结果
//
// 本机有 C 工具链，cgo 驱动能编过 —— 所以「本地能 build」证明不了任何事。
// 约束的载体必须是依赖清单本身（PLAN §2 D1 铁律 + 验收第 1 条的 CGO_ENABLED=0）。
//
// 判据：gorm.io/driver/sqlite 与 mattn/go-sqlite3 都是 cgo 实现，出现即失败。
func TestNoCgoDriver(t *testing.T) {
	root := moduleRootForStore(t)
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("读 go.mod 失败: %v", err)
	}
	mods := string(raw)

	for _, banned := range []string{"gorm.io/driver/sqlite", "mattn/go-sqlite3"} {
		if strings.Contains(mods, banned) {
			t.Errorf("go.mod 含 %s —— 它需要 cgo，会拆掉「单二进制零依赖」这条部署优势"+
				"（PLAN §2 D1）。纯 Go 驱动请用 github.com/glebarez/sqlite", banned)
		}
	}
	if !strings.Contains(mods, "github.com/glebarez/sqlite") {
		t.Error("go.mod 里没有 github.com/glebarez/sqlite —— 纯 Go SQLite 驱动是本次改造的前提")
	}
}

// moduleRootForStore 从包目录向上找 go.mod。
//
// 用「向上找」而不是硬编码相对层级：测试可能被 -C 或 IDE 以不同工作目录启动，
// 硬编码 ../.. 会随目录调整而失效（且失效方式是 Fatal，看起来像别的问题）。
func moduleRootForStore(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("向上 6 层都没找到 go.mod —— 无法校验依赖约束")
	return ""
}

// TestAllModelsAreMigrated 门禁 4：models.go 里每个 struct 都登记进了 allModels()。
//
// # 为什么用源码解析而不是反射枚举
//
// Go 的反射**不能枚举一个包的类型的类型**（只能看已知值的类型）。所以
// 「models.go 里有几个 struct」这件事只能从源码里读。
//
// 这条测试的价值在于：它把「加了 struct 忘了加一行」从一次几天后才暴露的
// no such table，变成一次立刻的测试失败。而那种遗漏**没有任何编译错误**。
func TestAllModelsAreMigrated(t *testing.T) {
	declared := structNamesInFile(t, "models.go")
	if len(declared) == 0 {
		t.Fatal("models.go 里一个 type 都没解析到 —— 测试自身失效（路径或解析方式错了）")
	}

	// valueTypes 是**不是表**的导出类型：它们只作为列的值类型存在（Wave A1 起
	// ChannelInfo 定义在本文件 —— 主脑冻结的 Channel 契约要求类型随结构体走）。
	//
	// 判据用**有没有 TableName() 方法**，而不是维护一份"我保证它不是表"的口头名单：
	// 一个类型一旦写了 TableName，它就是想被建表 —— 那时必须进 allModels()。
	// 这条反向断言让白名单不会变成"逃避登记的后门"。
	valueTypes := map[string]any{"ChannelInfo": ChannelInfo{}}
	for name, v := range valueTypes {
		if _, hasTableName := reflect.TypeOf(v).MethodByName("TableName"); hasTableName {
			t.Errorf("%s 有 TableName() ⇒ 它其实是一张表，必须登记进 allModels()", name)
		}
	}

	migrated := map[string]bool{}
	for _, m := range allModels() {
		typ := reflect.TypeOf(m)
		if typ.Kind() != reflect.Pointer {
			t.Errorf("allModels() 里 %v 不是指针 —— GORM 需要指针才能回填主键/时间", typ)
			continue
		}
		migrated[typ.Elem().Name()] = true
	}

	for _, name := range declared {
		if _, isValueType := valueTypes[name]; isValueType {
			continue
		}
		if !migrated[name] {
			t.Errorf("models.go 定义了 %s，但它不在 allModels() 里 —— "+
				"这张表不会被建出来，而错误要到第一次用到它时才出现", name)
		}
	}
	// 反向：登记了但**整个包**里都没有这个 struct（改名/删除后的残留）。
	//
	// ⚠ 反向检查扫的是整个包、而不是只有 models.go：Wave A1 起表定义可以落在别的
	// 文件（A2 的 token.go 定义 Token），但"登记了一个不存在的类型"永远是错的。
	// 正向检查仍**只**看 models.go —— 那里是文件的定位（"库里有哪些表"的唯一清单），
	// 而其它文件里的导出 struct 大量是值类型（ChannelQuery/ChannelPage/…），
	// 不该被要求进 allModels()。
	declaredInPkg := map[string]bool{}
	files, globErr := filepath.Glob("*.go")
	if globErr != nil {
		t.Fatalf("枚举包内 .go 文件失败: %v", globErr)
	}
	for _, fn := range files {
		if strings.HasSuffix(fn, "_test.go") {
			continue
		}
		for _, n := range structNamesInFile(t, fn) {
			declaredInPkg[n] = true
		}
	}
	for name := range migrated {
		if !declaredInPkg[name] {
			t.Errorf("allModels() 登记了 %s，但本包 .go 文件里没有这个 struct —— 残留登记会让人误解表清单", name)
		}
	}
}

// structNamesInFile 返回文件里所有**导出的 struct** 类型名。
//
// 只取导出的：本包的模型都是导出类型（其他切片要引用），未导出的辅助类型
// 不该被要求进 allModels()。
func structNamesInFile(t *testing.T, filename string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", filename, err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if _, ok := ts.Type.(*ast.StructType); !ok {
				continue
			}
			if ts.Name.IsExported() {
				out = append(out, ts.Name.Name)
			}
		}
	}
	return out
}

// TestPragmasAreEffective 门禁 5：PRAGMA 真的生效（不只是 DSN 拼对了）。
//
// # 为什么必须回查数据库
//
// buildDSN 拼得再漂亮，若驱动没认这个参数，运行期仍是 busy_timeout=0。
// 而那个故障的表现是「并发时偶发 database is locked」—— 现象与原因隔得很远。
// 所以这里直接问数据库：journal_mode 是什么、busy_timeout 是多少。
func TestPragmasAreEffective(t *testing.T) {
	openTemp(t)

	var journalMode string
	if err := DB.Raw("PRAGMA journal_mode").Scan(&journalMode).Error; err != nil {
		t.Fatalf("查 journal_mode 失败: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q，want wal —— 没有 WAL，写日志会阻塞读指标"+
			"（而那正是本改造要同时做的两件事）", journalMode)
	}

	var busyTimeout int
	if err := DB.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error; err != nil {
		t.Fatalf("查 busy_timeout 失败: %v", err)
	}
	if busyTimeout != 30000 {
		t.Errorf("busy_timeout = %d，want 30000 —— 0 会表现为随机 database is locked", busyTimeout)
	}
}

// TestBuildDSNRespectsCaller 调用方显式给的参数不被覆盖。
//
// # 为什么要单独测
//
// 「补默认值」的实现很容易写成「设默认值」（后写覆盖前写）。后者在正常启动时
// 看不出任何区别，只在运维真的需要关掉 WAL（网络盘）时才炸 —— 而那时
// 表现是「配置不生效」，没人会想到是网关把参数覆盖了。
//
// # 断言为什么绑「解码后的语义」而不是字面量子串（2026-10-26 修）
//
// buildDSN 末尾是 url.Values.Encode()，它会把 pragma 里的括号转义成 %28/%29，
// 于是 DSN 里出现的是 `_pragma=busy_timeout%2830000%29`。此前这里直接断言字面量
// `busy_timeout(30000)`，把**正确实现**判成了红。
//
// 哪边才是规格？驱动源码给了唯一答案 —— glebarez/go-sqlite@v1.21.2/sqlite.go:873-891：
//
//	q, err := url.ParseQuery(query)      // DSN 的查询串**先解码**
//	...
//	for _, v := range q["_pragma"] {     // 再逐条当 PRAGMA 执行
//	        cmd := "pragma " + v
//
// 即：URL 编码是**传输形式**，解码后的文本才是 pragma 语句。所以规格是
// 「解码后必须是 busy_timeout(30000)」，不是「字面量必须原样出现在 DSN 里」。
//
// 旁证两条：
//
//	· end-to-end：TestPragmasAreEffective 回查数据库，当前 DSN 的 WAL 与
//	  busy_timeout=30000 都**真的生效**（若编码不能被解码，它不可能绿）；
//	· 驱动自己的测试也用编码形式：go-sqlite/all_test.go:1209
//	  `?_pragma=busy_timeout%3d50000`。
//
// 结论：改**断言**，不改 buildDSN —— 编码不是缺陷，是 url.Values 的标准输出。
func TestBuildDSNRespectsCaller(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // 解码后必须出现
		deny []string // 解码后不许出现
	}{
		{
			name: "空查询串补全默认值",
			in:   "/tmp/gw.db",
			want: []string{"busy_timeout(30000)", "journal_mode(WAL)", "_txlock=immediate"},
		},
		{
			name: "调用方指定 journal_mode 时不追加",
			in:   "/tmp/gw.db?_pragma=journal_mode(DELETE)",
			want: []string{"journal_mode(DELETE)", "busy_timeout(30000)", "_txlock=immediate"},
			deny: []string{"journal_mode(WAL)"},
		},
		{
			name: "等号写法也认（不重复追加）",
			in:   "/tmp/gw.db?_pragma=journal_mode%3DDELETE",
			want: []string{"journal_mode=DELETE", "_txlock=immediate"},
			deny: []string{"journal_mode(WAL)"},
		},
		{
			name: "调用方指定 txlock 时不覆盖",
			in:   "/tmp/gw.db?_txlock=deferred",
			want: []string{"_txlock=deferred"},
			deny: []string{"_txlock=immediate"},
		},
		{
			name: "内存库同样带上参数",
			in:   ":memory:",
			want: []string{"busy_timeout(30000)", "_txlock=immediate"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildDSN(c.in)
			// 断言绑语义：把 DSN 还原成驱动**实际执行**的文本（见 decodedDSN）。
			decoded := decodedDSN(got)
			for _, w := range c.want {
				if !strings.Contains(decoded, w) {
					t.Errorf("buildDSN(%q) = %q\n解码后 = %q\n缺少 %q", c.in, got, decoded, w)
				}
			}
			for _, d := range c.deny {
				if strings.Contains(decoded, d) {
					t.Errorf("buildDSN(%q) = %q\n解码后 = %q\n不该出现 %q（调用方显式给的参数被覆盖了）",
						c.in, got, decoded, d)
				}
			}
		})
	}
}

// decodedDSN 把 buildDSN 的输出还原成**驱动实际执行**的文本：
//
//	每个 `_pragma` 的值单独一行（驱动执行的就是 `pragma <值>`）；
//	其余查询参数列成 k=v。
//
// 这样断言就绑在语义上，而不是绑在 URL 编码的字面形式上 —— 后者是
// url.Values.Encode 的实现细节（见 TestBuildDSNRespectsCaller 的注释）。
func decodedDSN(dsn string) string {
	_, rawQuery, _ := strings.Cut(dsn, "?")
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return dsn // 解析不了就把原文给出去，让断言以最直白的方式失败
	}
	var b strings.Builder
	for _, p := range q["_pragma"] {
		b.WriteString(p)
		b.WriteString("\n")
	}
	for k, vs := range q {
		if k == "_pragma" {
			continue
		}
		for _, v := range vs {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	return b.String()
}

// TestOpenCreatesParentDir 父目录不存在时自动创建。
//
// # 为什么这是部署期最容易踩的一脚
//
// 首次部署时 data/ 往往不存在，而 SQLite 的报错（unable to open database file）
// 不含路径，现场只能猜。这条把它变成一个显式的、可读的错误或一次自动修复。
func TestOpenCreatesParentDir(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "deep", "nested", "gw.db")
	if err := Open(nested); err != nil {
		t.Fatalf("Open 嵌套路径失败: %v（应自动创建父目录）", err)
	}
	defer func() { _ = Close() }()

	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("数据库文件没被创建: %v", err)
	}
}

// TestIsMemoryDSN 内存库判定 —— 认错的代价是创建名为 :memory: 的目录。
func TestIsMemoryDSN(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{":memory:", true},
		{"file::memory:", true},
		{"file::memory:?cache=shared", true},
		{"file:abc?mode=memory&cache=shared", true},
		{"file:abc?cache=shared", false},
		{filepath.Join("data", "gw.db"), false},
		{"gw.db", false},
		{"file:data/gw.db", false},
	}
	for _, c := range cases {
		if got := isMemoryDSN(c.path); got != c.want {
			t.Errorf("isMemoryDSN(%q) = %v，want %v", c.path, got, c.want)
		}
	}
}

// TestMigrateWithoutOpen 未 Open 时 Migrate 返回可判定的错误，而不是 panic。
//
// 本仓约定：服务不能因为一条路径（忘了接线）就整个崩掉 —— 报 500 并说明
// 原因，比进程退出好（后者会掐断已建立的会话和在跑的请求）。
func TestMigrateWithoutOpen(t *testing.T) {
	_ = Close() // 确保干净状态
	err := Migrate()
	if err == nil {
		t.Fatal("未 Open 时 Migrate 应返回错误")
	}
	if !errors.Is(err, ErrNotOpen) {
		t.Fatalf("错误 = %v，want errors.Is(err, ErrNotOpen) —— "+
			"上层要能用 errors.Is 判断「数据库没初始化」并给出可操作的提示", err)
	}
}
