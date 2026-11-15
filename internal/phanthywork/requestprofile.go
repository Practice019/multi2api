package phanthywork

import "strings"

// RequestProfile 决定**发往上游的推理请求长什么样**。
//
// # 为什么要有这个东西（2026-11-15 的方向性决策）
//
// 本包原先把「官方 Windows 客户端」的形态当成唯一答案：
// 设备注册 + 180 秒推理令牌 + 12 段 ed25519 签名 + 4 个证明头。
// 那一套在真机上是**能拿到 200 的**（17:07 那次），但账号仍然被反复封禁：
//
//	#1 存活 ~6 分钟   #2 存活 ~90 分钟   #3 存活 26 秒   #4 存活 ~60 分钟
//
// 其中 #4 是最关键的一次：它整整一小时**一次推理都没发过**（只做了
// register + summary + 两次 refresh），第一次推理就直接 403 disabled_account。
// ⇒ **推理不是被封的必要条件**，"复刻官方签名"这条路并没有解决问题。
//
// 与此同时，第三方 CLI `phanthy-code-launcher`（npm，MIT）用一条**完全不同**的路：
//
//	不注册设备、不换推理令牌、不做任何签名
//	只是把 agent 的请求原样转发，把 Authorization 换成真 token
//
// 真源：docs/protocols/pcl-phanthy-code-launcher-spec.md
//
// ⇒ 所以"发什么"这件事必须变成**可切换的**，而不是写死在代码里。
// 这个文件就是那个开关。
//
// # 四种形态
//
//	形态         设备注册  推理令牌  签名  设备指纹  额外身份头        说明
//	────────────────────────────────────────────────────────────────────────────
//	signature      ✅       ✅      ✅     ✅      官方那套         原路，保留以便回退
//	pcl            ❌       ❌      ❌     ✅      launcher 三头    第三方 CLI 的精确形态
//	relay(默认)    ❌       ❌      ❌     ✅      —                最小可用
//	bare           ❌       ❌      ❌     ❌      —                只 Bearer，最裸
//
// **默认是 `relay`** —— 用户 2026-11-15 的决定：
// 「不要用官方 sidecar，而是根据 CLI 去反代」。
//
// # 为什么 `signature` 不删
//
// 它是唯一**在真机上确认拿到过 200** 的形态（17:07 那次）。
// 删掉它就等于把"已知能通"的那条路也一起扔了。留着，可切回。
type RequestProfile string

const (
	// ProfileSignature 官方 Windows 客户端（sidecar 桥）的形态。
	//
	// 设备注册（`/api/oauth/desktop-installations/register`）
	// + 换 180 秒推理令牌（`/api/oauth/activities/summary`）
	// + 12 段 ed25519 canonical 签名
	// + 4 个 `x-phanthy-proof-*` 头
	// + 官方那套身份头（`x-app` / `x-phanthy-client-source: desktop` / `x-phanthy-client-version` / 设备指纹）
	ProfileSignature RequestProfile = "signature"

	// ProfilePCL 第三方 CLI `phanthy-code-launcher` 的精确形态。
	//
	// 只有：`Authorization: Bearer` + `Content-Type` + 它自己的三个基础头
	// （`User-Agent: phanthy-code-launcher/<ver>`、`x-phanthy-client-source: phanthy-code-launcher`、
	// `x-phanthy-product: phanthy_code`）+ `x-phanthy-cli-device`。
	//
	// ⚠ 它会**自称第三方**（见 §5.2 of pcl-phanthy-code-launcher-spec.md）。
	// 选这个形态等于放弃"看起来像官方客户端"。只有在 relay/bare 都测不通时才值得试。
	ProfilePCL RequestProfile = "pcl"

	// ProfileRelay 最小可用形态（**默认**）。
	//
	// `Authorization: Bearer` + `Content-Type` + `Accept` + `x-phanthy-cli-device`。
	// 不注册、不换令牌、不签名。
	ProfileRelay RequestProfile = "relay"

	// ProfileBare 最裸：只有 `Authorization` + `Content-Type` + `Accept`。
	//
	// 连设备指纹都不发。用来回答"设备指纹到底是不是必需的"。
	ProfileBare RequestProfile = "bare"
)

// LauncherUA `ProfilePCL` 用的 User-Agent。
//
// 真源：`M()` in `dist/cli.js`（`phanthy-code-launcher/${L}`，L = 包版本）。
// 这里取我们分析时的版本 0.4.3。
const LauncherUA = "phanthy-code-launcher/0.4.3"

// ParseRequestProfile 把配置值解析成形态。未知值回落 relay（并保持可测）。
func ParseRequestProfile(s string) RequestProfile {
	switch RequestProfile(strings.ToLower(strings.TrimSpace(s))) {
	case ProfileSignature:
		return ProfileSignature
	case ProfilePCL:
		return ProfilePCL
	case ProfileBare:
		return ProfileBare
	case ProfileRelay, "":
		return ProfileRelay
	default:
		// ⚠ 未知值**不静默**成 signature（那会意外打开设备注册与签名），
		// 回落 relay 更安全：它是"什么都不做"的那一档。
		return ProfileRelay
	}
}

// NeedsSignature 该形态是否需要设备注册 / 推理令牌 / 12 段签名。
func (r RequestProfile) NeedsSignature() bool { return r == ProfileSignature }

// SendsDeviceHeader 该形态是否携带 `x-phanthy-cli-device` 设备指纹。
func (r RequestProfile) SendsDeviceHeader() bool {
	return r == ProfileSignature || r == ProfilePCL || r == ProfileRelay
}

// SendsPCLIdentity 该形态是否自称 `phanthy-code-launcher`。
func (r RequestProfile) SendsPCLIdentity() bool { return r == ProfilePCL }

// String 便于日志与判据断言。
func (r RequestProfile) String() string {
	if r == "" {
		return string(ProfileRelay)
	}
	return string(r)
}
