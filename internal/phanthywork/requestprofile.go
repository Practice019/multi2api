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
//	形态          设备注册 推理令牌 签名 设备指纹 额外身份头              说明
//	──────────────────────────────────────────────────────────────────────────────────
//	signature       ✅     ✅    ✅    ✅     官方 Windows 桥那套     原路，保留以便回退
//	kernel(默认)    ❌     ❌    ❌    ✅     product+source=cli      ★ 官方终端 CLI 的形态
//	pcl             ❌     ❌    ❌    ✅     launcher 三头           第三方 CLI 的精确形态
//	relay           ❌     ❌    ❌    ✅     —                      最小可用
//	bare            ❌     ❌    ❌    ❌     —                      只 Bearer，最裸
//
// **默认是 `kernel`** —— 用户 2026-11-15 的决定：
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

	// ProfileRelay 最小可用形态。
	//
	// `Authorization: Bearer` + `Content-Type` + `Accept` + `x-phanthy-cli-device`。
	// 不注册、不换令牌、不签名。
	ProfileRelay RequestProfile = "relay"

	// ProfileKernel 官方**终端 CLI**（`@phanthy/phanthy-codeo`）的形态。★ 默认
	//
	// # 真源：npm 包 `@phanthy/phanthy-codeo@0.2.13` 的 `dist/cli.js`（未压缩，可读）
	//
	// 它是**独立运行的终端 agent** —— 直接打 `https://code.phanthy.com/v1/messages`，
	// **不经过 Windows 那个 sidecar 桥**。而它的源码里：
	//
	//	x-phanthy-proof         0 次
	//	x-phanthy-desktop-session 0 次
	//	desktop-installations   0 次
	//	inference_token         0 次
	//	activities/summary      0 次
	//
	// 也就是说：**官方自己的 CLI 这条路上根本没有签名、没有设备注册、没有推理令牌。**
	// 那三样只存在于 Windows 客户端 → sidecar 桥 这一跳。
	//
	// 它的头由两处拼成（逐字）：
	//
	//	// ① 真实推理头（LLMRequestPrep）
	//	headers: { ...$ ? { [ok.project]: $ } : {},
	//	           [ok.session]: A.sessionID,
	//	           [ok.request]: A.user.id,
	//	           [ok.client]:  A.flags.client,
	//	           ...hsA.headers(),                 // = { [ok.device]: <b64url 设备指纹> }
	//	           "User-Agent": wI4 }               // = phanthy-codeo/<版本>
	//
	//	// ② 请求包装器
	//	u.delete("x-api-key");
	//	u.set("Authorization", `Bearer ${令牌}`);
	//	u.set(ok.product, "phanthy_code");            // X-Phanthy-Product
	//	u.set(ok.clientSource, "cli");                // X-Phanthy-Client-Source: **cli**
	//	u.set("User-Agent", `${AG.name}/${fB}`);      // AG.name="phanthy-codeo", fB="0.2.13"
	//
	// 而头名表就在它自己源码里（`ok`）：
	//
	//	{ client:"x-phanthy-client", clientSource:"X-Phanthy-Client-Source",
	//	  device:"x-phanthy-cli-device", product:"X-Phanthy-Product",
	//	  project:"x-phanthy-project", request:"x-phanthy-request", session:"x-phanthy-session" }
	//
	// # ⚠ 这条同时**推翻了本仓两处旧结论**
	//
	//	旧结论（照 sidecar 桥的判断）              实际（照内核源码）
	//	──────────────────────────────────────────────────────────────
	//	「官方从不发 x-phanthy-product」           **发**（值 phanthy_code，profile/models/包装器都发）
	//	「client-source 应为 desktop」             **终端 CLI 发的是 cli**
	//
	// 错的根因：我们一直在读**桥**，而真正发请求的是**内核**。
	// 第三方 CLI（phanthy-code-launcher）也发 product —— 两个独立来源互相印证。
	ProfileKernel RequestProfile = "kernel"

	// ProfileBare 最裸：只有 `Authorization` + `Content-Type` + `Accept`。
	//
	// 连设备指纹都不发。用来回答"设备指纹到底是不是必需的"。
	ProfileBare RequestProfile = "bare"
)

// KernelUA 官方终端 CLI 的 User-Agent（`AG.name + "/" + fB`，fB 是包版本）。
//
// 真源：`@phanthy/phanthy-codeo@0.2.13` 的 `AG = {name:"phanthy-codeo"}` 与 `fB = "0.2.13"`。
const KernelUA = "phanthy-codeo/0.2.13"

// LauncherUA `ProfilePCL` 用的 User-Agent。
//
// 真源：`M()` in `dist/cli.js`（`phanthy-code-launcher/${L}`，L = 包版本）。
// 这里取我们分析时的版本 0.4.3。
const LauncherUA = "phanthy-code-launcher/0.4.3"

// ParseRequestProfile 把配置值解析成形态。空 / 未知值回落 kernel。
func ParseRequestProfile(s string) RequestProfile {
	switch RequestProfile(strings.ToLower(strings.TrimSpace(s))) {
	case ProfileSignature:
		return ProfileSignature
	case ProfilePCL:
		return ProfilePCL
	case ProfileKernel:
		return ProfileKernel
	case ProfileBare:
		return ProfileBare
	case ProfileRelay:
		return ProfileRelay
	// "" 与未知值都落到 kernel —— 它是**证据最强**的那一档（官方终端 CLI 的精确形态）。
	case "":
		return ProfileKernel
	default:
		// ⚠ 未知值**不静默**成 signature（那会意外打开设备注册与签名），
		// 回落 kernel：不注册不签名，但用官方 CLI 那套头（最像真实客户端）。
		return ProfileKernel
	}
}

// NeedsSignature 该形态是否需要设备注册 / 推理令牌 / 12 段签名。
func (r RequestProfile) NeedsSignature() bool { return r == ProfileSignature }

// SendsDeviceHeader 该形态是否携带 `x-phanthy-cli-device` 设备指纹。
func (r RequestProfile) SendsDeviceHeader() bool {
	return r == ProfileSignature || r == ProfilePCL || r == ProfileRelay || r == ProfileKernel
}

// SendsKernelIdentity 该形态是否照官方终端 CLI 自称（`phanthy-codeo/0.2.13` + product + source=cli）。
func (r RequestProfile) SendsKernelIdentity() bool { return r == ProfileKernel }

// SendsPCLIdentity 该形态是否自称 `phanthy-code-launcher`。
func (r RequestProfile) SendsPCLIdentity() bool { return r == ProfilePCL }

// String 便于日志与判据断言。
func (r RequestProfile) String() string {
	if r == "" {
		return string(ProfileKernel)
	}
	return string(r)
}
