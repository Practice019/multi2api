// multiprovider_test.go 锁定 S2（同 uid 冲突的判据）与 S3（日志报去重后的数）。
//
// # 这两个问题是从哪来的
//
// 用户实测：授权成功后日志说
//
//	codearts: 已并入账号池 2 个账号
//
// 但 GET /admin/accounts 只显示 1 个。侦察结论是 auths/codearts/ 下两份凭证
// 解析出**同一个 uid**（ParseCredential 优先取 refresh_token JWT 里的 account_id，
// 两次浏览器授权是同一个华为云账号）—— 那是**正确行为，不是 bug**。
//
// 但它暴露了两个真问题：
//
//	S2 同 uid 冲突没有判据：旧实现里 auths 追加 2 条、secrets 只留 1 份，
//	   且"哪份胜出"完全由 filepath.Glob 的字母序决定。两份都健康时无害，
//	   但一份已过期、一份有效时，胜出的可能是**过期那份** ——
//	   界面完全正常（号在池里、昵称也对），只有请求全挂。
//	S3 日志报的是"我传了几条"（含重复），与池子去重后的实际数量不一致。
//
// # 测试数据为什么用扁平形
//
// 扁平形（{"accessKeyId":...,"uid":"SAME-UID"}）里 uid 是**显式写死**的，
// 比嵌套形更好控制：嵌套形的 uid 会先被 account.uid 影响，实测确认两种形态
// 都能解析出期望的 uid（见下方 mustParseUID 的前置自检）。
//
// 复现"同一 uid 两份文件"最省事的做法就是**两份的 uid 字段显式相同** ——
// 这正是线上发生的事（同一个华为云账号）。
package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/pool"
)

// credJSON 造一份扁平形凭证 JSON。
//
// refreshToken 为空串时 json tag 是 omitempty 之外的普通字段，
// 这里显式写出以保证两个变体的**字节结构**一致，只有被测字段不同。
func credJSON(uid, ak, nickname string, expiresAt int64, refreshToken string, withDPoP bool) string {
	dpop := ""
	if withDPoP {
		dpop = `,"dpopPrivateKeyJwk":{"kty":"EC","crv":"P-256","x":"a","y":"b","d":"c"}`
	}
	return `{"accessKeyId":"` + ak + `",` +
		`"secretAccessKey":"` + ak + `_SECRET",` +
		`"securityToken":"` + ak + `_ST",` +
		`"expiresAt":` + itoa(expiresAt) + `,` +
		`"refresh_token":"` + refreshToken + `",` +
		`"clientId":"vscode-codebot",` +
		`"uid":"` + uid + `",` +
		`"nickname":"` + nickname + `"` + dpop + `}`
}

// itoa 避免为一处格式化引入 strconv 的读者负担（值域只有时间戳）。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// writeCred 落盘一份凭证并返回其路径。
func writeCred(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("写凭证 %s: %v", name, err)
	}
	return p
}

// mustParseUID 是**前置自检**：确认测试数据真的解析出期望的 uid。
//
// 为什么必须有：如果 uid 没按预期落下来（例如 json 字段名写错，
// 或将来 ParseCredential 的回落链变了），测试就会因为"uid 本来就不同"
// 而**假通过** —— 那验不出冲突判据，只证明了两份文件恰好不冲突。
func mustParseUID(t *testing.T, path, wantUID string) *codearts.Auth {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("前置自检读 %s: %v", path, err)
	}
	a, err := codearts.ParseCredential(raw)
	if err != nil {
		t.Fatalf("前置自检失败：%s 无法解析（%v）—— 本测试会假通过", path, err)
	}
	if a.UID != wantUID {
		t.Fatalf("前置自检失败：%s 解析出 uid=%q，期望 %q —— "+
			"冲突场景没有真正构造出来，本测试会假通过", path, a.UID, wantUID)
	}
	return a
}

// loadParsed 走真实路径（LoadDir + ParseCredential）拿到凭证列表，
// 并给每条打上前置自检。
func loadParsed(t *testing.T, dir string) []*codearts.Auth {
	t.Helper()
	list, err := codearts.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	return list
}

// ---------------------------------------------------------------------------
// S2：冲突判据
// ---------------------------------------------------------------------------

// TestDedupeKeepsLaterExpiry 锁定判据 1：expiresAt 更大的胜出 —— **与文件名顺序相反**。
//
// 这是整个 S2 的核心场景，也是线上最难查的那类故障的复现：
//
//	codearts-aaa.json  → 已过期（expiresAt 小） 但字母序在前
//	codearts-zzz.json  → 有效（expiresAt 大）   字母序在后
//
// 旧实现按 Glob 字母序「后写的胜出」，恰好选中 zzz（对的，巧合）；
// 把文件名对调（过期那份叫 zzz）旧实现就选错 —— 那条见下一条测试。
//
// 这里更直接地钉住"按内容选，不按文件名选"：让**字母序在前的那份**更晚过期。
// 若有人把选优逻辑去掉（还原成"最后出现者胜出"/“先出现者胜出”），
// 选中的就会是过期那份，断言立刻变红。
func TestDedupeKeepsLaterExpiry(t *testing.T) {
	const uid = "01a08fe00b8b7c21bd95e11109692b80"
	dir := t.TempDir()

	// 字母序在前 aaa：**更晚过期**（应胜出）
	fresh := writeCred(t, dir, "codearts-aaa.json",
		credJSON(uid, "AK_FRESH", "fresh", 1999999999, "RT_FRESH", true))
	// 字母序在后 zzz：**已过期**（应被丢弃）
	stale := writeCred(t, dir, "codearts-zzz.json",
		credJSON(uid, "AK_STALE", "stale", 1000000000, "RT_STALE", true))

	// 前置自检：两份必须真的解析出**同一个 uid**，否则本测试假通过
	mustParseUID(t, fresh, uid)
	mustParseUID(t, stale, uid)

	list := loadParsed(t, dir)
	if len(list) != 2 {
		t.Fatalf("LoadDir 读到 %d 个凭证，期望 2（前置条件：目录里确实有两份冲突凭证）", len(list))
	}

	auths, secrets := dedupeCodeartsByUID(list)

	// 断言 A：按 uid 去重后只剩 1 条 —— auths 不再有重复 uid
	if len(auths) != 1 {
		t.Fatalf("去重后 auths 有 %d 条，期望 1（同 uid 必须聚合）", len(auths))
	}
	// 断言 B：选中的是**更晚过期**的那份（AK_FRESH），不是字母序在后的 zzz
	got, ok := secrets[uid].(*codearts.Auth)
	if !ok || got == nil {
		t.Fatalf("secrets[%s] 不是 *codearts.Auth（实际 %T）", uid, secrets[uid])
	}
	if got.AccessKey != "AK_FRESH" {
		t.Errorf("胜出的是 AK=%q（文件 %s），期望 AK_FRESH（更新那份）—— "+
			"判据退化成了文件名字母序，一份过期凭证会静默顶掉有效凭证",
			got.AccessKey, filepath.Base(got.FilePath))
	}
	if got.ExpiresAt != 1999999999 {
		t.Errorf("胜出凭证 expiresAt=%d，期望 1999999999（更晚过期）", got.ExpiresAt)
	}
}

// TestDedupeKeepsLaterExpiryRegardlessOfFilenameOrder 是上一条的镜像补强：
//
// 把「更晚过期」的那份放到**字母序在后**（zzz），过期那份放 aaa。
// 两条测试一起构成完整的对照：
//
//	新鲜在 aaa / 新鲜在 zzz  → **两种文件顺序下都必须选中新鲜那份**
//
// 只写一条的话，一个"永远选字母序最后一个"的错误实现也可能蒙对一半。
func TestDedupeKeepsLaterExpiryRegardlessOfFilenameOrder(t *testing.T) {
	const uid = "01a08fe00b8b7c21bd95e11109692b80"
	dir := t.TempDir()

	// 字母序在前：已过期
	writeCred(t, dir, "codearts-aaa.json",
		credJSON(uid, "AK_STALE", "stale", 1000000000, "RT_STALE", true))
	// 字母序在后：更新（应胜出）
	writeCred(t, dir, "codearts-zzz.json",
		credJSON(uid, "AK_FRESH", "fresh", 1999999999, "RT_FRESH", true))

	list := loadParsed(t, dir)
	if len(list) != 2 {
		t.Fatalf("LoadDir 读到 %d 个凭证，期望 2", len(list))
	}

	auths, secrets := dedupeCodeartsByUID(list)
	if len(auths) != 1 {
		t.Fatalf("去重后 auths 有 %d 条，期望 1", len(auths))
	}
	got := secrets[uid].(*codearts.Auth)
	if got.AccessKey != "AK_FRESH" {
		t.Errorf("胜出的是 AK=%q，期望 AK_FRESH —— 结果取决于文件名而不是凭证内容", got.AccessKey)
	}
}

// TestDedupeTieBreakPrefersRefreshToken 锁定判据 2：
// expiresAt 相同时，**有 refresh_token** 的胜出（能自动续期）。
func TestDedupeTieBreakPrefersRefreshToken(t *testing.T) {
	const uid = "01a08fe00b8b7c21bd95e11109692b80"
	const sameExpiry = 1893456000
	dir := t.TempDir()

	// aaa 在前：无 refresh_token（不能续期，应被丢弃）
	writeCred(t, dir, "codearts-aaa.json",
		credJSON(uid, "AK_NORT", "nort", sameExpiry, "", true))
	// zzz 在后：有 refresh_token（应胜出）
	writeCred(t, dir, "codearts-zzz.json",
		credJSON(uid, "AK_WITHRT", "withrt", sameExpiry, "RT_OK", true))

	list := loadParsed(t, dir)
	auths, secrets := dedupeCodeartsByUID(list)
	if len(auths) != 1 {
		t.Fatalf("去重后 auths 有 %d 条，期望 1", len(auths))
	}
	got := secrets[uid].(*codearts.Auth)
	if got.AccessKey != "AK_WITHRT" {
		t.Errorf("胜出的是 AK=%q，期望 AK_WITHRT（有 refresh_token）—— "+
			"过期时间打平时应优先选能自动续期的那份", got.AccessKey)
	}
}

// TestDedupeTieBreakPrefersDPoPKey 锁定判据 3：
// expiresAt 与 refresh_token 都打平时，**有 DPoPPrivateKeyJWK** 的胜出。
//
// 为什么这条重要：DPoP 私钥是 codearts 续期的**必需**项 ——
// 即使有 refresh_token，缺私钥也签不出 DPoP proof，换不出新凭证。
func TestDedupeTieBreakPrefersDPoPKey(t *testing.T) {
	const uid = "01a08fe00b8b7c21bd95e11109692b80"
	const sameExpiry = 1893456000
	dir := t.TempDir()

	// aaa 在前：无 DPoP 私钥（应被丢弃）
	writeCred(t, dir, "codearts-aaa.json",
		credJSON(uid, "AK_NODPOP", "nodpop", sameExpiry, "RT_SAME", false))
	// zzz 在后：有 DPoP 私钥（应胜出）
	writeCred(t, dir, "codearts-zzz.json",
		credJSON(uid, "AK_WITHDPOP", "withdpop", sameExpiry, "RT_SAME", true))

	list := loadParsed(t, dir)
	auths, secrets := dedupeCodeartsByUID(list)
	if len(auths) != 1 {
		t.Fatalf("去重后 auths 有 %d 条，期望 1", len(auths))
	}
	got := secrets[uid].(*codearts.Auth)
	if got.AccessKey != "AK_WITHDPOP" {
		t.Errorf("胜出的是 AK=%q，期望 AK_WITHDPOP（有 DPoPPrivateKeyJWK）—— "+
			"缺私钥的凭证无法续期", got.AccessKey)
	}
}

// TestDedupeFullyTiedKeepsFirst 锁定判据 4：
// 三条全部打平时保留**先出现**的（可预期、可复现）。
func TestDedupeFullyTiedKeepsFirst(t *testing.T) {
	const uid = "01a08fe00b8b7c21bd95e11109692b80"
	const sameExpiry = 1893456000
	dir := t.TempDir()

	// 两份凭证除 AK/nickname 外完全一致 —— 判据 1~3 全部打平
	writeCred(t, dir, "codearts-aaa.json",
		credJSON(uid, "AK_FIRST", "first", sameExpiry, "RT_SAME", true))
	writeCred(t, dir, "codearts-zzz.json",
		credJSON(uid, "AK_SECOND", "second", sameExpiry, "RT_SAME", true))

	list := loadParsed(t, dir)
	auths, secrets := dedupeCodeartsByUID(list)
	if len(auths) != 1 {
		t.Fatalf("去重后 auths 有 %d 条，期望 1", len(auths))
	}
	got := secrets[uid].(*codearts.Auth)
	if got.AccessKey != "AK_FIRST" {
		t.Errorf("胜出的是 AK=%q，期望 AK_FIRST（全平保留先出现的）—— "+
			"全平时若随 map 遍历漂移，同一份数据每次启动会选到不同的号，无法复现问题", got.AccessKey)
	}
}

// TestDedupeLogsDiscardedFile 锁定"丢弃时必须记日志"：
// 日志要含 uid 前缀、选中的文件名、被丢弃的文件名。
//
// 为什么必须测日志而不是返回值：丢弃是**副作用**，
// 返回值只体现"剩下几个"，看不出"丢了谁、为什么丢"。
// 用户拿着日志才能自己判断"它是不是丢错了那份"。
func TestDedupeLogsDiscardedFile(t *testing.T) {
	const uid = "01a08fe00b8b7c21bd95e11109692b80"
	dir := t.TempDir()

	writeCred(t, dir, "codearts-aaa.json",
		credJSON(uid, "AK_FRESH", "fresh", 1999999999, "RT_FRESH", true))
	writeCred(t, dir, "codearts-zzz.json",
		credJSON(uid, "AK_STALE", "stale", 1000000000, "RT_STALE", true))

	list := loadParsed(t, dir)

	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	}()

	dedupeCodeartsByUID(list)

	got := buf.String()
	if got == "" {
		t.Fatalf("丢弃凭证时没有产生任何日志 —— 用户无法知道丢了哪份、为什么丢")
	}
	// uid 只打**前 8 位**（不打整串）。
	//
	// ⚠ 这里原来写的是 `strings.Contains(got, uid[:8])` —— 那条**恒真**，
	// 抓不到任何东西。原因：不截断时日志里是**完整的 32 位 uid**，
	// 而它**也包含前 8 位**，所以 Contains 照样为 true。
	//
	// 实测：把 `shortUID` 改成直接返回整串（可编译），全部测试仍然绿 ——
	// 说明"截断"这条行为**当时没有任何断言覆盖**。
	//
	// 正确判据要**双向**：
	//   · 必须含前 8 位（证明它确实打了 uid）
	//   · 必须**不含完整 uid**（证明它确实截断了）
	// 只写前一条 = 假断言；只写后一条 = 打空也能过。
	if !strings.Contains(got, uid[:8]) {
		t.Errorf("日志里没有 uid 前 8 位 %q —— 用户无法辨认是哪个账号。实际日志=%q", uid[:8], got)
	}
	if strings.Contains(got, uid) {
		t.Errorf("日志里出现了**完整 uid**（32 位）—— 应只打前 8 位，"+
			"整串会把真正可读的信息挤掉。实际日志=%q", got)
	}
	// 选中的与被丢弃的文件名都要在
	if !strings.Contains(got, "codearts-aaa.json") {
		t.Errorf("日志里没有选中的文件名 codearts-aaa.json。实际日志=%q", got)
	}
	if !strings.Contains(got, "codearts-zzz.json") {
		t.Errorf("日志里没有被丢弃的文件名 codearts-zzz.json。实际日志=%q", got)
	}
	// 不打完整路径（日志够长了，参考 LoadDir 的做法）
	if strings.Contains(got, dir) {
		t.Errorf("日志里出现了完整路径，应只用 filepath.Base。实际日志=%q", got)
	}
}

// TestDedupeNoLogWhenNoConflict 锚定日志的**另一面**：
// 没有冲突时**不产生任何日志**。
//
// 否则账号一多（每个 uid 都打一行）就是刷屏，
// 真正需要被看见的冲突行会被淹没 —— 与 LoadDir 里"正常文件不产生日志"同一考虑。
func TestDedupeNoLogWhenNoConflict(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "codearts-a.json",
		credJSON("uid-aaa", "AK_A", "a", 1893456000, "RT_A", true))
	writeCred(t, dir, "codearts-b.json",
		credJSON("uid-bbb", "AK_B", "b", 1893456000, "RT_B", true))

	list := loadParsed(t, dir)

	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	}()

	auths, _ := dedupeCodeartsByUID(list)
	if len(auths) != 2 {
		t.Fatalf("两个不同 uid 应保留 2 条，实际 %d", len(auths))
	}
	if s := buf.String(); s != "" {
		t.Errorf("无冲突时产生了日志（会刷屏）。实际日志=%q", s)
	}
}

// TestDedupeUIDsConsistentWithSecrets 锁定 S2 的**结构性不变量**：
// auths 与 secrets 的 uid 集合必须严格一致，且 auths 里没有重复 uid。
//
// 旧实现正是在这里破的：auths 追加 2 条同 uid、secrets 只有 1 个键。
// 两者长度不一致 → 池子按 2 条 upsert 却只有 1 份 secret，
// 白做一次，且"到底算几个账号"没人说得清。
func TestDedupeUIDsConsistentWithSecrets(t *testing.T) {
	dir := t.TempDir()
	// 三个 uid，其中第一个有二重冲突
	writeCred(t, dir, "codearts-1.json", credJSON("uid-dup", "AK_D1", "d1", 1000000000, "", false))
	writeCred(t, dir, "codearts-2.json", credJSON("uid-dup", "AK_D2", "d2", 1999999999, "RT", true))
	writeCred(t, dir, "codearts-3.json", credJSON("uid-solo", "AK_S", "s", 1893456000, "RT_S", true))

	list := loadParsed(t, dir)
	auths, secrets := dedupeCodeartsByUID(list)

	if len(auths) != 2 {
		t.Fatalf("去重后 auths 有 %d 条，期望 2（uid-dup + uid-solo）", len(auths))
	}
	if len(secrets) != len(auths) {
		t.Fatalf("auths %d 条 vs secrets %d 个键 —— 两者 uid 集合不一致，"+
			"池子会按 %d 条 upsert 却只有 %d 份凭证",
			len(auths), len(secrets), len(auths), len(secrets))
	}

	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		if seen[a.UID] {
			t.Errorf("auths 里出现重复 uid %q —— 去重没生效", a.UID)
		}
		seen[a.UID] = true
		if _, ok := secrets[a.UID]; !ok {
			t.Errorf("auths 里的 uid %q 在 secrets 里没有对应凭证", a.UID)
		}
	}
	for uid := range secrets {
		if !seen[uid] {
			t.Errorf("secrets 里的 uid %q 不在 auths 里 —— 会留下没有账号的孤儿凭证", uid)
		}
	}
}

// ---------------------------------------------------------------------------
// S3：日志报去重后的数
// ---------------------------------------------------------------------------

// TestSyncReturnsDedupedCount 锁定 S3：返回值必须是**实际生效数**（去重后）。
//
// 用户正是靠"日志说 2、界面只有 1"才找上来的。
// 若日志与界面不一致，用户只能猜；两者一致，日志才成为可信的观测点。
//
// 两个 uid 各有两份凭证：文件 4 个，但池子里实际只有 2 个账号。
// 因此期望返回值 2，**不是** 4。
func TestSyncReturnsDedupedCount(t *testing.T) {
	dir := t.TempDir()
	// uid-A：两份冲突，过期/新鲜各一
	writeCred(t, dir, "codearts-a1.json", credJSON("uid-A", "AK_A1", "a1", 1000000000, "", false))
	writeCred(t, dir, "codearts-a2.json", credJSON("uid-A", "AK_A2", "a2", 1999999999, "RT_A", true))
	// uid-B：两份冲突，两条都是好的
	writeCred(t, dir, "codearts-b1.json", credJSON("uid-B", "AK_B1", "b1", 1893456000, "RT_B", true))
	writeCred(t, dir, "codearts-b2.json", credJSON("uid-B", "AK_B2", "b2", 1893456000, "RT_B", true))

	// 前置自检：Loader 必须真的看到 4 个文件，否则"去重"这件事没被测到
	list := loadParsed(t, dir)
	if len(list) != 4 {
		t.Fatalf("LoadDir 读到 %d 个文件，期望 4（前置条件：确实存在重复）", len(list))
	}

	p := newTestPool(t)
	got := syncCodeartsAccounts(p, dir)

	if got != 2 {
		t.Errorf("syncCodeartsAccounts 返回 %d，期望 2（去重后的实际账号数）—— "+
			"返回的是「我传了几条」(含重复)，用户看到的日志数字会与界面不一致，只能靠猜", got)
	}

	// 最强的钉子：返回值必须等于**池子实际的 uid 数**。
	// 这一条把"返回值和池子内容一致"变成受约束的事实，
	// 而不是只对着一个写死的 2 做断言。
	inPool := p.AvailableUIDsFor(codearts.ProviderID)
	if len(inPool) != got {
		t.Errorf("返回 %d 但池子里实际有 %d 个可用账号 %v —— 日志与界面不一致",
			got, len(inPool), inPool)
	}
}

// TestSyncSingleCredentialReturnsOne 是最小的正向对照：
// 只有一个文件、没有冲突时，返回 1（防止把去重写成"永远返回 0"或算错）。
func TestSyncSingleCredentialReturnsOne(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "codearts-only.json",
		credJSON("uid-only", "AK_ONLY", "only", 1893456000, "RT", true))

	p := newTestPool(t)
	got := syncCodeartsAccounts(p, dir)
	if got != 1 {
		t.Errorf("单个凭证时返回 %d，期望 1", got)
	}
	if inPool := p.AvailableUIDsFor(codearts.ProviderID); len(inPool) != 1 {
		t.Errorf("池子里有 %d 个账号 %v，期望 1", len(inPool), inPool)
	}
}

// TestSyncEmptyDirReturnsZero 锚定空目录路径：
// 不是错误、不刷日志、返回 0，并且仍然对齐一次池子（把已删除的账号剔掉）。
func TestSyncEmptyDirReturnsZero(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	}()

	p := newTestPool(t)
	if got := syncCodeartsAccounts(p, dir); got != 0 {
		t.Errorf("空目录返回 %d，期望 0", got)
	}
	if s := buf.String(); s != "" {
		t.Errorf("空目录不该产生日志（用户可能还没跑 cmd/login）。实际日志=%q", s)
	}
}

// ---------------------------------------------------------------------------
// 装配层的整体行为：同 uid 冲突后池子里只剩一条
// ---------------------------------------------------------------------------

// TestSyncPoolHasOneEntryForDuplicateUID 从**池子**这一侧确认 S2 的效果：
// 两份同 uid 凭证走完 syncCodeartsAccounts 后，池子里有且只有 1 个账号，
// 并且它持有的 secret 是**更新**那份。
//
// 这条测试跨过了 syncCodeartsAccounts 与 pool 的边界，
// 是"用户看到界面只有 1 个号"这一现象的端到端复现。
func TestSyncPoolHasOneEntryForDuplicateUID(t *testing.T) {
	const uid = "01a08fe00b8b7c21bd95e11109692b80"
	dir := t.TempDir()
	writeCred(t, dir, "codearts-HSTA7F6GTSM91YHGLERV.json",
		credJSON(uid, "HSTA7F6GTSM91YHGLERV", "n1", 1000000000, "", false))
	writeCred(t, dir, "codearts-HSTACXI1XFLQHQ6LYQOS.json",
		credJSON(uid, "HSTACXI1XFLQHQ6LYQOS", "n2", 1999999999, "RT", true))

	p := newTestPool(t)
	if got := syncCodeartsAccounts(p, dir); got != 1 {
		t.Fatalf("返回 %d，期望 1（两份文件同一个 uid）", got)
	}

	inPool := p.AvailableUIDsFor(codearts.ProviderID)
	if len(inPool) != 1 || inPool[0] != uid {
		t.Fatalf("池子里有 %v，期望恰好 [%s]", inPool, uid)
	}

	// secret 必须是更晚过期的那份（HSTACXI1XF...，expiresAt 更大）
	sec, ok := p.SecretOf(uid)
	if !ok {
		t.Fatalf("池子里取不到 uid=%s 的 secret", uid)
	}
	ca, ok := sec.(*codearts.Auth)
	if !ok || ca == nil {
		t.Fatalf("secret 不是 *codearts.Auth（实际 %T）", sec)
	}
	if ca.AccessKey != "HSTACXI1XFLQHQ6LYQOS" {
		t.Errorf("池子里那份 secret 的 AK=%q，期望 HSTACXI1XFLQHQ6LYQOS（更晚过期的那份）—— "+
			"界面看起来正常，但请求会用过期凭证全挂", ca.AccessKey)
	}

	// nickname 跟随胜出那份，保证界面显示与池子内容一致
	if got := p.PickByUID(uid); got == nil || got.Nickname != "n2" {
		t.Errorf("账号昵称=%v，期望 n2（与胜出凭证一致）", got)
	}
}

// newTestPool 造一个**真实** Pool（不是 mock）。
//
// 走真实实现是刻意的：S2/S3 的故障正体现在"返回值 vs 池子实际内容"的差距上，
// 用 mock 复现不出这个差距。
//
// 传空 stateFp：池子全程内存态，既不读也不写 state.json ——
// 测试因此不会污染真实状态文件，也不会起后台落盘 goroutine 在测试结束后写盘。
func newTestPool(t *testing.T) *pool.Pool {
	t.Helper()
	return pool.New("")
}

// 编译期锚点：确认我们断言的 auth.Auth 就是池子用的那个类型
// （接口漂移时这里会先红，而不是等到断言悄悄失效）。
var _ = (*auth.Auth)(nil)
