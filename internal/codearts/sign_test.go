package codearts

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// vectorFile 由已验证的 JS 实现生成（gen-vector.js），确保 Go 实现与其逐字节一致。
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
				t.Skip("未设置 CODEARTS_SECRET_KEY，跳过完整比对（结构测试见 TestCanonicalURI）")
			}

			headers := map[string]string{"X-Sdk-Date": c.Date}
			if c.Method == "POST" {
				if c.Body[0] == '{' {
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

// TestEscapeMatchesJS 确保 URI 编码与 JS 的 encodeURIComponent 对齐。
// 典型差异：Go 的 QueryEscape 把空格编成 '+'，而华为云要求 %20。
func TestEscapeMatchesJS(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a b", "a%20b"},
		{"a+b", "a%2Bb"},
		{"a/b", "a%2Fb"},
		{"a!b", "a%21b"},
		{"a'b", "a%27b"},
		{"a(b)", "a%28b%29"},
		{"a*b", "a%2Ab"},
		{"abc-_.~", "abc-_.~"},
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
