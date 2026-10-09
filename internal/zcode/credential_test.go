// credential_test.go 凭证层与导入解析的守卫。
//
// # 这些断言守的是"管理员真的会遇到的路径"
//
// 凭证层是唯一由**用户输入**驱动的地方（粘贴 Key、手写 JSON、从官方客户端
// 拷凭证文件）。这里的错误不会崩，只会表现为"加了账号但用不了" ——
// 而排查它要从"以为是上游问题"一路查到"导入时少解析了一个字段"。
package zcode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 凭证文件两种形态都要认。
//
//	{"auth": {...}}  本包落盘的标准形态
//	{...}            手写的 / 从别处抄来的
//
// 只认一种会让"用户手写一份凭证"这个最常见的用法静默失败。
func TestLoadFileAcceptsBothShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 期望取到的 api_key
	}{
		{
			name: "包装形态 auth",
			raw:  `{"auth":{"kind":"api-key","api_key":"aaa.bbb"}}`,
			want: "aaa.bbb",
		},
		{
			name: "平铺形态",
			raw:  `{"kind":"api-key","api_key":"ccc.ddd"}`,
			want: "ccc.ddd",
		},
		{
			name: "包装形态 + remark",
			raw:  `{"auth":{"api_key":"eee.fff"},"remark":"备用号"}`,
			want: "eee.fff",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := parseAuth([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseAuth 失败: %v", err)
			}
			if a.APIKey != tc.want {
				t.Fatalf("api_key = %q，期望 %q", a.APIKey, tc.want)
			}
			if a.UID == "" {
				t.Error("UID 应被自动派生（空 UID 会让多个账号在池里互相覆盖）")
			}
		})
	}
}

// 空凭证必须被拒（否则会并池成一个"看着有、用不了"的账号）。
func TestParseAuthRejectsEmptyToken(t *testing.T) {
	for _, raw := range []string{
		`{"auth":{"kind":"api-key"}}`,
		`{"auth":{"api_key":"   "}}`,
		`{}`,
	} {
		if a, err := parseAuth([]byte(raw)); err == nil {
			t.Errorf("空凭证 %s 应被拒，却拿到 %+v", raw, a)
		}
	}
}

// API Key 的 UID 兜底必须**不是明文前缀**。
//
// # 为什么这条要守
//
// 凭证文件常被贴到 issue 里求助。如果 UID 用了明文 key 前 8 位，
// 那求助帖就等于泄露了一半密钥。用哈希则两头都满足：
// 同一 Key 每次派生出同一个 UID（能去重），但从中推不出密钥。
func TestUIDFallbackDoesNotLeakKeyPrefix(t *testing.T) {
	secret := "sk-super-secret-value-0123456789.verysecretpart"
	a := &Auth{Kind: CredKindAPIKey, APIKey: secret}
	a.ensureUID()

	if a.UID == "" {
		t.Fatal("UID 应被派生")
	}
	if strings.Contains(a.UID, "sk-super") {
		t.Errorf("UID 泄露了密钥前缀：%q", a.UID)
	}
	if strings.Contains(a.UID, "verysecret") {
		t.Errorf("UID 泄露了密钥内容：%q", a.UID)
	}
	// 同一 Key 必须稳定（否则重启后账号池会出现重复账号）。
	b := &Auth{Kind: CredKindAPIKey, APIKey: secret}
	b.ensureUID()
	if a.UID != b.UID {
		t.Errorf("同一 Key 派生出不同 UID：%q vs %q", a.UID, b.UID)
	}
	// 不同 Key 必须不同。
	c := &Auth{Kind: CredKindAPIKey, APIKey: secret + "x"}
	c.ensureUID()
	if a.UID == c.UID {
		t.Error("不同 Key 派生出相同 UID —— 账号池会把它们合成一个")
	}
}

// 凭证类型的归一化。
//
// 空 kind 按 api-key 处理的理由：猜错的代价是明确的鉴权失败
// （用 api-key 打 Anthropic 端点），比直接拒绝加载更容易排障。
func TestKindNormalization(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{"", CredKindAPIKey},
		{CredKindAPIKey, CredKindAPIKey},
		{CredKindJWT, CredKindJWT},
		{"oauth", CredKindJWT},       // 别名
		{"coding-plan", CredKindJWT}, // 别名
		{"JWT", CredKindJWT},         // 大小写不敏感
		{"unknown-thing", CredKindAPIKey},
	}
	for _, tc := range cases {
		a := &Auth{Kind: tc.kind, APIKey: "k", JWT: "j"}
		if got := a.KindOf(); got != tc.want {
			t.Errorf("KindOf(%q) = %q，期望 %q", tc.kind, got, tc.want)
		}
	}
}

// Token 必须按通道返回对应的令牌。
//
// 发错令牌（比如给 JWT 通道发 API Key）会得到 401，
// 而 401 在本上游的处置是"标记需重登"—— 代价很高。
func TestTokenSelectsByChannel(t *testing.T) {
	key := &Auth{Kind: CredKindAPIKey, APIKey: "the-key", JWT: "the-jwt"}
	if got := key.Token(); got != "the-key" {
		t.Errorf("API Key 通道 Token() = %q，期望 the-key", got)
	}
	jwt := &Auth{Kind: CredKindJWT, APIKey: "the-key", JWT: "the-jwt"}
	if got := jwt.Token(); got != "the-jwt" {
		t.Errorf("JWT 通道 Token() = %q，期望 the-jwt", got)
	}
	// 只有 JWT 没有 API Key 时也算可用。
	onlyJWT := &Auth{Kind: CredKindJWT, JWT: "j"}
	if !onlyJWT.Usable() {
		t.Error("只有 JWT 的凭证应可用")
	}
}

// 过期判定：**没有过期信息不能算过期**。
//
// # 为什么这条很要紧
//
// 账号池会按"过期"禁用账号。若把"没有过期信息"当成已过期，
// 那么 API Key 通道的所有账号会在启动瞬间全被禁用 ——
// 现象是"账号都在列表里、但一个都用不了"。
func TestExpiryNeverAssumesExpiredWhenUnknown(t *testing.T) {
	now := time.Now()
	noInfo := &Auth{Kind: CredKindAPIKey, APIKey: "k"}
	if noInfo.Expired(now) {
		t.Error("没有过期信息时不该判为已过期 —— 那会把所有 API Key 账号在启动时禁用")
	}
	if ms := noInfo.ExpiresAtMS(); ms != 0 {
		t.Errorf("无过期信息时 ExpiresAtMS 应为 0，实际 %d", ms)
	}

	past := &Auth{Kind: CredKindJWT, JWT: "j", ExpiresAt: now.Add(-time.Hour).Unix()}
	if !past.Expired(now) {
		t.Error("过去的过期时刻应判为已过期")
	}

	future := &Auth{Kind: CredKindJWT, JWT: "j", ExpiresAt: now.Add(time.Hour).Unix()}
	if future.Expired(now) {
		t.Error("未来的过期时刻不该判为已过期")
	}
}

// 秒 → 毫秒的转换（本仓约定是毫秒）。
//
// ⚠ 单位错了的后果很隐蔽：一个 1.7e9 的"毫秒"时间戳看起来就像 1970 年，
// 于是账号被立刻判过期；反过来则永远不过期。两种都不会报错。
func TestExpiresAtMSConvertsSecondsToMillis(t *testing.T) {
	const sec = 1793000000 // 2026 年附近的某个时刻
	a := &Auth{Kind: CredKindJWT, JWT: "j", ExpiresAt: sec}
	if got := a.ExpiresAtMS(); got != sec*1000 {
		t.Fatalf("ExpiresAtMS() = %d，期望 %d（秒→毫秒）", got, sec*1000)
	}
}

// LoadDir 的健壮性：目录不存在不是错、坏文件不拖垮整体、隐藏文件被跳过。
func TestLoadDirToleratesBadFilesAndMissingDir(t *testing.T) {
	// 目录不存在 → 空列表，无错误（首次运行是正常路径）。
	got, err := LoadDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("目录不存在不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("应返回空列表，实际 %d 项", len(got))
	}

	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("zcode-good.json", `{"auth":{"kind":"api-key","api_key":"good.key","uid":"u-good"}}`)
	write("zcode-bad.json", `{ this is not json`)
	write("zcode-empty.json", `{"auth":{}}`)
	write(".hidden.json", `{"auth":{"api_key":"hidden"}}`)
	write("backup.json.bak", `{"auth":{"api_key":"bak"}}`)
	write("note.txt", `hello`)

	got, err = LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir 不该因坏文件报错: %v", err)
	}
	if len(got) != 1 {
		var uids []string
		for _, a := range got {
			uids = append(uids, a.UID)
		}
		t.Fatalf("应只加载 1 份好凭证，实际 %d 份: %v", len(got), uids)
	}
	if got[0].APIKey != "good.key" {
		t.Errorf("加载到的凭证不对: %+v", got[0])
	}
	// FilePath 必须回填（删账号要连文件一起删，缺了会出现"删除后重启复活"）。
	if got[0].FilePath == "" {
		t.Error("FilePath 未回填 —— 删账号时文件不会被移除，重启后账号会复活")
	}
	if !strings.HasSuffix(got[0].FilePath, "zcode-good.json") {
		t.Errorf("FilePath 不对: %q", got[0].FilePath)
	}
}

// SaveFile 必须落盘成标准形态，且**能被自己读回来**。
func TestSaveFileRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "zcode")
	a := &Auth{
		Kind:       CredKindAPIKey,
		APIKey:     "round.trip",
		Nickname:   "测试号",
		DeviceMid:  "11111111-2222-4333-8444-555555555555",
		CodingPlan: true,
	}
	path, err := SaveFile(dir, a)
	if err != nil {
		t.Fatalf("SaveFile 失败: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), "zcode-") {
		t.Errorf("文件名应带 zcode- 前缀: %s", path)
	}

	back, err := LoadFile(path)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if back.APIKey != a.APIKey || back.Nickname != a.Nickname ||
		back.DeviceMid != a.DeviceMid || back.CodingPlan != a.CodingPlan {
		t.Errorf("回读不一致：\n原始 %+v\n回读 %+v", a, back)
	}

	// 落盘内容必须是 {"auth": {...}} 形态（本仓其它上游的约定，
	// admin 的批量导入按键名统一解析 —— 破坏它会让导入功能对本上游失灵）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["auth"]; !ok {
		t.Errorf("落盘内容应有顶层 auth 键，实际键: %v", keysOf(probe))
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sanitizeUID 必须清掉文件名非法字符（UID 可能来自 OAuth 的邮箱）。
func TestSanitizeUIDMakesSafeFileNames(t *testing.T) {
	cases := []struct{ in, notWant string }{
		{"user@example.com", "@"},
		{"a/b\\c", "/"},
		{"a:b", ":"},
		{"a b", " "},
	}
	for _, tc := range cases {
		got := sanitizeUID(tc.in)
		if strings.Contains(got, tc.notWant) {
			t.Errorf("sanitizeUID(%q) = %q 仍含非法字符 %q", tc.in, got, tc.notWant)
		}
		if got == "" {
			t.Errorf("sanitizeUID(%q) 返回空串（会让文件名变成 zcode-.json）", tc.in)
		}
	}
	// 超长 UID 要截断（Windows 文件名上限 255）。
	long := sanitizeUID(strings.Repeat("a", 500))
	if len(long) > 80 {
		t.Errorf("超长 UID 未截断：%d 字符", len(long))
	}
}

// 粘贴导入：本上游的 Key 是**两段点分字符串**，用户会从各处复制。
//
// # 为什么必须宽容
//
// 本仓其它上游的导入都是"粘贴一段 JSON"，但 API Key 不是 JSON。
// 而且它常带前后空格、`Bearer ` 前缀、或平台前缀。
// 严格要求 JSON 会让这个功能基本没法用 —— 而这是本上游**最主要**的加号路径。
func TestImportCredentialsAcceptsRealWorldInputs(t *testing.T) {
	p := &Provider{}

	cases := []struct {
		name       string
		pasted     string
		wantKey    string
		wantCP     bool
		wantOrigin string
	}{
		{"裸 Key", "abc123.def456", "abc123.def456", false, ""},
		{"带前后空格", "  abc123.def456  ", "abc123.def456", false, ""},
		{"带 Bearer 前缀", "Bearer abc123.def456", "abc123.def456", false, ""},
		{"带双引号", `"abc123.def456"`, "abc123.def456", false, ""},
		{"平台前缀 zai", "zai:abc123.def456", "abc123.def456", false, originZAI},
		{"平台前缀 bigmodel", "bigmodel:abc123.def456", "abc123.def456", false, originBigModel},
		{"Coding Plan 标记", "cp:abc123.def456", "abc123.def456", true, ""},
		{"平台 + CP", "zai:cp:abc123.def456", "abc123.def456", true, originZAI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := p.ImportCredentials(tc.pasted)
			if err != nil {
				t.Fatalf("导入失败: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("应得到 1 条，实际 %d 条", len(out))
			}
			a, err := parseAuth(out[0].Raw)
			if err != nil {
				t.Fatalf("产出的凭证解不开: %v", err)
			}
			if a.APIKey != tc.wantKey {
				t.Errorf("api_key = %q，期望 %q", a.APIKey, tc.wantKey)
			}
			if a.CodingPlan != tc.wantCP {
				t.Errorf("CodingPlan = %v，期望 %v", a.CodingPlan, tc.wantCP)
			}
			if a.Origin != tc.wantOrigin {
				t.Errorf("Origin = %q，期望 %q", a.Origin, tc.wantOrigin)
			}
			if out[0].UID == "" {
				t.Error("UID 不能为空（空 UID 会让账号在池里互相覆盖）")
			}
			if out[0].FileName == "" || strings.ContainsAny(out[0].FileName, `/\`) {
				t.Errorf("文件名非法: %q", out[0].FileName)
			}
		})
	}
}

// 多行粘贴（每行一个 Key）与注释行。
func TestImportCredentialsMultiLine(t *testing.T) {
	p := &Provider{}
	pasted := "# 我的号\nabc.111\n\ndef.222\n"
	out, err := p.ImportCredentials(pasted)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("应得到 2 条（注释与空行跳过），实际 %d 条", len(out))
	}
	if out[0].UID == out[1].UID {
		t.Error("两个不同 Key 得到了相同 UID")
	}
}

// JSON 形态的导入（单对象 + 数组 + 部分失败）。
func TestImportCredentialsJSON(t *testing.T) {
	p := &Provider{}

	single, err := p.ImportCredentials(`{"api_key":"one.two","nickname":"一号"}`)
	if err != nil {
		t.Fatalf("单对象导入失败: %v", err)
	}
	if len(single) != 1 {
		t.Fatalf("应 1 条，实际 %d", len(single))
	}
	if single[0].Nickname != "一号" {
		t.Errorf("昵称未解析: %q", single[0].Nickname)
	}

	// 数组里有坏条目 → 好的仍要成功（不能因一条坏的全盘失败）。
	arr, err := p.ImportCredentials(`[{"api_key":"a.1"},{"no_token":true},{"jwt":"jwtval","kind":"jwt"}]`)
	if err != nil {
		t.Fatalf("数组导入失败: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("应导入 2 条好的（跳过 1 条坏的），实际 %d 条", len(arr))
	}

	// 全坏 → 报错，不能静默成功。
	if _, err := p.ImportCredentials(`[{"no_token":true}]`); err == nil {
		t.Error("全部条目都无效时应报错")
	}
}

// TestImportCredentialsMultipleObjects 多个**独立对象**连在一起也要认。
//
// # 守的是用户实测提的需求
//
// 用户："我通常导入的时候会导入多个独立的 JSON 文件，那这个时候它就不行了。
//
//	希望能够自动组合成一个完整的大的 JSON 文件再导入。"
//
// 改造前本包的 ImportCredentials 自己写了一套分支：
//
//	`{` → importJSON（只认单个对象）
//	`[` → importJSONArray
//
// 而"多个独立对象连在一起"是在 gateway.SplitAccountImportItems 里做的 ——
// 本包没走那个共享判据，于是**只有别家上游能导入**，zcode 不能。
// 共享判据抄两遍，必然有一遍漏更新。
func TestImportCredentialsMultipleObjects(t *testing.T) {
	p := &Provider{}

	// 依次粘贴两个独立文件的内容（换行分隔）。
	got, err := p.ImportCredentials(
		"{\"api_key\":\"a.1\",\"nickname\":\"甲\"}\n{\"api_key\":\"b.2\",\"nickname\":\"乙\"}")
	if err != nil {
		t.Fatalf("多个独立对象应当能导入: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应导入 2 条，实际 %d 条: %#v", len(got), got)
	}

	// 逗号分隔（从数组里复制掉方括号）同样要认。
	got2, err := p.ImportCredentials(`{"api_key":"a.1"},{"api_key":"b.2"}`)
	if err != nil {
		t.Fatalf("逗号分隔的多个对象应当能导入: %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("逗号分隔应导入 2 条，实际 %d 条", len(got2))
	}

	// 多条里有一条坏的 → 好的仍要成功（与数组导入同一条容错契约）。
	got3, err := p.ImportCredentials(`{"api_key":"a.1"}` + "\n" + `{"no_token":true}`)
	if err != nil {
		t.Fatalf("坏条目应被跳过而不是全盘失败: %v", err)
	}
	if len(got3) != 1 {
		t.Fatalf("应导入 1 条好的，实际 %d 条", len(got3))
	}
}

// 裸 API Key 的既有路径不能被这次改动碰坏（回归防线）。
//
// zcode 认"每行一个 Key"这种非 JSON 输入；而多对象支持的实现如果写成
// "统一先包一层 [ ]"，就会把 `sk-xxx` 变成 `["sk-xxx"]` —— 静默改变含义。
func TestImportCredentialsBareKeysUnaffected(t *testing.T) {
	p := &Provider{}
	got, err := p.ImportCredentials("sk-aaa111\nsk-bbb222")
	if err != nil {
		t.Fatalf("每行一个裸 Key 应当能导入: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应导入 2 条，实际 %d 条", len(got))
	}
}

// 时间戳解析要认秒与毫秒两种。
//
// ⚠ 判据是**量级**：大于 1e12 一定是毫秒（1e12 秒 = 公元 33658 年）。
// 单位判错会让账号立刻过期或永不过期，两种都不报错。
func TestParseUnixishHandlesSecondsAndMillis(t *testing.T) {
	const sec = 1793000000
	cases := []struct {
		in   string
		want int64
	}{
		{"1793000000", sec},    // 秒
		{"1793000000000", sec}, // 毫秒 → 归一成秒
		{" 1793000000 ", sec},  // 带空格
		{"", 0},                // 空
		{"abc", 0},             // 非数字
		{"-1", 0},              // 负数无意义
	}
	for _, tc := range cases {
		if got := parseUnixish(tc.in); got != tc.want {
			t.Errorf("parseUnixish(%q) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}

// normalizeOrigin 只认已知平台，未知值必须返回空（而不是编一个 URL）。
func TestNormalizeOrigin(t *testing.T) {
	for _, s := range []string{"zai", "z.ai", "z-ai", originZAI, "ZAI"} {
		if got := normalizeOrigin(s); got != originZAI {
			t.Errorf("normalizeOrigin(%q) = %q，期望 %q", s, got, originZAI)
		}
	}
	for _, s := range []string{"bigmodel", "bigmodel.cn", "open.bigmodel.cn", originBigModel} {
		if got := normalizeOrigin(s); got != originBigModel {
			t.Errorf("normalizeOrigin(%q) = %q，期望 %q", s, got, originBigModel)
		}
	}
	// 未知平台返回空 —— 装配时会走探测，不能瞎猜一个域。
	for _, s := range []string{"", "openai", "anthropic", "unknown", "http://evil.example"} {
		if got := normalizeOrigin(s); got != "" {
			t.Errorf("normalizeOrigin(%q) = %q，未知平台应返回空串", s, got)
		}
	}
}

// DisplayNameOf 在昵称为空时必须给出**能区分**的兜底名。
//
// # 为什么
//
// API Key 通道没有账号概念，昵称常为空。列表里显示空白会让人
// 分不清哪个是哪个账号 —— 而这是排障时第一眼要看的东西。
// 兜底名带 key 尾 4 位：够区分，又不泄露完整密钥。
func TestDisplayNameFallbackIsDistinguishable(t *testing.T) {
	a := &Auth{Kind: CredKindAPIKey, APIKey: "key-aaaa1111"}
	b := &Auth{Kind: CredKindAPIKey, APIKey: "key-bbbb2222"}
	if a.Nickname == "" && b.Nickname == "" {
		na, nb := DisplayNameOf(a), DisplayNameOf(b)
		if na == "" || nb == "" {
			t.Fatalf("兜底名不能为空：%q / %q", na, nb)
		}
		if na == nb {
			t.Errorf("不同 Key 的兜底名相同: %q", na)
		}
		if strings.Contains(na, "key-aaaa1111") {
			t.Errorf("兜底名泄露了完整密钥: %q", na)
		}
	}
	// 有昵称时优先用它。
	named := &Auth{Kind: CredKindAPIKey, APIKey: "k", Nickname: "我的主号"}
	if got := DisplayNameOf(named); got != "我的主号" {
		t.Errorf("DisplayNameOf = %q，期望 我的主号", got)
	}
}

// DisplayUID 在 UID 为空时要派生（不能返回空）。
func TestDisplayUIDNeverEmptyForUsableAuth(t *testing.T) {
	a := &Auth{Kind: CredKindAPIKey, APIKey: "some-key"}
	if got := DisplayUID(a); got == "" {
		t.Error("DisplayUID 对可用凭证不该返回空 —— 空 UID 会让账号在池里互相覆盖")
	}
	if DisplayUID(nil) != "" {
		t.Error("DisplayUID(nil) 应返回空串")
	}
}
