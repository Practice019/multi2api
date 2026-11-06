package codearts

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// vectorFile 由已验证的 JS 实现生成（gen-vector.js），确保 Go 实现与其逐字节一致。
//
// 两套用例：
//
//	· Cases        —— 用**真实账号的 AK**（SK 从环境变量给）生成，属"活凭据锚点"，
//	                  没有 CODEARTS_SECRET_KEY 就 Skip（见 TestSignMatchesJS）。
//	· FixtureCases —— ★ **全合成夹具**（密钥就在文件里），默认 `go test` 即逐字节比对，
//	                  不依赖环境变量、不会 Skip（见 TestSignMatchesFixtures）。
//	                  新增向量请一律加到 FixtureCases，别再往 Cases 里塞。
type vectorFile struct {
	AccessKey string `json:"accessKey"`
	Cases     []struct {
		Name     string `json:"name"`
		Method   string `json:"method"`
		URL      string `json:"url"`
		Body     string `json:"body"`
		Date     string `json:"date"`
		WantAuth string `json:"wantAuth"`
	} `json:"cases"`
	Fixtures struct {
		Note     string `json:"note"`
		Source   string `json:"source"`
		Verified string `json:"verified"`
	} `json:"fixtures"`
	FixtureCases []struct {
		Name          string            `json:"name"`
		Covers        string            `json:"covers"`
		Method        string            `json:"method"`
		URL           string            `json:"url"`
		Body          string            `json:"body"`
		AccessKey     string            `json:"accessKey"`
		SecretKey     string            `json:"secretKey"`
		SecurityToken string            `json:"securityToken"`
		Headers       map[string]string `json:"headers"`
		WantAuth      string            `json:"wantAuth"`
	} `json:"fixtureCases"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata_vector.json")
	if err != nil {
		t.Skipf("向量文件缺失（%v）；先跑 gen-vector.js 生成", err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("解析向量失败: %v", err)
	}
	return v
}

// TestSignMatchesJS 是本包最重要的测试：
// 它把 Go 实现锚定到**已实测通过服务端校验**的 JS 实现上。
// 一旦签名逻辑被改动而破坏一致性，这里立刻失败。
//
// ⚠️ 这组用例（Cases）的 wantAuth 是**带真实 x-security-token** 生成的
// （SignedHeaders 里含 x-security-token，其值参与签名）⇒ 没有真实凭据就**无法**复算，
// 所以无 CODEARTS_SECRET_KEY 时 Skip 是**正确**行为，别把它改成合成密钥：
// 换成合成密钥后这些期望值必然对不上（差在 STS 值上）。
// ★ 需要"默认就跑"的锚点，请用 TestSignMatchesFixtures（全合成夹具）。
func TestSignMatchesJS(t *testing.T) {
	v := loadVectors(t)
	if len(v.Cases) == 0 {
		t.Fatal("向量为空")
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			cred := Credential{
				AccessKey:     v.AccessKey,
				SecretKey:     "", // 见下方说明
				SecurityToken: os.Getenv("CODEARTS_SECURITY_TOKEN"),
			}
			// 向量不含 secretKey（避免把活凭证提交进仓库）。
			// 因此这里只校验「结构性」部分：SignedHeaders 与 URL 处理。
			// 完整比对需要 secretKey，通过环境变量提供。
			if os.Getenv("CODEARTS_SECRET_KEY") != "" {
				cred.SecretKey = os.Getenv("CODEARTS_SECRET_KEY")
			} else {
				t.Skip("未设置 CODEARTS_SECRET_KEY，跳过完整比对（结构测试见 TestCanonicalURI；夹具比对见 TestSignMatchesFixtures）")
			}

			headers := map[string]string{"X-Sdk-Date": c.Date}
			if c.Method == "POST" {
				// ⚠ 用 HasPrefix 而不是 c.Body[0]：空 body 的 POST 会让下标越界 panic
				if strings.HasPrefix(c.Body, "{") {
					headers["Content-Type"] = "application/json"
				} else {
					headers["Content-Type"] = "application/x-www-form-urlencoded"
				}
			}
			got, err := Sign(cred, c.Method, c.URL, c.Body, headers)
			if err != nil {
				t.Fatalf("签名失败: %v", err)
			}
			if got["Authorization"] != c.WantAuth {
				t.Errorf("\n期望: %s\n实际: %s", c.WantAuth, got["Authorization"])
			}
		})
	}
}

// TestCanonicalURI 覆盖本项目最关键的坑：CanonicalURI 必须补尾斜杠。
//
// 依据：上游 401 响应回显的 canonical_request 是
//
//	GET|/v5/caller-identity/||x-sdk-date:...|x-security-token:...||x-sdk-date;x-security-token|<hash>
//
// 注意 `/v5/caller-identity/` 的尾斜杠；不补则签名不匹配、恒 401。
func TestCanonicalURI(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v5/caller-identity", "/v5/caller-identity/"},
		{"/v5/caller-identity/", "/v5/caller-identity/"},
		{"/api/v2/chat/completions", "/api/v2/chat/completions/"},
		{"/", "/"},
		{"/a/b/c", "/a/b/c/"},
	}
	for _, c := range cases {
		if got := canonicalURI(c.path); got != c.want {
			t.Errorf("canonicalURI(%q) = %q, 期望 %q", c.path, got, c.want)
		}
	}
}

// TestSignMatchesFixtures 用**全合成夹具**逐字节比对签名 —— 默认 `go test` 即执行，
// 不需要任何环境变量、不碰真实凭据、不会 Skip。
//
// 这是本包**唯一的常开签名锚点**：改 `escape`/`canonicalURI`/`Sign` 的任何一处，
// 这里都必须立刻变红。第 5 组专门钉 T-228 修的 P0（`! ' ( ) *` 不得转义）。
func TestSignMatchesFixtures(t *testing.T) {
	v := loadVectors(t)
	if len(v.FixtureCases) == 0 {
		t.Fatal("fixtureCases 为空：请从 register-machine 的 T-220 金向量补入")
	}
	for _, c := range v.FixtureCases {
		t.Run(c.Name, func(t *testing.T) {
			cred := Credential{AccessKey: c.AccessKey, SecretKey: c.SecretKey, SecurityToken: c.SecurityToken}
			got, err := Sign(cred, c.Method, c.URL, c.Body, c.Headers)
			if err != nil {
				t.Fatalf("签名失败: %v", err)
			}
			if got["Authorization"] != c.WantAuth {
				t.Errorf("覆盖点：%s\n期望: %s\n实际: %s", c.Covers, c.WantAuth, got["Authorization"])
			}
		})
	}
}

// TestCanonicalRequestHasBlankLine 钉住 register-machine T-220 发现的**坑②**：
// canonicalHeaders 是逐头拼 `name:value\n`（自带尾 \n），CanonicalRequest 又用
// strings.Join(..., "\n") 拼 ⇒ SignedHeaders 前**多一个空行**。
//
// 这是**正确行为**，不是笔误：去掉它上游回 APIG.0301（签名错误）。
// 依据：signer.rs 的 sign_material 与官方文档一致（见 deep-dive-01-signer.md §2.2）。
func TestCanonicalRequestHasBlankLine(t *testing.T) {
	cred := Credential{AccessKey: "AKID", SecretKey: "SECRET"}
	headers := map[string]string{"X-Sdk-Date": "20260101T000000Z", "Host": "h"}
	cr, err := CanonicalRequest(cred, "GET", "https://h/a", "", headers)
	if err != nil {
		t.Fatalf("CanonicalRequest 失败: %v", err)
	}
	// 最后一个规范头（x-sdk-date:…）之后必须是 TWO 个 \n，紧接 SignedHeaders
	want := "x-sdk-date:20260101T000000Z\n\nhost;x-sdk-date"
	if !strings.Contains(cr, want) {
		t.Fatalf("SignedHeaders 前的空行丢了（上游会回 APIG.0301）\n期望包含: %q\n实际: %q", want, cr)
	}
	// 反向断言：规范头段本身必须**以 \n 结尾**（这才是空行的来源）
	if !strings.Contains(cr, "host:h\nx-sdk-date:20260101T000000Z\n") {
		t.Errorf("规范头段应逐行以 \\n 结尾（含末尾）：%q", cr)
	}
}

// TestEscapeMatchesJS 确保 URI 编码与 JS 的 encodeURIComponent 对齐。
// 典型差异：Go 的 QueryEscape 把空格编成 '+'、且会把 !'()* 也转义，两者都与华为云要求不符。
//
// ★ T-228：`! ' ( ) *` 五字符从"应转义"改为"**不转义**"（原来那 4 条期望值是错的，
//
//	它们与本测试的名字自相矛盾 —— JS 里 encodeURIComponent("a!b") === "a!b"）。
func TestEscapeMatchesJS(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a b", "a%20b"},             // 空格 → %20（不是 +）
		{"a+b", "a%2Bb"},             // + 必须转义
		{"a/b", "a%2Fb"},             // / 必须转义（逐段编码里它是分隔符）
		{"a!b", "a!b"},               // ★ 不转义
		{"a'b", "a'b"},               // ★ 不转义
		{"a(b)", "a(b)"},             // ★ 不转义
		{"a*b", "a*b"},               // ★ 不转义
		{"abc-_.~", "abc-_.~"},       // 标准未保留集
		{"a@b", "a%40b"},             // @ 要转义
		{"a&b=c", "a%26b%3Dc"},       // & = 要转义
		{"a#b", "a%23b"},             // # 要转义
		{"a$b", "a%24b"},             // $ 要转义
		{"中文", "%E4%B8%AD%E6%96%87"}, // UTF-8 逐字节、大写 hex
		{"a!b'c(d)e*f~g-h.i_j", "a!b'c(d)e*f~g-h.i_j"}, // ★ 完整保留集一次过
	}
	for _, c := range cases {
		if got := escape(c.in); got != c.want {
			t.Errorf("escape(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestSDKDate 校验时间格式（20260101T000000Z）。
func TestSDKDate(t *testing.T) {
	tm := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := SDKDate(tm); got != "20260101T000000Z" {
		t.Errorf("SDKDate = %q, 期望 20260101T000000Z", got)
	}
}

// TestSecurityTokenHeader 确认临时凭证会被自动注入请求头。
func TestSecurityTokenHeader(t *testing.T) {
	cred := Credential{AccessKey: "AK", SecretKey: "SK", SecurityToken: "TOK"}
	got, err := Sign(cred, "GET", "https://example.com/x", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["X-Security-Token"] != "TOK" {
		t.Errorf("缺少 X-Security-Token，实际头: %v", got)
	}
	if got["Authorization"] == "" {
		t.Error("缺少 Authorization")
	}
}
