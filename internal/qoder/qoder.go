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
//
// # ⚠ 中国版是**另一个服务**，不是"同一端点的另一张模型表"
//
// 这一点我第一版移植时判断错了：看到 CN「没有可用的公开端点」，
// 就顺手把国际版的四个端点与 clientId 全抄了一遍，只改了 ID 与显示名。
// 参照的 CN 定义是**独立的一份配置**（qoder-product.ts:433-482）：
//
//	authBase            qoder.cn                  （国际版 qoder.com）
//	openApiBase         openapi.qoder.com.cn      （国际版 openapi.qoder.sh）
//	encryptedInferBase  gateway.qoder.com.cn      （国际版 api2.qoder.sh）
//	clientId            732aef47-9cf2-46a2-95fe-4cebb5d0d1fa
//	                    （国际版 e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb）
//
// 参照对 clientId 的措辞最要紧：「**与国际版完全不同** —— 国际版两个 id
// 在 CN asar 里命中 0 次」，且
//
//	用错的症状是「授权页 302 正常、点击授权后报参数无效」，
//	故**不能**靠探测入口验证，必须真实登录闭环
//
// 也就是说 clientId 写错**不会**在任何探测里暴露 —— 授权页照常打开，
// 只有用户真的点了授权才失败。所以这四个值必须逐字照抄，不能"看起来
// 差不多就复用"。
//
// ⚠ InferBase 在两边都**没有调用方**（公开端点方案早已被加密端点取代）。
// 参照把 CN 的 inferBase 填成与 encryptedInferBase 同值，仅表示
//「没有独立公开端点」，不要据此发请求 —— 实测 gateway.qoder.com.cn
// 的公开路径回 503（alb 无上游路由）。
var QoderCN = Product{
	ID:                 ProviderIDCN,
	DisplayName:        "Qoder (中国版)",
	AuthBase:           "https://qoder.cn",
	OpenAPIBase:        "https://openapi.qoder.com.cn",
	// ⚠ 与 EncryptedInferBase 同值只表示"没有独立公开端点"，见上。
	InferBase:          "https://gateway.qoder.com.cn",
	EncryptedInferBase: "https://gateway.qoder.com.cn",
	// ⚠ 与国际版**完全不同**，且写错只在"点击授权后"才暴露。
	ClientID: "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
	// ⚠ CN 的 test 与 prod 是**同一个值**（国际版那两个不同）。
	// 故不存在国际版"J_a / G_a 被读反"那类风险。
	TestClientID:    "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
	UserAgentPrefix: "qoder",
	// E10：CN asar 里同样是 `Fh = Object.freeze({ clientType: 10, … })`。
	SashClientType: "10",
	// sash 系列的基址随产品走（CN 是 qoder.cn）。
	SashBase: "https://qoder.cn",
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
	// ExpiresAt 访问令牌的**绝对**过期时刻（毫秒时间戳）。
	//
	// # ⚠ 为什么是绝对时刻而不是旧的 `expires_in`（相对秒）
	//
	// 上游续期响应给的是 `expires_at`（ISO 字符串，实测
	// `"2026-10-30T06:56:55Z"`），**不是** `expires_in`。旧实现存相对秒
	// 有两层问题：字段名对不上（永远存不进来），且相对值一离开响应就
	// 没有参照点 —— 存下来再读只能得到"签发那一刻剩余多久"。
	//
	// ⚠ 本字段是「Token」列与"要不要续期"的**唯一**权威：
	// qoder 的 access_token 是 `dt-` 前缀的**不透明串**（27 字符，不是 JWT），
	// 所以"解 JWT exp"那条路走不通（旧实现只走那条 → 界面恒显示 `—`）。
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// RefreshTokenExpiresAt refresh_token 的绝对过期时刻（毫秒）。
	//
	// 用来区分"access 过期（可自动续）"与"refresh 也过期（只能重登）"。
	RefreshTokenExpiresAt int64 `json:"refresh_token_expires_at,omitempty"`
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
// # 顺序（本轮修正）
//
//	① Auth.ExpiresAt（落盘字段，来自续期响应的 `expires_at`）
//	② access_token 的 JWT `exp`（回落：手工导入的 JWT 凭据没有落盘字段）
//	③ 0 = 未知
//
// ⚠ 旧实现**只**走 ② —— 而 qodercn 的 access_token 是 `dt-` 前缀的
// 不透明串（27 字符，不是 JWT），于是恒返回 0，界面「Token」列恒显示 `—`
// （用户报障："没有 Token"）。落盘字段才是主路径，JWT 只是兜底。
//
// ⚠ 返回 0 的含义是"**不知道**"，不是"1970 年过期"——
// 调用方（CredentialExpiryExt）据此让界面显示 `—` 而不是"已过期"。
func (a *Auth) ExpiresAtMS() int64 {
	if a == nil {
		return 0
	}
	if a.ExpiresAt > 0 {
		return a.ExpiresAt
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
