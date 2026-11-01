// regmachine_orphan_test.go T-026b 的两条治理：(A) 缺 userId 的注册机行拒绝落盘；
// (B) 身份升级时把同 session 的旧残档改名迁走。
//
// # 背景（真机数据，2026-11-02）
//
// `auths/monkeycode/` 里曾躺着两份**同一个账号**的凭证：
//
//	monkeycode-session-b4cb99d10c5b5b9c.json   残档：只有 session/api_key/signing_secret/region
//	monkeycode-002d3463-53d0-4d55-8fc5-ea086473c.json  正主：字段齐全
//
// 二者的 `UID()` 不同（前者是 `session-<短哈希>` 回退值，后者是真 userid）
// ⇒ `dedupeMonkeyCodeByUID` 按 UID 去重**不会**合并 ⇒ 池里两行、面板上两行。
// 这两条测试就把"怎么产生的"钉死在代码行为上。
//
// ⚠ 本文件全程**不联网、不碰真实目录**（`t.TempDir()`），只调纯函数与落盘路径。
// ⚠ 本文件的凭证一律是**假值**（.invalid 域 / 固定串）。绝不要粘真实凭证进来 —— 见 AGENTS.md 的安全条目
package monkeycode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// ── (A) 缺 userId 的注册机行 ────────────────────────────────────────────────
//
// ⚠ **结论修正（2026-11-02，实测撞到既有测试后）**：缺 userId 的行**不能拒绝**。
// `regmachine_test.go` 的 `TestImportMinimalRegMachineRow` 钉着一条**用户明确要求**的契约：
// 注册机导出精简到只剩 `{platform, session, region}` 三个字段也必须导得进来
//（"用户哪天手删字段就导不进去了"）。所以 (A) 落地成**可搜索的警告日志**，
// 而"同一账号两份文件"这个真问题由 `SaveFile` 的迁移 (B) 治。
//
// 本组测试因此改成：① 缺 userId 的行**照样导入**（守既有契约）；
// ② 它落成 `monkeycode-session-<hash>.json`（保留"会走 UID 回落"这个事实）；
// ③ **该账号后来带 userId 再落盘 ⇒ 旧档被迁移**（= 真正治住重复的那条，见下方 B 组）。

// 注册机行（platform=monkeycode）。userID 传空串即"缺 userId"那种形态。
func regMachineLine(uid string, withName bool) string {
	name := ""
	if withName {
		name = `"name":"blackwellaudrey12",`
	}
	return `{"platform":"monkeycode","userId":"` + uid + `",` + name +
		`"region":"intl","session":"00000000-0000-4000-8000-000000000000",` +
		`"plan":"basic","balance":50366,"dailyTokenBalance":8115608,"dailyTokenLimit":10000000,` +
		`"email":"a@b.c","password":"TestPassw0rd!FAKE"}`
}

// withoutUserID 把 `"userId":""` 摘掉，模拟"这一行压根没有 userId"。
func withoutUserID(line string) string {
	return strings.Replace(line, `"userId":"",`, ``, 1)
}

// TestRegMachineMissingUserIDStillImportsButFallsBackToSessionName (A：
// 缺 userId **不拒绝**（守既有契约），但必须落成 session 派生的文件名 —— 那正是
// "后来带 userId 再落盘会产生第二份"的前置条件，因此紧接着由 B 的迁移兜住。
func TestRegMachineMissingUserIDStillImportsButFallsBackToSessionName(t *testing.T) {
	p := &Provider{}
	line := withoutUserID(regMachineLine("", true))
	if strings.Contains(line, "userId") {
		t.Fatalf("夹具没做对：这一行仍然带 userId")
	}

	creds, err := p.ImportCredentials(line)
	if err != nil {
		t.Fatalf("缺 userId 的行**必须仍能导入**（既有契约：极简行可导）：%v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("应当产出 1 条凭证，实得 %d", len(creds))
	}
	if !strings.HasPrefix(creds[0].UID, "session-") {
		t.Errorf("缺 userId 时 UID 会回退成 session-<短哈希>（这是实体事实，测试把它钉住）：%q", creds[0].UID)
	}
	if !strings.HasPrefix(creds[0].FileName, "monkeycode-session-") {
		t.Errorf("文件名应当是 session 回落形态，实得 %q", creds[0].FileName)
	}
	// 落盘一次（照调用方的行为），确认它就是一个用 session 命名的正当文件。
	dir := t.TempDir()
	if _, err := SaveFile(dir, mustAuth(t, creds[0])); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	if got := countJSON(t, dir); got != 1 {
		t.Fatalf("目录里应当有 1 个文件，实得 %d：%v", got, listNames(t, dir))
	}
}

// TestRegMachineImportWithUserIDStillWorks (A 正例) 有 userId 的行照常导入（别把好路堵死）。
func TestRegMachineImportWithUserIDStillWorks(t *testing.T) {
	p := &Provider{}
	const uid = "22222222-2222-4222-8222-222222222222"

	creds, err := p.ImportCredentials(regMachineLine(uid, true))
	if err != nil {
		t.Fatalf("带 userId 的注册机行应当照常导入，实得错误：%v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("应当产出 1 条凭证，实得 %d", len(creds))
	}
	if creds[0].UID != uid {
		t.Fatalf("UID 应当是行里的 userId，实得 %q", creds[0].UID)
	}
	if want := "monkeycode-" + uid + ".json"; creds[0].FileName != want {
		t.Fatalf("文件名应当是 %q（真 userid，不带 session- 前缀），实得 %q", want, creds[0].FileName)
	}
	// 落盘一次也要正常（teeth：确认正例能走完整条链）。
	dir := t.TempDir()
	a := mustAuth(t, creds[0])
	if _, err := SaveFile(dir, a); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	if got := countJSON(t, dir); got != 1 {
		t.Fatalf("目录里应当有 1 个文件，实得 %d", got)
	}
}

// TestPureSessionPasteStillWorks (A 边界) **纯 session 粘贴**不受影响 —— 这是最该守住的好路。
//
// 它走 parseAuth 的 ②/③ 分支（`{"auth":{…}}` / 扁平 / 单行 uuid），
// **不经过 fromRegMachine**，所以 UID 用 session 短哈希是设计如此，不该被拒。
func TestPureSessionPasteStillWorks(t *testing.T) {
	p := &Provider{}
	const sess = "00000000-0000-4000-8000-000000000000"

	for _, tc := range []struct {
		name  string
		paste string
	}{
		{"单行 uuid", sess},
		{"扁平 JSON", `{"session":"` + sess + `"}`},
		{"嵌套 JSON", `{"auth":{"session":"` + sess + `","region":"intl"}}`},
	} {
		creds, err := p.ImportCredentials(tc.paste)
		if err != nil {
			t.Errorf("%s：纯 session 粘贴必须仍然可用，实得错误：%v", tc.name, err)
			continue
		}
		if len(creds) != 1 {
			t.Errorf("%s：应当产出 1 条凭证，实得 %d", tc.name, len(creds))
			continue
		}
		// 这一条的 UID 就是 session 回退值 —— 这是**正常**的（不是残档）。
		if !strings.HasPrefix(creds[0].FileName, "monkeycode-session-") {
			t.Errorf("%s：纯 session 粘贴的文件名仍应是 session 回退形态，实得 %q", tc.name, creds[0].FileName)
		}
	}
}

// ── (B) 身份升级时迁移旧残档 ────────────────────────────────────────────────

// legacyFileName 算出"没有 userid 时"那份凭证会落成什么文件名（不写盘，只算名字）。
func legacyFileName(t *testing.T, session string) string {
	t.Helper()
	return FileName(&Auth{Session: session, Region: RegionIntl})
}

// TestSaveFileMigratesLegacySessionFile (B 正例) 同 session 的旧残档被改名、内容未改。
func TestSaveFileMigratesLegacySessionFile(t *testing.T) {
	dir := t.TempDir()
	const sess = "33333333-3333-4333-8333-333333333333"
	const uid = "44444444-4444-4444-8444-444444444444"

	// ① 先造出残档：一份没有 UserID 的凭证（UID 只能回退成 session-<hash>）。
	legacyPath, err := SaveFile(dir, &Auth{Session: sess, Region: RegionIntl})
	if err != nil {
		t.Fatalf("造残档失败：%v", err)
	}
	if want := legacyFileName(t, sess); filepath.Base(legacyPath) != want {
		t.Fatalf("残档文件名应当是 %q，实得 %q", want, filepath.Base(legacyPath))
	}
	legacyBytes, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("读残档失败：%v", err)
	}

	// ② 身份升级：同一 session、但这次带真 userid ⇒ 应当写新文件 + 把残档改名。
	newPath, err := SaveFile(dir, &Auth{Session: sess, UserID: uid, Region: RegionIntl, Nickname: "blackwellaudrey12"})
	if err != nil {
		t.Fatalf("身份升级那次落盘失败：%v", err)
	}
	if want := "monkeycode-" + uid + ".json"; filepath.Base(newPath) != want {
		t.Fatalf("新文件名应当是 %q，实得 %q", want, filepath.Base(newPath))
	}

	// ③ 残档原名不该再存在；应当有一个 .migrated-* 且**内容一字未改**。
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("旧残档应当已被改名（原路径不该还存在），stat err=%v", err)
	}
	migrated := findMigrated(t, dir, filepath.Base(legacyPath))
	if migrated == "" {
		t.Fatalf("没有找到 %s.json.migrated-*（目录内容：%v）", filepath.Base(legacyPath), listNames(t, dir))
	}
	after, err := os.ReadFile(filepath.Join(dir, migrated))
	if err != nil {
		t.Fatalf("读迁移后的文件失败：%v", err)
	}
	if string(after) != string(legacyBytes) {
		t.Errorf("迁移只改名、**不许改内容**：\n旧=%s\n新=%s", legacyBytes, after)
	}
	// 两个文件都在（新 + 迁移后的旧）。
	if got := countJSON(t, dir); got != 2 {
		t.Errorf("目录里应当剩 2 个 .json（新文件 + 迁移后的旧档），实得 %d：%v", got, listNames(t, dir))
	}
}

// TestSaveFileKeepsOtherSessionsLegacyFile (B 负例) session **不同**的残档不许被误伤。
func TestSaveFileKeepsOtherSessionsLegacyFile(t *testing.T) {
	dir := t.TempDir()
	const other = "11111111-1111-4111-8111-111111111111" // 另一个账号的 session
	const sess = "55555555-5555-4555-8555-555555555555"
	const uid = "66666666-6666-4666-8666-666666666666"

	otherPath, err := SaveFile(dir, &Auth{Session: other, Region: RegionIntl})
	if err != nil {
		t.Fatalf("造「别人的残档」失败：%v", err)
	}
	if _, err := SaveFile(dir, &Auth{Session: sess, UserID: uid, Region: RegionIntl}); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}

	// 别人的残档必须原地不动（既没改名、也没被删）。
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatalf("session 不同的残档不该被改名：%v", err)
	}
	if got := findMigrated(t, dir, filepath.Base(otherPath)); got != "" {
		t.Errorf("session 不同的残档被误迁移成了 %q", got)
	}
	if got := countJSON(t, dir); got != 2 {
		t.Errorf("目录里应当有 2 个 .json（别人的残档 + 新文件），实得 %d：%v", got, listNames(t, dir))
	}
}

// 顺带钉住"身份升级迁移"不会把**自己**改名成迁移件（U一样时无操作）。
func TestSaveFileWithoutUserIDDoesNotMigrate(t *testing.T) {
	dir := t.TempDir()
	const sess = "77777777-7777-4777-8777-777777777777"

	if _, err := SaveFile(dir, &Auth{Session: sess, Region: RegionIntl}); err != nil {
		t.Fatalf("首次落盘失败：%v", err)
	}
	// 再落一次（还是没 userid）：文件名相同 ⇒ 覆盖同一份，不该产生 .migrated-*。
	if _, err := SaveFile(dir, &Auth{Session: sess, Region: RegionIntl, Balance: "123"}); err != nil {
		t.Fatalf("二次落盘失败：%v", err)
	}
	if got := countJSON(t, dir); got != 1 {
		t.Errorf("没有 userid 时两次落盘应覆盖同一文件，实得 %d 个：%v", got, listNames(t, dir))
	}
	for _, n := range listNames(t, dir) {
		if strings.Contains(n, ".migrated-") {
			t.Errorf("没有身份升级时不该出现迁移件：%s", n)
		}
	}
}

// ── 小工具 ────────────────────────────────────────────────────────────────

// countJSON 数"凭证件"：含 .json 的都算 —— 迁移件是 `xxx.json.migrated-<ts>`，
// 它**不以 .json 结尾**，所以不能按后缀数（第一版就是这里数的 1 个）。
func countJSON(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for _, name := range listNames(t, dir) {
		if strings.Contains(name, ".json") {
			n++
		}
	}
	return n
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败：%v", err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// findMigrated 找 `<base>.migrated-*` 形式的文件名（找不到返回空串）。
func findMigrated(t *testing.T, dir, base string) string {
	t.Helper()
	for _, name := range listNames(t, dir) {
		if strings.HasPrefix(name, base+".migrated-") {
			return name
		}
	}
	return ""
}

// mustAuth 把导入结果还原成 *Auth（走真实解析路径，不手工拼字段）。
func mustAuth(t *testing.T, c gateway.ImportedCredential) *Auth {
	t.Helper()
	a, err := parseAuth(c.Raw)
	if err != nil {
		t.Fatalf("回读导入结果失败：%v", err)
	}
	return a
}
