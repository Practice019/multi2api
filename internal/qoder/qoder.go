// qoder.go Qoder（阿里系，国际版 + 中国版）的协议常量与纯函数。
//
// # 事实来源
//
// 协议事实来自参照项目 dsh-codearts-auth（TypeScript，已实测跑通）的
// src/qoder*.ts 与 AGENTS.md。本包是它的 Go 移植，**判据照搬**。
//
// # 这个上游最特别的地方：推理走**加密端点**
//
//	公开：POST api2-v2.qoder.sh/model/v1/chat/completions
//	      认通用名（qwen-flash / qwen-plus），**不认目录 key**
//	加密：POST api2.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation
//	      请求体与签名头由客户端内嵌的 WASM 生成，**认目录 key**（qfmodel / dmodel）
//
// ⚠ `api2.qoder.sh` 与 `api2-v2.qoder.sh` **不是同一个 host**，混用 404。
//
// ⚠ 中国版**没有**可用的公开端点（gateway.qoder.com.cn 与 openapi.qoder.com.cn
// 的 /model/v1/chat/completions 都回 503，alb 无上游路由）。
//
// # 本文件只放"不需要 WASM 就能确定"的部分
//
// 推理的加密/解密在 wasm*.go 里（那是独立的一块）。
// 本文件覆盖：端点常量、产品配置、凭据结构、模型表、错误分类、过期判定。
package qoder

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// providerID 国际版标识。
const providerID = "qoder"

// ProviderID 导出国际版标识，供装配层（cmd/server）使用。
//
// 装配层要把它传给 pool.SyncToDirWithSecrets（账号池按 provider 分域），
// 与其它上游的同名常量同一理由：标识的唯一权威仍是 providerID。
const ProviderID = providerID

// ProviderIDCN 中国版标识。
//
// 与 note 一致：**两个 provider**（同协议族、共用同一份 WASM），
// 差异全部收敛在 Product 配置里。
const ProviderIDCN = "qodercn"

// Product 一个 Qoder 产品（国际版或中国版）的全部差异配置。
//
// 与参照项目的 QoderProduct 对应。
type Product struct {
	// ID provider 标识。
	ID string
	// DisplayName 展示名。
	DisplayName string
	// AuthBase 授权站点。
	AuthBase string
	// OpenAPIBase 设备码轮询 host。
	//
	// ⚠ **不是** AuthBase 的同名路径：实测
	//	openapi.qoder.sh/device/...        正常
	//	qoder.com/同名路径                 返回 401
	OpenAPIBase string
	// InferBase 推理基址（**公开的 OpenAI 兼容端点**）。
	InferBase string
	// EncryptedInferBase 加密推理基址（我们实际用的那条）。
	//
	// ⚠ 与 InferBase 不是同一个 host。
	EncryptedInferBase string
	// ClientID prod 环境的 client_id。
	//
	// ⚠ 实测依据：源码 `client_id: i ? J_a : G_a`，调用点第 4 参传 `isProd()`，
	// 故 prod 用 J_a。把第 4 参误读成 "useIdeClientId" 会让 GitHub 授权页面
	// 报「参数无效」—— 那是真实缺陷（用户报障）。
	ClientID string
	// TestClientID daily/test 环境的 client_id（保留供对照）。
	TestClientID string
	// UserAgentPrefix UA 前缀。
	UserAgentPrefix string
	// SashClientType `/sash/` 端点的客户端类型（官方桌面端身份）。
	SashClientType string
	// SashBase `/sash/` 端点基址。
	SashBase string
}

// Qoder 国际版。
var Qoder = Product{
	ID:                 providerID,
	DisplayName:        "Qoder",
	AuthBase:           "https://qoder.com",
	OpenAPIBase:        "https://openapi.qoder.sh",
	InferBase:          "https://api2-v2.qoder.sh",
	EncryptedInferBase: "https://api2.qoder.sh",
	// ⚠ prod 用这个（源码第 4 参是 isProd()，不是 useIdeClientId）
	ClientID:        "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
	TestClientID:    "e93fe488-5778-4c35-a6fc-0f54ed7b3139",
	UserAgentPrefix: "qoder",
	SashClientType:  "10",
	SashBase:        "https://qoder.com",
}

// QoderCN 中国版。
//
// ⚠ 与国际版的差异（实测，见参照 qoder-product.ts 的 CN 表注释）：
//
//	CN 独有 q37fmodel / gm51model
//	CN 没有 ultimate / performance / efficient / smodel / cmodel
//	  —— 沿用国际版会让菜单出现 5 个 CN 端点根本不认的模型，点了就报错
//	5 条上下文窗口、4 条思考标记、1 条 vl 标记不同
//	mmodel 在 CN 是 MiniMax-M2.7（国际版 M3）
var QoderCN = Product{
	ID:                 ProviderIDCN,
	DisplayName:        "Qoder (中国版)",
	AuthBase:           "https://qoder.com",
	OpenAPIBase:        "https://openapi.qoder.sh",
	InferBase:          "https://api2-v2.qoder.sh",
	EncryptedInferBase: "https://api2.qoder.sh",
	ClientID:           "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
	TestClientID:       "e93fe488-5778-4c35-a6fc-0f54ed7b3139",
	UserAgentPrefix:    "qoder",
	SashClientType:     "10",
	SashBase:           "https://qoder.com",
}

// 端点路径。
const (
	// deviceSelectPath 设备码授权入口（`/device/selectAccounts`）。
	//
	// ⚠ 该路径对**任一** client_id（含全零 UUID）都返回 302 ——
	// 故它**不能**用来校验 client_id 是否正确。
	// client_id 的错误要到**授权回调阶段**才被服务端校验出来
	//（真实缺陷：prod 误用测试 client_id 时，GitHub 授权点击后
	// 页面报「参数无效 / 你可以稍后前往 IDE 客户端并登录Qoder」）。
	deviceSelectPath = "/device/selectAccounts"
	// devicePollPath 设备码轮询。
	//
	// ⚠ **404 表示「用户尚未完成授权」，必须继续轮询，不是错误**。
	// 实测依据：该端点返回 404 而任意不存在的路径返回 401 ——
	// 说明它被网关豁免认证、由业务层报「会话未就绪」。
	// 轮询 host 是 openapi.qoder.sh（qoder.com 的同名路径返回 401）。
	devicePollPath = "/api/v1/deviceToken/poll"
	// refreshPath 续期。
	//
	// ⚠ 请求体**必须带 machine_id**（与多数 OAuth 实现不同）。
	refreshPath = "/api/v1/deviceToken/refresh"
	// userInfoPath 用户信息。
	userInfoPath = "/api/v1/userinfo"
	// publicInferPath 公开推理端点（**不认目录 key**）。
	publicInferPath = "/model/v1/chat/completions"
	// encryptedInferPath 加密推理端点（我们实际用的那条）。
	//
	// ⚠ 请求体必须带 business 字段（`business:{type:"agent"}`）——
	// 缺了服务端会把请求路由到**故障节点** `oa_qwen-plus-2025-04-28`
	// 并返回 `[FAIL]node:... msg:Execution failed`。
	// 这是真实缺陷（2026-09-20 定位，极隐蔽）：qfmodel 因此"看起来不可用"，
	// 而同一模型在 Qoder IDE 里完全正常。
	//
	// ⚠ 判据是「IDE 能否用同一模型」：IDE 能用 → 是我们的请求缺东西，
	// 不是服务端故障。
	encryptedInferPath = "/algo/api/v2/service/pro/sse/agent_chat_generation"
	// modelListPath 模型列表（需 WASM 签名，我们**不发**这个请求）。
	modelListPath = "/algo/api/v2/model/list"
)

// 超时与轮询。
const (
	requestTimeoutMS = 30_000
	// loginTimeoutMS 登录流程整体超时。
	loginTimeoutMS = 300_000
	// pollIntervalMS 设备码轮询间隔。
	pollIntervalMS = 1_000
	// pollMaxFailures 连续网络失败上限。
	pollMaxFailures = 5
)

// clientMetadata 设备码授权请求的客户端元数据。
var clientMetadata = map[string]string{
	"client_type":      "5",
	"business_product": "cli",
	"business_type":    "agent",
	"scene":            "assistant",
}

// Auth 一份 Qoder 凭证。
type Auth struct {
	// AccessToken 访问令牌。
	AccessToken string `json:"access_token"`
	// RefreshToken 续期令牌。
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresIn 相对过期秒数。
	ExpiresIn int64 `json:"expires_in,omitempty"`
	// UID 账号主键。
	UID string `json:"uid,omitempty"`
	// Nickname 展示名。
	Nickname string `json:"nickname,omitempty"`
	// MachineID 机器标识。
	//
	// ⚠ **续期请求体必须带它**（与国际上多数 OAuth 实现不同）。
	MachineID string `json:"machine_id,omitempty"`
	// DeviceID 设备 id（`pc_` 前缀那类）。
	DeviceID string `json:"device_id,omitempty"`
	// ProductID 这份凭证属于哪个产品（qoder / qodercn）。
	//
	// ⚠ 必须持久化：同一个账号池里两个产品并存，
	// 起客户端时要知道该用哪份配置。
	ProductID string `json:"product_id,omitempty"`
	// FilePath 凭证文件落点。
	FilePath string `json:"-"`
}

// UIDValue 账号主键。
func (a *Auth) UIDValue() string {
	if a == nil {
		return ""
	}
	if strings.TrimSpace(a.UID) != "" {
		return strings.TrimSpace(a.UID)
	}
	if a.MachineID != "" {
		return "qoder-" + a.MachineID
	}
	if a.AccessToken != "" {
		sum := sha256.Sum256([]byte(a.AccessToken))
		return "qoder-" + hex.EncodeToString(sum[:8])
	}
	return ""
}

// Renewable 报告是否可以续期。
func (a *Auth) Renewable() bool {
	return a != nil && strings.TrimSpace(a.RefreshToken) != ""
}

// ExpiresAtMS 过期时刻（毫秒）。
//
// 顺序：JWT exp → 无（返回 0）。
//
// ⚠ 与某些上游不同，Qoder 的凭证里没有绝对过期字段，只能读 JWT。
func (a *Auth) ExpiresAtMS() int64 {
	if a == nil {
		return 0
	}
	return jwtExpMS(a.AccessToken)
}

// IsExpired 报告是否过期。
//
// ⚠ 无过期信息时**不**判过期（保守）：交给服务端 401/业务码判定。
func (a *Auth) IsExpired(now time.Time) bool {
	ms := a.ExpiresAtMS()
	if ms <= 0 {
		return false
	}
	return ms <= now.UnixMilli()
}

// jwtExpMS 从 JWT payload 取 exp（毫秒）；取不到返回 0。只解码不验签。
func jwtExpMS(token string) int64 {
	if token == "" {
		return 0
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return 0
		}
	}
	var payload struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}
	if payload.Exp <= 0 || payload.Exp != payload.Exp {
		return 0
	}
	return int64(payload.Exp * 1000)
}

// ProductByID 按产品 id 取配置。
func ProductByID(id string) (Product, bool) {
	switch id {
	case providerID:
		return Qoder, true
	case ProviderIDCN:
		return QoderCN, true
	}
	return Product{}, false
}

// DerivedUID 由 token 派生 uid（无 uid 字段时的兜底）。
func DerivedUID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "qoder-" + hex.EncodeToString(sum[:8])
}

// shortUID 日志用的短 uid。
func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8] + "…"
}

var _ = fmt.Sprintf
