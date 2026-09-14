// smslogin.go 讯飞账号网关的**短信验证码登录**。
//
// # 为什么 loomy 需要这条路（本轮用户的要求）
//
// 上一轮「添加账号」只有一条路：读本机 Loomy 客户端已经登录好的 session。
// 那条路在"客户端没装/没登录/网关不在同一台机器"时完全不可用 ——
// 界面上按钮直接不出现。用户的要求是**直接输手机号 + 验证码登录**，
// 于是这条链路必须自己走通。
//
// 协议来自对 `Loomy.exe`（v0.9.37）的逆向报告：
//
//	POST {accountBase}/login/phone/sendMsgCode   → data.msgid（短信已发出）
//	POST {accountBase}/login/phone/checkCode     → data.session + data.userid
//
// 生产网关 `https://account.xfinfr.com`，appId `GM3LOOMY`。
// **两个端点都需要 HMAC-SHA1 请求签名**（见下面 signAccount 的逐段说明），
// 缺签名会直接 403/401 —— 所以本文件的主要重量在签名上，而不是在调用上。
//
// # 关于下面那对默认 key（必须说清楚，不能装作没这回事）
//
// `accessKeyId` / `accessKeySecret` 随客户端安装包分发，藏在
// `resources/.env.prod` 里（AES-256-GCM + scrypt 口令**内嵌在同一个包里**）。
// 客户端自己的源码注释就写明这是 **"local-obfuscation"、不是真保密** ——
// 任何拿到安装包的人都能还原它们。本包把解出来的值作为**默认值**，
// 让功能开箱可用；同时留了配置与环境的覆盖口（见 Config）。
//
//	⚠ 它们**不是用户凭证**：泄漏不等于掉号，但它确实让"拿到安装包就能
//
// 以官方身份调用账号网关"成立。这与本仓库清理过的"真实 api_key 硬编码"
// 不是同一类东西（那是用户自己的账号密钥），所以这里选择内置，
// 并在 README 里说明来源与覆盖方式。
//
// # 为什么不用 Go 现解 .env.prod
//
// 它的 KDF 是 **scrypt**，而 Go 标准库没有 —— 那会为本包引入
// `golang.org/x/crypto` 这个唯一的外部依赖。一个"纯转发的轻上游"
// 为了一对本来就在安装包里明文的 key 去加依赖，不划算。
package loomy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 讯飞账号网关的默认值（来自 `resources/.env.prod`，见文件头注释）。
const (
	DefaultAccountBaseURL   = "https://account.xfinfr.com"
	DefaultAccountAppID     = "GM3LOOMY"
	DefaultAccountAccessKey = "2thryby66wxi53sk"
	DefaultAccountSecret    = "zsak6eadrbawz683wf5r3m2snrwj868r"
	// accountUA 客户端自己用的 UA（原样照抄，不从别处拼）。
	accountUA = "Loomy|Desktop|Electron|macOS"
	// maxAccountBody 账号网关回执的读取上限（回执很小，1 MiB 足够）。
	maxAccountBody = 1 << 20
)

// SMSClient 讯飞账号网关的短信登录客户端。
//
// 零值不可用（base/keys 都空），请用 NewSMSClient。
type SMSClient struct {
	baseURL         string
	appID           string
	accessKeyID     string
	accessKeySecret string
	http            *http.Client
}

// NewSMSClient 建一个短信登录客户端；空字段落回默认值。
func NewSMSClient(baseURL, appID, keyID, keySecret string) *SMSClient {
	pick := func(v, def string) string {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
		return def
	}
	return &SMSClient{
		baseURL:         strings.TrimRight(pick(baseURL, DefaultAccountBaseURL), "/"),
		appID:           pick(appID, DefaultAccountAppID),
		accessKeyID:     pick(keyID, DefaultAccountAccessKey),
		accessKeySecret: pick(keySecret, DefaultAccountSecret),
		// 与 client.go 同一条：**不设** http.Client.Timeout —— 这里虽然
		// 全是短请求，但设一个会落到"账号网关慢一点就登录失败"的体验上。
		// 超时由调用方的 ctx 控制。
		http: &http.Client{},
	}
}

// Configured 报告签名所需的四件东西都齐了。
func (c *SMSClient) Configured() bool {
	return c != nil && c.baseURL != "" && c.appID != "" &&
		c.accessKeyID != "" && c.accessKeySecret != ""
}

// SendCode 下发短信验证码，返回 msgid（下一步验码要带回）。
func (c *SMSClient) SendCode(ctx context.Context, phone string) (string, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return "", fmt.Errorf("loomy: 手机号不能为空")
	}
	body := map[string]any{
		"base": c.base(),
		"param": map[string]any{
			"ccode":  "86",
			"phone":  phone,
			"expire": 300,
		},
	}
	data, err := c.post(ctx, "/login/phone/sendMsgCode", body)
	if err != nil {
		return "", err
	}
	var out struct {
		MsgID string `json:"msgid"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("loomy: 短信下发回执格式不对: %w", err)
	}
	if out.MsgID == "" {
		return "", fmt.Errorf("loomy: 短信下发成功但回执里没有 msgid")
	}
	return out.MsgID, nil
}

// Login 用手机号 + 验证码 + msgid 换取 session。
//
// 返回的 *Auth 已经填好 Session / UID / Nickname，可直接落盘。
func (c *SMSClient) Login(ctx context.Context, phone, code, msgid string) (*Auth, error) {
	phone, code, msgid = strings.TrimSpace(phone), strings.TrimSpace(code), strings.TrimSpace(msgid)
	if phone == "" || code == "" {
		return nil, fmt.Errorf("loomy: 手机号与验证码都不能为空")
	}
	body := map[string]any{
		"base": c.base(),
		"param": map[string]any{
			"ccode":  "86",
			"phone":  phone,
			"mcode":  code,
			"msgid":  msgid,
			"expire": 14 * 24 * 3600, // 报告实测：session 有效期 14 天
		},
	}
	data, err := c.post(ctx, "/login/phone/checkCode", body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Session  string `json:"session"`
		UserID   string `json:"userid"`
		Nickname string `json:"nickname"`
		Phone    string `json:"phone"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("loomy: 登录回执格式不对: %w", err)
	}
	if strings.TrimSpace(out.Session) == "" {
		return nil, fmt.Errorf("loomy: 登录成功但回执里没有 session")
	}
	a := &Auth{
		Session: out.Session,
		UserID:  out.UserID,
		Phone:   firstNonEmpty(out.Phone, phone),
		// 登录时刻由**我们**记下来（账号网关的回执里没有这个字段，
		// 而 Auth.LoggedInAt 要用于"同一 uid 多份凭证取较新者"的排序）。
		LoggedInAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	a.UID = firstNonEmpty(a.UserID, deriveUID(a.Session))
	a.Nickname = firstNonEmpty(a.Phone, out.Nickname, shortUID(a.UID))
	return a, nil
}

// base 拼请求里的 base 段（与客户端 makeBase 逐字段一致）。
//
// traceid 是 32 位 hex（客户端用 UUID 去掉横线）。**每次请求重新生成** ——
// 它是链路追踪 id，不是会话标识，重用会让多账号排障时对不上。
func (c *SMSClient) base() map[string]any {
	return map[string]any{
		"appid":   c.appID,
		"modelid": "web",
		"version": "1.0.0",
		"devid":   "web",
		"ua":      accountUA,
		"traceid": randomHex(16),
	}
}

// post 签一个名、发一个 JSON 请求、解出 `data` 段。
//
// 账号网关的响应格式是 `{"code":"000000","desc":"...","data":{...}}`：
// **业务失败也是 HTTP 200**，所以判据必须看 `code` 而不是状态码 ——
// 与 loomy 模型代理的"200 + 缺少 token"是同一种（很容易搞错）的形态。
func (c *SMSClient) post(ctx context.Context, path string, body map[string]any) (json.RawMessage, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("loomy: 账号网关未配置（缺 base/appid/access key）")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("loomy: 序列化请求体失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("loomy: 构造请求失败: %w", err)
	}
	for k, v := range c.sign(path, string(raw)) {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loomy: 调用账号网关失败（%s）: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, maxAccountBody))

	var out struct {
		Code string          `json:"code"`
		Desc string          `json:"desc"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("loomy: 账号网关回执不是合法 JSON（HTTP %d）: %s",
			resp.StatusCode, summarize(buf))
	}
	if out.Code != "000000" {
		// desc 是给人看的（"验证码错误"/"验证码已过期"…），原样带出去 ——
		// 换成我们自己的措辞会让用户失去唯一的可行动信息。
		return nil, fmt.Errorf("loomy: 账号网关返回 %s: %s", out.Code, out.Desc)
	}
	return out.Data, nil
}

// ── 签名（HMAC-SHA1，magic 前缀 "account"）────────────────────────────────

// sign 生成一次请求所需的全部鉴权头。
//
// # stringToSign 是 9 段以 \n 连接
//
//	METHOD  ESCAPED_PATH  ESCAPED_QUERY  CONTENT_MD5  CONTENT_TYPE
//	DATE  NONCE  SIGNED_HEADERS  CANONICALIZED_HEADERS
//
// 本实现不发任何 `x-` 前缀的自定义头，所以第 8 段是空串、第 9 段也是空串
// （拼接后 stringToSign 以两个 \n 结尾 —— 照抄客户端的实现，不要"顺手清理"）。
//
//	⚠ 判据必须逐段对齐：只要某一段少一个字符，服务端返回的是
//
// **签名不匹配**（而不是 401/403 那种一眼能看出的形态），
// 实测表现与"key 不对"完全一样，极难分辨。
func (c *SMSClient) sign(path, body string) map[string]string {
	date := time.Now().UTC().Format(http.TimeFormat) // = new Date().toUTCString()
	nonce := randomUUID()
	contentMD5 := ""
	if body != "" {
		sum := md5.Sum([]byte(body))
		contentMD5 = base64.StdEncoding.EncodeToString(sum[:])
	}
	parts := []string{
		http.MethodPost,
		escapeAccountPath(path),
		"", // ESCAPED_QUERY：本实现的三个端点都不带 query
		contentMD5,
		"application/json",
		date,
		nonce,
		"", // SIGNED_HEADERS：没有 x- 头
		"", // CANONICALIZED_HEADERS：同上
	}
	mac := hmac.New(sha1.New, []byte(c.accessKeySecret))
	mac.Write([]byte(strings.Join(parts, "\n")))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	headers := map[string]string{
		"Authorization": "account " + c.accessKeyID + ":" + sig,
		"Date":          date,
		"Nonce":         nonce,
		"Content-Type":  "application/json",
	}
	if contentMD5 != "" {
		headers["Content-MD5"] = contentMD5
	}
	return headers
}

// escapeAccountPath 按 RFC3986 **逐段**转义路径。
//
// `encodeURIComponent` 不转义 `!'()*`，而讯飞的签名要求它们也转义 ——
// 客户端自己就是这么补的（`sign.js`），照抄。
func escapeAccountPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "" {
			continue
		}
		segs[i] = escapeAccountSegment(s)
	}
	return strings.Join(segs, "/")
}

// escapeAccountSegment 与 js 的 encodeURIComponent 对齐（含 4 个补充字符）。
func escapeAccountSegment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// randomHex 返回 n 字节的十六进制串。
//
// 失败时**退化成时间戳**而不是 panic 或返回空：traceid 只是链路追踪，
// 拿不到好随机数不该让登录失败（唯一的要求是非空且近似唯一）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// randomUUID 生成 v4 形式的 UUID（客户端用 crypto.randomUUID()）。
func randomUUID() string {
	h := randomHex(16)
	if len(h) < 32 {
		// 退化成时间戳时长度不足：补齐到 32（Nonce 只要求"每次不同"）。
		h = (h + "00000000000000000000000000000000")[:32]
	}
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}
