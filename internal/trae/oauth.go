// oauth.go TRAE 浏览器 OAuth 登录的 URL 构造与回调解析。
//
// # 流程（复现自 traework2api 的 login.sh，实测协议）
//
//  1. 生成 machine_id / device_id（hex32）与 login_trace_id（hex16）
//  2. 构造登录 URL（https://www.trae.cn/authorization?...，回调指向
//     127.0.0.1:18080/authorize）→ 用户在浏览器登录
//  3. 浏览器跳到 127.0.0.1（打不开）→ 用户复制地址栏完整回调链接
//  4. 解析回调 query：refreshToken / userInfo(URL 编码 JSON) / userJwt(URL 编码 JSON)
//  5. refreshToken → ExchangeToken 换 accessToken；无 refreshToken 时
//     用 userJwt.Token 兜底（此时无法自动续期）
//  6. GetUserInfo 拿 uid / nickname / enterpriseID → 落盘
package trae

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// ConsoleHost 登录页站点。
const ConsoleHost = "https://www.trae.cn"

// loginCallbackPort 回调端口（与 traework2api login.sh 一致：18080）。
// 回调地址只是"浏览器跳到打不开的 127.0.0.1"时的落点 —— 不需要真监听，
// 因为流程是"用户复制回调链接粘贴回来"，不是回调服务器。
const loginCallbackPort = "18080"

// loginParams 登录 URL 的固定参数（实测自 traework2api login.sh）。
var loginParams = map[string]string{
	"login_version":     "1",
	"auth_from":         "solo",
	"login_channel":     "native_ide",
	"plugin_version":    "2.3.62834",
	"auth_type":         "local",
	"client_id":         ClientID,
	"redirect":          "0",
	"auth_callback_url": "http://127.0.0.1:" + loginCallbackPort + "/authorize",
	"x_device_brand":    "PC",
	"x_device_type":     "PC",
	"x_os_version":      "1.0",
	"x_app_version":     IdeVersion,
	"x_app_type":        "stable",
}

// BuildLoginURL 构造登录链接。
//
// machineID / deviceID 为 32 位 hex（同一对 id 要同时用于登录 URL 与落盘凭证，
// 前后一致 —— 上游以此识别设备）；traceID 为 16 位 hex（链路追踪，每次新生成）。
func BuildLoginURL(machineID, deviceID, traceID string) string {
	q := make(url.Values, len(loginParams)+6)
	for k, v := range loginParams {
		q.Set(k, v)
	}
	q.Set("login_trace_id", traceID)
	q.Set("machine_id", machineID)
	q.Set("device_id", deviceID)
	q.Set("x_device_id", deviceID)
	q.Set("x_machine_id", machineID)
	return ConsoleHost + "/authorization?" + q.Encode()
}

// CallbackInfo 回调链接解析出的登录信息。
type CallbackInfo struct {
	RefreshToken  string
	Token         string // userJwt.Token（无 refreshToken 时的兜底 accessToken）
	UserID        string
	ScreenName    string
	TenantID      string
	TokenExpireAt int64
	// ClientID 签发这份 refreshToken 的 OAuth client id（ExchangeToken 必须用它）。
	// 来源：userJwt.ClientID / 顶层 clientID / client_id（兼容三种命名）。
	ClientID string
}

// ParseCallback 解析回调链接（parse_qs + unquote 处理 URL 编码，与 login.sh 一致）。
//
// 三种容错：
//
//   - refreshToken 缺失 → 用 userJwt.RefreshToken 顶上
//   - userInfo / userJwt 是 URL 编码的 JSON → 先原样解，再 unquote 一层解
//   - 完全解不出 → 返回错误（回调用错/不完整）
func ParseCallback(callback string) (*CallbackInfo, error) {
	u, err := url.Parse(strings.TrimSpace(callback))
	if err != nil {
		return nil, fmt.Errorf("trae: 回调链接不是合法 URL: %w", err)
	}
	q := u.Query()
	info := &CallbackInfo{
		RefreshToken: q.Get("refreshToken"),
	}
	userInfo := decodeJSONParam(q.Get("userInfo"))
	userJWT := decodeJSONParam(q.Get("userJwt"))

	if v, _ := userInfo["UserID"].(string); v != "" {
		info.UserID = v
	}
	if v, _ := userInfo["ScreenName"].(string); v != "" {
		info.ScreenName = v
	}
	if v, _ := userInfo["TenantID"].(string); v != "" {
		info.TenantID = v
	}
	if v, _ := userJWT["Token"].(string); v != "" {
		info.Token = v
	}
	if v, _ := userJWT["RefreshToken"].(string); v != "" {
		if info.RefreshToken == "" {
			info.RefreshToken = v
		}
	}
	if v, _ := userJWT["TokenExpireAt"].(float64); v > 0 {
		info.TokenExpireAt = int64(v)
	}
	if v, _ := userJWT["ClientID"].(string); v != "" {
		info.ClientID = v
	}
	if info.ClientID == "" {
		info.ClientID = q.Get("clientID")
	}
	if info.ClientID == "" {
		info.ClientID = q.Get("client_id")
	}

	if info.RefreshToken == "" && info.Token == "" {
		return nil, fmt.Errorf("trae: 回调链接缺少 refreshToken 与 userJwt.Token —— " +
			"请确认粘贴的是登录成功后地址栏的完整链接")
	}
	return info, nil
}

// decodeJSONParam 解 URL 编码的 JSON 参数；失败返回空 map。
func decodeJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	// parse_qs 已解一层，再容错解一层 unquote（login.sh 的 parse_json_param 同款）。
	for _, cand := range []string{raw, mustUnescape(raw)} {
		if cand == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(cand), &obj); err == nil {
			return obj
		}
	}
	return nil
}

// mustUnescape 尽力再解一层 URL 编码；失败原样返回。
func mustUnescape(s string) string {
	if out, err := url.QueryUnescape(s); err == nil {
		return out
	}
	return s
}

// normalizeExpiresAtMilli 毫秒/秒归一化（登录回调的 TokenExpireAt 也可能是毫秒）。
func normalizeExpiresAtMilli(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}
