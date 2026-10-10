// minimax.go MiniMax Code（中国版）上游的常量与纯辅助。
//
// # 这个上游与其它上游最大的不同
//
// 它是本仓**第二个 Anthropic Messages 协议族**的上游（第一个是 zcode 的
// JWT 通道）。两条事实决定了它的形状：
//
//	① 推理请求/响应是 **Anthropic 形状**，与网关对外的 OpenAI 形状不同
//	   → 走共享的 `internal/anthroconv` 转换层
//	② 认证只要一个 `Authorization: Bearer`，**没有签名、没有机器指纹**
//	   （实测签到/积分/目录/推理都如此）—— 比 qoder / trae 那类简单得多
//
// # 协议事实的来源
//
// 全部来自开源参照项目（`deepseek-harness-codearts` 的 `src/minimax*.ts`，
// 逐行提取，见每处的行号引用）。**不是**自己逆出来的 —— 所以那些
// "不能凭直觉改"的点都有源码依据，而不是传闻。
//
// ⚠ 本文件刻意只放**常量与纯函数**（无 IO、无状态）：便于单测直接钉住
// 那些"看起来可以简化、实际是上游事实"的判据。
package minimax

import (
	"encoding/json"
	"strconv"
	"strings"
)

// 主机。
//
// ⚠ **两个不同的 host，不能合并**：
//
//	accountHost  OAuth（设备码 / 轮询 / 续期）
//	apiHost      业务（模型目录 / 签到 / 积分）+ 推理
//
// 把 OAuth 打到 apiHost（或反之）会得到一个 404 —— 而 404 在本仓
// 通常表示"账号不存在"，两者混淆会让排障方向完全错。
const (
	accountHost = "https://account.minimax.cn"
	apiHost     = "https://agent.minimax.cn"

	// 参照项目里 host 是**硬编码常量**，没有任何环境变量可以覆盖基址
	// （全仓 `process.env.MINIMAX*` 只存在于 e2e 闸门）。
	// 本仓仍然把两个 host 做成可注入（`Config.AccountBase` / `Config.APIBase`），
	// 理由与 zcode 的 OAuthBase 相同：测试要能把假上游指进来，
	// 否则"登录流程"与"额度查询"这两条路径**无法被断言**。
)

// OAuth 端点（accountHost 下）。
const (
	pathDeviceCode = "/oauth2/device/code"
	pathToken      = "/oauth2/token"
)

// 业务端点（apiHost 下）。
const (
	pathModels        = "/mavis/api/v1/models"
	pathInferMessages = "/mavis/api/v1/llm/v1/messages"
	pathSigninStatus  = "/minimax-cloud/api/v1/signin/status"
	pathSigninClaim   = "/minimax-cloud/api/v1/signin/claim"
	pathCreditDetails = "/minimax-cloud/api/v1/credit/details"
)

// OAuth 产品常量（照抄参照项目 `MINIMAX`）。
const (
	oauthClientID = "mcode-public"
	oauthAudience = "agent-backend"
	oauthScope    = "agent.default"

	// deviceCodeGrant 设备码轮询的 grant_type（RFC 8628 的标准 URN）。
	deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"
	refreshGrant    = "refresh_token"
)

// 目录查询参数（**query**，不是头）。
//
// ⚠ 参照项目明确记过：region / buildEnv 是 query 参数，
// 而且**没有任何** UA / 版本 / region 头。放在头上是凭空猜测。
const (
	queryRegion   = "cn"
	queryBuildEnv = "prod"
)

// 签到面板常量（照抄参照项目 `MINIMAX_SIGNIN_STATUS` / `MINIMAX_CLAIM_RESULT`）。
const (
	signinStatusUpcoming  = 1
	signinStatusClaimable = 2
	signinStatusClaimed   = 3
	signinStatusDisabled  = 4

	// claimResultClaimed / claimResultAlreadyClaimed 签到领取的业务码。
	//
	// ⚠ **幂等判据是它，不是 HTTP 状态码** —— 重复领取同样返回 200。
	// 只看 HTTP 会虚报成功（用户以为 +了积分，实际 +0）。
	claimResultClaimed        = 1
	claimResultAlreadyClaimed = 2
)

// 思考档位模式（远端 `thinking_config.mode` 的三种取值）。
//
// ⚠ 三者语义**完全不同**，混用会让"关掉思考"变成空转或硬报错：
//
//	forcedOn   思考强制开启。传 disabled 被**静默忽略**（M2.7）
//	           或被**硬拒 400**（M3.1，报 `requires adaptive thinking`）
//	switchable 可以开关。不传 = 不思考；`adaptive` = 有思考（M3）
const (
	thinkingModeForcedOn   = "forced_on"
	thinkingModeSwitchable = "switchable"
)

// requiresAdaptiveThinking 该模型是否**必须**发 `thinking:{type:"adaptive"}`。
//
// ⚠ 判据是**模型名前缀** `MiniMax-M3.1`（照抄参照项目
// `minimax-adapter.ts` 的 `requiresAdaptiveThinking`）。
//
// 为什么不能只看 `effort_options`：M3.1 **即使不选档位**也必须发 adaptive
// —— 发 `disabled` 会被服务端硬拒：
//
//	invalid params, model "MiniMax-M3.1-Flash-Preview" requires adaptive
//	thinking; thinking.type="disabled" (including reasoning.effort=none)
//	is not allowed (2013)
//
// 用前缀而不是等值比较：将来出现 `MiniMax-M3.1-x` 的兄弟模型时
// 它同样要求 adaptive（参照项目也是前缀判据）。
func requiresAdaptiveThinking(model string) bool {
	return strings.HasPrefix(strings.TrimSpace(model), "MiniMax-M3.1")
}

// isSwitchableThinking 该模型的思考是否可开关（远端 `thinking_config.mode`）。
func isSwitchableThinking(mode string) bool {
	return strings.TrimSpace(mode) == thinkingModeSwitchable
}

// strictInt 取**严格**整数：只认 JSON 数字，**不认数字字符串**。
//
// # 为什么需要它（与 toFloat 的分工）
//
// `toFloat` 刻意容忍数字字符串 —— 那是给**金额字段**用的
// （`remaining_amount` 实测就是 `"800.00"`）。
//
// 但**判据类字段**不能容忍：
//
//	claim_result:"1"（字符串）
//
// 若用 toFloat 读它就会得到 1 ⇒ 虚报「签到成功」，而参照项目明确要求
// 这种情况判 **failed**（原话：「缺失/null/越界/字符串时一律判 failed」）。
//
// 区分「金额」与「判据」是必要的：前者宽容能少踩上游的类型漂移，
// 后者宽容会让**失败被伪装成成功**。
func strictInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		// JSON 数字解出来就是 float64。非整数（如 1.5）也算不合法。
		if x != float64(int(x)) {
			return 0, false
		}
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

// positiveInt 取正整数字面量；否则 (0,false)。
//
// ⚠ `0` 在本包一律是**「未知/缺失」哨兵**，不是合法值
// （照抄参照项目 `minimax.ts` 的注释：调用方必须判 `> 0` 才声明）。
// 所以这里与别处一样："读不到"绝不编造 0 —— 但要显式返回 ok=false，
// 让调用方自己决定回落（本包里窗口是 `context_window_options` 最大值
// → 回退 `limit.context`，那是两条**有据**的回落，不是编造）。
func positiveInt(v any) (int, bool) {
	n, ok := toFloat(v)
	if !ok || n <= 0 {
		return 0, false
	}
	return int(n), true
}

// toFloat 把 JSON 数字（或数字字符串）转成 float64。
//
// 容忍字符串：参照项目实测余额的 `remaining_amount` 是 `"800.00"`（字符串），
// 而同一批响应里 `total_count` 是裸数字 —— 上游同一份 JSON 里两种混用。
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f, true
		}
	case string:
		// ⚠ 空串**先挡掉**：`strconv.ParseFloat("")` 会报错，
		// 但别处若用 `Number('')` 语义会得到 0 —— 那会把"缺字段"
		// 误读成"0 积分"（本仓铁律：不编造 0）。
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// looseAmount 宽容地取一个「金额」（数字或数字字符串）。
//
// ⚠ 与 `toFloat` 的差别在**语义**：这个返回指针，nil 表示"没读到"，
// 调用方必须显式处理 nil（而不是拿到一个默认为 0 的 float64 直接相加）。
// 参照项目的 `looseAmount` 就是这个形状，注释写明理由：
// 「空串/非有限 ⇒ undefined（不编造 0）」。
func looseAmount(v any) *float64 {
	if f, ok := toFloat(v); ok {
		return &f
	}
	return nil
}

// roundCredits 把积分保留两位（照抄参照项目 `roundCredits`）。
//
// 上游的余额是 `"800.00"` 这种字符串，浮点求和后可能出现
// `799.9999999999999`；界面上显示一串 9 会被当成计算错误。
func roundCredits(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// unwrapEnvelopeData 解包 `data` 层（最多一层）。
//
// # 为什么必须兼容"平铺"与"信封"两种形状
//
// 参照项目实测：三个 `/minimax-cloud/api/v1/*` 端点**形状不一致**：
//
//	signin/status / signin/claim  业务字段在 `data` 下（信封）
//	credit/details                `total_count` 与 `base_resp` **与顶层同级**，
//	                              **没有** `data` 键（平铺）
//
// 而这两个端点的响应要经过**同一个**解包函数。硬编码"必须有 data"
// 会把"余额为 0"报成"查询失败" —— 这是参照项目踩过的原话。
func unwrapEnvelopeData(v map[string]any) map[string]any {
	if v == nil {
		return nil
	}
	if d, ok := v["data"].(map[string]any); ok {
		return d
	}
	return v
}

// baseRespStatus 读业务码 `base_resp.status_code`。
//
// ⚠ 三点：
//
//  1. 字段名是 **`base_resp.status_code`**，**不是** `code` ——
//     本仓其它上游（zcode 等）用的是 `code`，照抄会静默读不到。
//  2. 它**只**服务那 3 个 `/minimax-cloud/api/v1/*` 端点。目录端点只看
//     HTTP 状态码，推理端点看 HTTP + SSE 的 error 帧 —— 混用会误判。
//  3. 返回 (code, present)。调用方必须区分「码是 0」与「压根没有 base_resp」：
//     参照项目的实现里 `base_resp` 缺失 ⇒ 走"成功"分支，也就是一个
//     HTTP 500 的 JSON 响应若没有 `base_resp` 会被判成成功。
//     **本包不照抄这个行为**：缺失时按"未知"处理，由调用方决定
//     （额度那条会更保守：宁愿报"查不到"也不编造 0）。
func baseRespStatus(v map[string]any) (int, bool) {
	if v == nil {
		return 0, false
	}
	br, ok := v["base_resp"].(map[string]any)
	if !ok {
		return 0, false
	}
	n, ok := positiveInt(br["status_code"])
	if ok {
		return n, true
	}
	// status_code = 0 是正常值，但 positiveInt 把它判为"非正" —— 这里补一次。
	if f, ok2 := toFloat(br["status_code"]); ok2 && f == 0 {
		return 0, true
	}
	return 0, false
}

// baseRespMessage 读 `base_resp.status_msg`（错误说明）。
func baseRespMessage(v map[string]any) string {
	if v == nil {
		return ""
	}
	br, ok := v["base_resp"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := br["status_msg"].(string)
	return strings.TrimSpace(s)
}
