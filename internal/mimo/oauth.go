// oauth.go MiMo 官方登录协议：X25519 加密回调 → 长期 sk。
//
// 逐段对齐官方 MiMo-Code plugin/mimo.ts（报告 §2.3，行号见引用处）：
//
//  1. 生成 X25519 密钥对，pk = base64url(SPKI-DER)
//  2. 授权 URL：GET {platform}/authorize?pk=…&redirect_uri=…&kn=mimocode&key_name=…
//  3. 用户浏览器授权 → 回跳 redirect_uri?u=<base64url 密文>
//  4. 密文 u = ephemeralPub(32B) ‖ nonce(12B) ‖ ciphertext ‖ tag(16B)
//     共享密钥 = SHA256(X25519(我方私钥, 对方临时公钥))，AES-256-GCM
//  5. 明文 JSON {"sk","uid","url?"} —— url=账号专属区域网关（必须存进凭证）
//  6. 浏览器侧 302 回 {platform}/authorize/callback?status=success|error
//
// key_name 持久化在 {authDir}/mimo-key-name（官方同款：复用同一名字避免
// 在用户控制台堆垃圾 key）。
package mimo

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"crypto/aes"
	"crypto/cipher"
)

// PlatformBase 授权站点（可被配置覆盖的默认值）。
const PlatformBase = "https://platform.xiaomimimo.com"

// oauthSession 一次进行中的加密登录。
type oauthSession struct {
	priv    *ecdh.PrivateKey
	keyName string
	state   string
}

// newOAuthSession 生成密钥对与 key_name。
func newOAuthSession() (*oauthSession, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mimo: X25519 密钥生成失败: %w", err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &oauthSession{
		priv:    priv,
		keyName: fmt.Sprintf("mimo-code-cli-key-%s", hex.EncodeToString(b)),
		state:   randHex(16),
	}, nil
}

// authorizeURL 构造授权链接 —— 参数集与用户 v2 探测报告 §7.2 逐字对齐：
//
//	{platform}/authorize?pk={pub}&redirect_uri={回调}&kn=mimocode&key_name={key名}&app=MiMo
//
// ⚠ 曾经的偏差（v2 报告纠正）：kn 是**固定渠道名 "mimocode"**，key_name 才是
// 生成的钥匙名 —— 我们一度把钥匙名塞进了 kn（并靠平台宽容才没炸），且缺
// app=MiMo。manual 模式下 redirect_uri 指平台 code/callback 页（展示加密
// code 供粘贴），auto 模式指本机 127.0.0.1:port。
func authorizeURL(platform, pkB64URL, redirect, keyName string) string {
	q := url.Values{}
	q.Set("pk", pkB64URL)
	q.Set("redirect_uri", redirect)
	q.Set("kn", "mimocode")
	q.Set("key_name", keyName)
	q.Set("app", "MiMo")
	return platform + "/authorize?" + q.Encode()
}

// spkiPrefixX25519 X25519 公钥的 DER(SPKI) 固定头（12 字节）。
// base64url 后即用户教程 §3.2 示例里 pk=MCowBQYDK2VuAyEA… 的那个前缀。
var spkiPrefixX25519 = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e, 0x03, 0x21, 0x00}

// pubKeyB64URL pk=base64url(SPKI DER) —— **不是裸 32 字节**！
//
// 官方桌面端与用户手动登录教程（§3.1 generateKeyPairSync spki/der）都以
// SPKI 编码交换 pk；发 raw32 会让平台侧按错误结构解出对不上的公钥，
// ECDH 共享密钥整体错位 → 我们解密必败。早先的测试是"自己加密自己解密"
// 的闭环，钉不住与官方的一致性 —— 教训同 TRAE：假上游全绿 ≠ 真上游能跑。
// u 参数里的临时公钥仍是 raw32（官方拆法 r.subarray(0,32) 为证），解 u 不变。
func (o *oauthSession) pubKeyB64URL() string {
	spki := make([]byte, 0, len(spkiPrefixX25519)+32)
	spki = append(spki, spkiPrefixX25519...)
	spki = append(spki, o.priv.PublicKey().Bytes()...)
	return base64.RawURLEncoding.EncodeToString(spki)
}

// DecryptCallbackU 解回调 `u` 参数 → {sk,uid,url}。
//
// u 的两种包装（官方/平台回跳都出现）：base64url(整包) 或 base64url 带填充；
// 都试。整包布局：pub(32) ‖ nonce(12) ‖ ct ‖ tag(16)。
func (o *oauthSession) DecryptCallbackU(u string) (sk, uid, accountBase string, err error) {
	raw, err := uDecodeLoose(u)
	if err != nil {
		return "", "", "", fmt.Errorf("mimo: u 参数不是 base64url: %w", err)
	}
	const pubLen, nonceLen, tagLen = 32, 12, 16
	if len(raw) < pubLen+nonceLen+tagLen+1 {
		return "", "", "", fmt.Errorf("mimo: u 参数长度异常（%d 字节，不足以容纳密文）", len(raw))
	}
	ephPub, err := ecdh.X25519().NewPublicKey(raw[:pubLen])
	if err != nil {
		return "", "", "", fmt.Errorf("mimo: u 里的临时公钥非法: %w", err)
	}
	shared, err := o.priv.ECDH(ephPub)
	if err != nil {
		return "", "", "", fmt.Errorf("mimo: ECDH 失败: %w", err)
	}
	key := sha256.Sum256(shared)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", "", err
	}
	nonce := raw[pubLen : pubLen+nonceLen]
	ct := raw[pubLen+nonceLen:]
	plain, err := gcm.Open(nil, nonce, ct, nil) // ct 尾部含 tag（GCM 约定）
	if err != nil {
		return "", "", "", fmt.Errorf("mimo: AES-GCM 解密失败（密钥不匹配或数据被改）: %w", err)
	}
	var payload struct {
		SK  string `json:"sk"`
		UID any    `json:"uid"` // 平台可能回数字或字符串
		URL string `json:"url"`
	}
	if err := json.Unmarshal(plain, &payload); err != nil {
		return "", "", "", fmt.Errorf("mimo: 解密结果不是合法 JSON: %w", err)
	}
	if strings.TrimSpace(payload.SK) == "" {
		return "", "", "", fmt.Errorf("mimo: 解密结果缺 sk")
	}
	uid = anyToUIDString(payload.UID)
	return payload.SK, uid, strings.TrimRight(payload.URL, "/"), nil
}

func anyToUIDString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	case json.Number:
		return t.String()
	}
	return ""
}

func uDecodeLoose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// loadOrCreateKeyName 持久化 key_name（同 trae 的"设备对前后一致"纪律：
// 上游以 key_name 识别我们的注册，每次换新会在用户控制台堆垃圾授权）。
func loadOrCreateKeyName(authDir string) (string, error) {
	path := filepath.Join(authDir, "mimo-key-name")
	if raw, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(raw)); s != "" {
			return s, nil
		}
	}
	name := "mimo-code-cli-key-" + randHex(4)
	if authDir != "" {
		if err := os.MkdirAll(authDir, 0o755); err == nil {
			_ = os.WriteFile(path, []byte(name+"\n"), 0o600)
		}
	}
	return name, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── 本机官方客户端目录探测（批量导入的 pick_local 源）──────────────────
//
// 路径候选 = 官方 global.ts:26-52 + MiMo2API token-store.ts:9-30 的并集。
// 只**读**，永不回写官方文件（官方弃用 env 注入的泄密教训同理：网关不往
// 客户端生态里写任何东西）。

// clientAuthPathCandidates 官方 auth.json 的可能位置（按优先级）。
func clientAuthPathCandidates(override string) []string {
	if strings.TrimSpace(override) != "" {
		return []string{filepath.Join(override, "data", "auth.json"), filepath.Join(override, "auth.json")}
	}
	var out []string
	if h := strings.TrimSpace(os.Getenv("MIMOCODE_HOME")); h != "" {
		out = append(out, filepath.Join(h, "data", "auth.json"))
	}
	if hd, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(hd, ".local", "share", "mimocode", "data", "auth.json"),                // Linux XDG
			filepath.Join(hd, "Library", "Application Support", "mimocode", "data", "auth.json"), // macOS
			filepath.Join(hd, ".mimocode", "data", "auth.json"),                                  // 兜底
		)
	}
	if la := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); la != "" { // Windows
		out = append(out, filepath.Join(la, "mimocode", "data", "auth.json"))
	}
	return out
}

// ParseClientAuthJSON 解官方整包 auth.json（{providers:{xiaomi:<entry>}} 或
// 顶层直接是 provider map）→ 全部可导入条目（归一由 Parse 完成）。
//
// ⚠ MIMOCODE_AUTH_CONTENT 环境变量形态**不读**：官方弃用它正是因为
// CLI 的 bash 工具会 echo 泄密（auth/index.ts:11-20）。文件形态才可信。
func ParseClientAuthJSON(raw []byte) ([]*Auth, error) {
	var doc struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Providers) == 0 {
		// 退化：整包就是 provider→entry 的 map（无 providers 包装）。
		var flat map[string]json.RawMessage
		if json.Unmarshal(raw, &flat) != nil {
			return nil, fmt.Errorf("mimo: auth.json 结构不识别")
		}
		doc.Providers = flat
	}
	var out []*Auth
	for name, entry := range doc.Providers {
		if !strings.EqualFold(name, "xiaomi") && !strings.EqualFold(name, "mimo") {
			continue // 只吃 MiMo 域，别家 provider 不越权
		}
		a, err := Parse(entry)
		if err != nil {
			// 单条坏不拖整包（比如 wellknown 形态没有 key）。
			continue
		}
		if a.Source == "" {
			a.Source = "authjson"
		}
		out = append(out, a)
	}
	return out, nil
}
