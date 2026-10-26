package apikey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/store"
)

// ── B2（task-59）SQL 后端的验收测试 ─────────────────────────────────────
//
// 组织方式：每个用例自己开一个临时库 + 临时 JSON 路径，结束时 store.Close()
// （Close 会把 store.DB 置回 nil，所以同包里既有的 JSON 用例不受影响）。
// 都是串行用例：store.DB 是包级单例，并行会互相踩。

// openSQL 开一个临时 SQL 库（DB 全局可用），返回 JSON 路径与清理函数。
func openSQL(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gw.db")
	if err := store.Open(dbPath); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		_ = store.Close()
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return filepath.Join(dir, "apikeys.json")
}

// mustCreate 建一把 key（失败立即 Fatal）。
func mustCreate(t *testing.T, s *Store, name string, limit int64, models []string) *Key {
	t.Helper()
	k, err := s.Create(name, limit, 0, models)
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	return k
}

// TestSQLValidateAcceptsBothBearerForms sk- 形态与裸串形态都要能通过 Validate，
// 并覆盖 hyxapi 那套 "-<channelId>" 后缀剥离（task-59 的头号陷阱）。
func TestSQLValidateAcceptsBothBearerForms(t *testing.T) {
	path := openSQL(t)
	s, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("NewWithStore: %v", err)
	}
	k := mustCreate(t, s, "两种形态", 0, nil)
	if !strings.HasPrefix(k.ID, "sk-") {
		t.Fatalf("新建 key 的 ID 应当仍是 sk- 形态（对外身份不变）: %q", k.ID)
	}
	// ① 原样（sk- 形态，也是 Bearer 值的正身）
	if got, err := s.Validate(k.ID); err != nil || got != k.ID {
		t.Fatalf("原样形态应当通过: (%q, %v)", got, err)
	}
	// ② 剥掉 sk- 后的形态（48 位裸串那一路的写法）
	if got, err := s.Validate(strings.TrimPrefix(k.ID, "sk-")); err != nil || got != k.ID {
		t.Fatalf("剥 sk- 形态应当通过: (%q, %v)", got, err)
	}
	// ③ 再带 -<channelId> 后缀（hyxapi middleware/auth.go:549-551 的口径）
	if got, err := s.Validate(k.ID + "-123"); err != nil || got != k.ID {
		t.Fatalf("带 -channelId 后缀应当通过: (%q, %v)", got, err)
	}
	// ④ 未知凭据
	if _, err := s.Validate("sk-ffffffffffffffff"); err != ErrUnknown {
		t.Fatalf("未知凭据应当 ErrUnknown，实际 %v", err)
	}
	// ⑤ 两种形态在**重启后**同样有效（索引是从库里重建的）
	s2, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("重启 NewWithStore: %v", err)
	}
	if _, err := s2.Validate(strings.TrimPrefix(k.ID, "sk-")); err != nil {
		t.Fatalf("重启后裸串形态应当通过: %v", err)
	}
}

// TestSQLValidateRejections 三种拒绝各一条：禁用 / 过期 / 耗尽。
func TestSQLValidateRejections(t *testing.T) {
	path := openSQL(t)
	s, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("NewWithStore: %v", err)
	}
	// ① 禁用
	disabled := mustCreate(t, s, "禁用", 0, nil)
	if _, err := s.Toggle(disabled.ID, false); err != nil {
		t.Fatalf("Toggle: %v", err)
	}
	if _, err := s.Validate(disabled.ID); err != ErrDisabled {
		t.Fatalf("禁用应当 ErrDisabled，实际 %v", err)
	}
	// ② 耗尽：额度 10，用掉 10
	exhausted := mustCreate(t, s, "耗尽", 10, nil)
	s.BumpUsage(exhausted.ID, 10, true)
	if _, err := s.Validate(exhausted.ID); err != ErrQuota {
		t.Fatalf("耗尽应当 ErrQuota，实际 %v", err)
	}
	// ③ 过期：直接改库里的 expired_time（apikey 没有公开的改期 API），
	//    再重启一次让索引从库里重建 —— 顺带证明"过期是库侧事实"。
	expiring := mustCreate(t, s, "过期", 0, nil)
	row, err := store.GetTokenByKey(expiring.ID)
	if err != nil || row == nil {
		t.Fatalf("GetTokenByKey: (%v, %v)", row, err)
	}
	row.ExpiredTime = 1 // 1970-01-01，必过期
	if err := store.SaveToken(row); err != nil {
		t.Fatalf("SaveToken(过期): %v", err)
	}
	s2, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("重启 NewWithStore: %v", err)
	}
	if _, err := s2.Validate(expiring.ID); err != ErrExpired {
		t.Fatalf("过期应当 ErrExpired，实际 %v", err)
	}
	// 未过期的那些不该被连坐
	if _, err := s2.Validate(exhausted.ID); err != ErrQuota {
		t.Fatalf("耗尽那把应当仍是 ErrQuota，实际 %v", err)
	}
}

// TestSQLBumpUsageSurvivesRestart 计量必须落库：重启后计数还在（task-59 要求 5）。
func TestSQLBumpUsageSurvivesRestart(t *testing.T) {
	path := openSQL(t)
	s, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("NewWithStore: %v", err)
	}
	k := mustCreate(t, s, "计量", 100, nil)
	s.BumpUsage(k.ID, 30, true)
	s.BumpUsage(k.ID, 12, true)
	s.BumpUsage(k.ID, 0, false)

	// 重启：新实例的索引完全来自库
	s2, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("重启 NewWithStore: %v", err)
	}
	views := s2.List()
	if len(views) != 1 {
		t.Fatalf("重启后应当只有 1 把 key，实际 %d", len(views))
	}
	v := views[0]
	if v.Requests != 3 || v.Used != 42 || v.Fails != 1 {
		t.Fatalf("计量没落库：requests=%d used=%d fails=%d（want 3/42/1）", v.Requests, v.Used, v.Fails)
	}
	// 余量也必须跟着走：Limit=100、Used=42 ⇒ remain=58
	if v.Remain != 58 {
		t.Fatalf("余量应当是 100-42=58，实际 %d", v.Remain)
	}
	// 库里的原始行也要对得上（不是只有内存对）
	row, err := store.GetTokenByKey(k.ID)
	if err != nil || row == nil {
		t.Fatalf("GetTokenByKey: (%v, %v)", row, err)
	}
	if row.Requests != 3 || row.UsedQuota != 42 || row.Fails != 1 || row.RemainQuota != 58 {
		t.Fatalf("库行的三列不对：requests=%d used=%d fails=%d remain=%d", row.Requests, row.UsedQuota, row.Fails, row.RemainQuota)
	}
}

// TestSQLBackfillFromJSONIsIdempotent 回填：JSON 有、tokens 空 ⇒ 导入一次；跑两次行数不变。
func TestSQLBackfillFromJSONIsIdempotent(t *testing.T) {
	// ① JSON 模式先造两把 key（此时 store.DB 必须是 nil：上一个用例的 Close 已置空）
	if store.DB != nil {
		t.Fatal("前置条件：本用例需要 store.DB == nil（JSON 模式起点）")
	}
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "apikeys.json")
	jsonStore, err := New(jsonPath)
	if err != nil {
		t.Fatalf("JSON 模式 New: %v", err)
	}
	mustCreate(t, jsonStore, "存量一", 100, nil)
	mustCreate(t, jsonStore, "存量二", 0, []string{"m-x"})

	// ② 开库 + 接管 ⇒ 一次性回填
	dbPath := filepath.Join(dir, "gw.db")
	if err := store.Open(dbPath); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	if err := jsonStore.AttachSQL(); err != nil {
		t.Fatalf("AttachSQL: %v", err)
	}
	first, err := store.CountTokens()
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if first != 2 {
		t.Fatalf("回填后应当 2 行，实际 %d", first)
	}

	// ③ 再跑一次（同一个 JSON、同一个库）：行数不变
	again, err := NewWithStore(filepath.Join(dir, "apikeys.json"))
	if err != nil {
		t.Fatalf("第二次装配: %v", err)
	}
	second, err := store.CountTokens()
	if err != nil {
		t.Fatalf("CountTokens(第二次): %v", err)
	}
	if second != first {
		t.Fatalf("回填必须幂等：第一次 %d 行、第二次 %d 行", first, second)
	}
	if got := len(again.List()); got != 2 {
		t.Fatalf("第二次装配后内存索引应当 2 把，实际 %d", got)
	}
	// ④ 存量 key 的属性也要搬对（白名单那把）
	for _, v := range again.List() {
		if v.Name == "存量二" && (len(v.Models) != 1 || v.Models[0] != "m-x") {
			t.Fatalf("回填丢了白名单: %+v", v.Models)
		}
	}
}

// TestSQLModelMapRoundTrip ModelMap 往返无损，含 nil 与空 map 的区分（验收⑤）。
func TestSQLModelMapRoundTrip(t *testing.T) {
	// ① 映射层三形态
	if got, err := encodeModelMap(nil); err != nil || got != "" {
		t.Fatalf("nil 应当编码成空串，实际 (%q, %v)", got, err)
	}
	empty, err := encodeModelMap(map[string]string{})
	if err != nil || empty != "{}" {
		t.Fatalf("空 map 应当编码成 {}，实际 (%q, %v)", empty, err)
	}
	nonEmpty, err := encodeModelMap(map[string]string{"pub": "internal"})
	if err != nil || nonEmpty != "{\"pub\":\"internal\"}" {
		t.Fatalf("非空 map 编码错: (%q, %v)", nonEmpty, err)
	}
	if decodeModelMap("") != nil {
		t.Fatal("空串应当解回 nil")
	}
	decodedEmpty := decodeModelMap("{}")
	if decodedEmpty == nil || len(decodedEmpty) != 0 {
		t.Fatalf("{} 应当解回非 nil 的空 map，实际 %#v", decodedEmpty)
	}
	decoded := decodeModelMap(nonEmpty)
	if decoded["pub"] != "internal" || len(decoded) != 1 {
		t.Fatalf("非空 map 解错: %#v", decoded)
	}

	// ② 走库往返：配了映射的 key 重启后映射还在
	path := openSQL(t)
	s, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("NewWithStore: %v", err)
	}
	k, err := s.Create("映射", 0, 0, nil, map[string]string{"deepseek-v4-pro": "csg/deepseek-v4.1-flash-1017"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s2, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("重启: %v", err)
	}
	got, ok := s2.ModelMapOf(k.ID)
	if !ok {
		t.Fatal("重启后应当还能读到这把 key")
	}
	if len(got) != 1 || got["deepseek-v4-pro"] != "csg/deepseek-v4.1-flash-1017" {
		t.Fatalf("映射往返丢了: %#v", got)
	}
	// ③ 库里那一列也必须是 JSON（不是被别的字段挤掉）
	row, err := store.GetTokenByKey(k.ID)
	if err != nil || row == nil {
		t.Fatalf("GetTokenByKey: (%v, %v)", row, err)
	}
	if !strings.Contains(row.ModelMap, "deepseek-v4-pro") {
		t.Fatalf("model_map 列内容不对: %q", row.ModelMap)
	}
}

// TestSQLOwnerIDRoundTrip OwnerID 是字符串，往返与归属过滤都要对（验收⑥）。
func TestSQLOwnerIDRoundTrip(t *testing.T) {
	path := openSQL(t)
	s, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("NewWithStore: %v", err)
	}
	owned, err := s.CreateOwned("u-abc123", "成员 key", 0, 0, nil)
	if err != nil {
		t.Fatalf("CreateOwned: %v", err)
	}
	admin := mustCreate(t, s, "管理员 key", 0, nil)

	s2, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("重启: %v", err)
	}
	if owner, ok := s2.OwnerOf(owned.ID); !ok || owner != "u-abc123" {
		t.Fatalf("OwnerID 往返丢了: (%q, %v)", owner, ok)
	}
	if owner, ok := s2.OwnerOf(admin.ID); !ok || owner != "" {
		t.Fatalf("管理员 key 的 owner 应当是空串（= 管理员所有）: (%q, %v)", owner, ok)
	}
	// 成员视角只看得到自己的那一把；管理员视角看得到全部
	if got := len(s2.ListFor("u-abc123", false)); got != 1 {
		t.Fatalf("成员应当只看到 1 把，实际 %d", got)
	}
	if got := len(s2.ListFor("", true)); got != 2 {
		t.Fatalf("管理员应当看到 2 把，实际 %d", got)
	}
}

// TestSQLExplicitWiringAndLazyAttach 两条装配路线都要能工作（验收⑦）。
func TestSQLExplicitWiringAndLazyAttach(t *testing.T) {
	// ① 显式装配：库已开 ⇒ NewWithStore 直接是 SQL 模式，且**不落** JSON 文件
	path := openSQL(t)
	s, err := NewWithStore(path)
	if err != nil {
		t.Fatalf("NewWithStore: %v", err)
	}
	k := mustCreate(t, s, "显式装配", 0, nil)
	row, err := store.GetTokenByKey(k.ID)
	if err != nil || row == nil {
		t.Fatalf("显式装配下应当立刻有库行: (%v, %v)", row, err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("SQL 模式不该生成 %s（stat err=%v）", path, statErr)
	}
	// ② 显式装配要求库已开：库没开时必须报错，而不是静默退化成 JSON
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := NewWithStore(path); err == nil {
		t.Fatal("库未打开时 NewWithStore 应当报错")
	}
	// ③ 懒接管：先 JSON 起手（库关闭），开库后第一次调用自动接管并回填
	lazyPath := filepath.Join(t.TempDir(), "apikeys.json")
	lazy, err := New(lazyPath)
	if err != nil {
		t.Fatalf("JSON 模式 New: %v", err)
	}
	lazyKey := mustCreate(t, lazy, "懒接管", 30, nil)
	if _, err := store.GetTokenByKey(lazyKey.ID); err == nil {
		t.Fatal("接管前不该有库行")
	}
	if err := store.Open(filepath.Join(t.TempDir(), "lazy.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	// 一次普通读调用触发接管（读方法也挂了守卫，不必等写路径）
	if _, ok := lazy.ModelsOf(lazyKey.ID); !ok {
		t.Fatal("懒接管后应当能读到这把 key")
	}
	if _, err := lazy.Validate(lazyKey.ID); err != nil {
		t.Fatalf("懒接管后 Validate 应当通过: %v", err)
	}
	row2, err := store.GetTokenByKey(lazyKey.ID)
	if err != nil || row2 == nil {
		t.Fatalf("懒接管应当把 JSON 快照回填进库: (%v, %v)", row2, err)
	}
	// 接管后新的写入直接进库
	newKey := mustCreate(t, lazy, "接管后新建", 0, nil)
	if row3, err := store.GetTokenByKey(newKey.ID); err != nil || row3 == nil {
		t.Fatalf("接管后的新建应当直接落库: (%v, %v)", row3, err)
	}
	// 重启一次，两把都在
	after, err := NewWithStore(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatalf("重启: %v", err)
	}
	if got := len(after.List()); got != 2 {
		t.Fatalf("重启后应当 2 把，实际 %d", got)
	}
}

// TestSQLDeletedKeysAreNotResurrectedByRestart 「全删光后重启」不能把 JSON 里的旧 key 复活。
//
// 这是"tokens 为空就导入"这条门唯一能出错的地方：管理员把 key 全删了 ⇒ 表又空了，
// 而 data/apikeys.json 还在 ⇒ 下一次启动就会把删掉的 key 原样搬回来。
// 导入标记（<path>.imported）就是为这一条加的。
func TestSQLDeletedKeysAreNotResurrectedByRestart(t *testing.T) {
	if store.DB != nil {
		t.Fatal("前置条件：本用例需要 store.DB == nil（JSON 模式起点）")
	}
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "apikeys.json")
	jsonStore, err := New(jsonPath)
	if err != nil {
		t.Fatalf("JSON 模式 New: %v", err)
	}
	k1 := mustCreate(t, jsonStore, "会被删掉一", 0, nil)
	k2 := mustCreate(t, jsonStore, "会被删掉二", 0, nil)

	if err := store.Open(filepath.Join(dir, "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	if err := jsonStore.AttachSQL(); err != nil {
		t.Fatalf("AttachSQL: %v", err)
	}
	if n, err := store.CountTokens(); err != nil || n != 2 {
		t.Fatalf("接管后应当 2 行，实际 (%d, %v)", n, err)
	}
	// ① 全删（管理台清空）
	if err := jsonStore.Delete(k1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := jsonStore.Delete(k2.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n, err := store.CountTokens(); err != nil || n != 0 {
		t.Fatalf("删光后库里不该有行，实际 (%d, %v)", n, err)
	}
	// ② 导入标记必须已经落下（它是防止复活的那道闸）——**在库里**，不是文件
	marked, err := store.OptionExists("apikey.json_imported")
	if err != nil {
		t.Fatalf("OptionExists: %v", err)
	}
	if !marked {
		t.Fatal("导入标记应当已写进 options 表（否则重启会复活旧 key）")
	}
	// 并且**不许**留下文件产物（主脑裁决：标记进库，不进 data/，避免被误 add）
	if _, statErr := os.Stat(jsonPath + ".imported"); !os.IsNotExist(statErr) {
		t.Fatalf("不该有文件标记（stat err=%v）", statErr)
	}
	// ③ 重启：JSON 里那两把**不许**回来
	again, err := NewWithStore(jsonPath)
	if err != nil {
		t.Fatalf("重启: %v", err)
	}
	if got := again.List(); len(got) != 0 {
		t.Fatalf("删光的 key 被 JSON 复活了 %d 把：%+v", len(got), got)
	}
	if n, err := store.CountTokens(); err != nil || n != 0 {
		t.Fatalf("重启后库里仍应为 0 行，实际 (%d, %v)", n, err)
	}
}
