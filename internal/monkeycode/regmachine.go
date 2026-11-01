// regmachine.go 认「注册机（register-machine）导出的 JSON」。
//
// # 用户要打通的链路
//
//	注册机选中账号 → 导出 accounts-export-*.jsonl
//	→ 反代（本仓）「批量导入」按钮 → 账号进池 → 能对话
//
// # 为什么不能直接喂给 parseAuth（实测踩到）
//
// 注册机的字段名与类型都与本仓不同，而 Go 的 json.Unmarshal
// 遇到**一个类型不符**就整条失败 —— 于是 session 明明在里面，
// 却报「凭证文件里没有可用的 session」：
//
//	注册机                本仓 Auth            问题
//	──────────────────────────────────────────────────────
//	session               session             ✓ 同名
//	region                region              ✓ 同名
//	plan                  plan                ✓ 同名
//	userId                userid              ✗ 驼峰 vs 全小写
//	name                  nickname            ✗ 字段名不同
//	balance   (number)    balance   (string)  ✗ **类型冲突 → 整条 Unmarshal 失败**
//	dailyTokenBalance     daily_token         ✗
//	dailyTokenLimit       daily_token_limit   ✗
//
// 最后一条是全或无的：只要 balance 是数字，`json.Unmarshal` 就返回 error，
// 连带 session 也读不到。所以**必须在解析前先认出注册机格式并翻译**。
//
// # 必需字段只有 5 个（实测穷举）
//
// 用户问过：「注册机导出的 json 文件内容还是很多，你保证这些都是必须的字段么」。
//
// 诚实的回答是：**不，其中 25 个本仓根本不读**。逐字段验证后的结论：
//
//	必需（缺了就不工作）
//	  session     唯一判据 Usable()
//	  region      决定基址/模型目录/是否走代理，错了整天 404 或 401
//	  platform    混合导入时用来跳过别的上游的行（单源导入可省）
//
//	强烈建议（缺了能跑，但难用）
//	  userId      账号池主键；缺了回退 session-<hash>，删号/重导对不上
//	  name        展示名；缺了显示 session-xxxx，看不出是哪个号
//
//	可选（仅展示，第一轮额度刷新就覆盖）
//	  plan / balance / dailyTokenBalance / dailyTokenLimit
//
//	无用（本仓完全不读）
//	  version baseUrl imageId imageIdNote subscription role status isBlocked
//	  avatarUrl teamCount modelsOk modelsCount modelsError addedAt
//	  expiresAt createdAt checkedIn checkinAt walletAt checkinCheckedAt
//	  hasPassword securityAt passwordAt
//
//	★ 危险
//	  expiresAt   注册机按 30 天算的；本仓按实际更短的过期处理，抄进来是假值
//
//	★ 2026-11-01 起从"危险/无用"改成**要读**的：
//	  email       续期必需（与 password 配对喂 password-login，见 renew.go）
//	  password    同上；它是**唯一的自救钥匙** —— session 完全失效时靠它换回新 session
//
// 所以注册机侧配了 config.output.exportFields.monkeycode，只导上面那 9 个：
// 35 字段 / 1100 字节 → 9 字段 / 253 字节（减 77%）。
// 而本仓**两种都能收**：精简格式与全量格式都实测导入成功 ——
// 精简是为了省体积与去掉明文密码，不是为了"必须精简才能导入"。
//
// # ★ 2026-11-01 口径变更：当初为什么丢 password、为什么现在又要
//
// 当初的判断（保留，不要抹掉——它是当时条件下的正解）：
//
//	「明文密码。导进本仓毫无用处，只是泄露面」
//
// 那时本仓只有 session 一条路（密码只能换来一个和 session 等价的东西），
// 所以把密码收进来 = 纯增泄露面、零收益，主动裁掉是对的。
//
// 现在变了：本仓**能自动续期**了（见 renew.go 与 credential.go 的说明）。
// session 一旦失效，密码是**唯一**能自救的材料（password-login 不需要有效 session）。
// 不收它 ⇒ 每 30 天必须人工重新登录导入一次，用户报的正是这个痛点。
//
// 代价（必须接受并约束）：明文密码多落一份盘。边界见 credential.go 的
// 「代价与边界」：绝不进日志、绝不进 admin 响应、只活在 0600 的凭据档里。
// 用户已明确选择"能自动续期优先"（2026-11-01 裁决）。
//
// # 为什么放在上游侧而不是核心
//
// 「注册机的 JSON 长什么样」是关于 monkeycode 这个上游的外部事实，
// 与 gateway 契约无关。放上游侧，核心的导入逻辑一行不用改；
// 将来注册机改字段，也只改这一个文件。
package monkeycode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// regMachineDoc 注册机导出的单行（只取我们要的字段）。
//
// 用 json.Number 而不是 int64/float64 接数值：注册机那边 balance 是数字，
// 但**同一份文件里可能混着字符串形态**（它早期版本存过 string）。
// json.Number 两种都能接住，不会因为类型细节再整条失败。
type regMachineDoc struct {
	Platform string `json:"platform"`
	Session  string `json:"session"`
	Region   string `json:"region"`
	UserID   string `json:"userId"`
	UserID2  string `json:"userid"`
	Name     string `json:"name"`
	Plan     string `json:"plan"`
	Balance  string `json:"-"`

	// Email / Password 续期材料（2026-11-01 起保留）。
	//
	// 历史：本仓曾把它们归入「危险/无用」并**主动不收**（那时没有续期路径，
	// 收了只是纯增泄露面）。现在本仓能自动续期（见 renew.go），
	// 而 session 失效时密码是**唯一**能自救的材料 —— 所以改成收。
	//
	// ⚠ 这两个值**绝不进日志**（见 credential.go 的「代价与边界」）。
	Email    string `json:"email"`
	Password string `json:"password"`

	// RenewedAt 注册机那边记的"上次续期时刻"。
	//
	// ⚠ 用 **json.RawMessage** 而不是 string / time.Time：注册机写的是
	// 毫秒时间戳**数字**（1793545574222）。解进 string 会报类型错，
	// 解进 time.Time 也会报类型错 —— 而两者都是**整条 Unmarshal 失败**，
	// 现象是「凭证文件里没有可用的 session」（同 balance 那个坑）。
	// 拿原文后自己走 ParseAuthTime 容错解析。
	RenewedAt json.RawMessage `json:"renewedAt"`

	// 数值字段先按 RawMessage 接，再自己转 —— 见 regMachineBalance 的说明。
	RawBalance  json.RawMessage `json:"balance"`
	RawDailyBal json.RawMessage `json:"dailyTokenBalance"`
	RawDailyLim json.RawMessage `json:"dailyTokenLimit"`
}

// isRegMachineDoc 判断这段 JSON 是不是注册机导出的。
//
// 判据只认**结构特征**，不认平台名以外的东西：
//
//	① platform == "monkeycode"（注册机自己写的）
//	② 或同时有 session + (userId|userid) + name
//
// 第②条是给"用户手动删掉了 platform 字段"留的后路 —— 那种文件仍然可用。
func isRegMachineDoc(raw []byte) bool {
	var probe struct {
		Platform string `json:"platform"`
		Session  string `json:"session"`
		UserID   string `json:"userId"`
		UserID2  string `json:"userid"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(probe.Platform), providerID) {
		return true
	}
	return probe.Session != "" && probe.Name != "" && (probe.UserID != "" || probe.UserID2 != "")
}

// fromRegMachine 把注册机的一条记录翻译成 *Auth。
func fromRegMachine(raw []byte) (*Auth, error) {
	var d regMachineDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("monkeycode: 解析注册机记录失败：%w", err)
	}
	if strings.TrimSpace(d.Session) == "" {
		return nil, fmt.Errorf("monkeycode: 注册机记录里没有 session")
	}

	a := &Auth{
		Session: strings.TrimSpace(d.Session),
		Region:  normalizeRegion(d.Region),
		Plan:    strings.TrimSpace(d.Plan),
		// 注册机的 name 是本仓的 nickname（两边都表示"展示名"）。
		Nickname: strings.TrimSpace(d.Name),
	}
	if d.UserID != "" {
		a.UserID = strings.TrimSpace(d.UserID)
	} else {
		a.UserID = strings.TrimSpace(d.UserID2)
	}
	// ★ 2026-11-02：缺 userId ⇒ **只警告，不拒绝**。
	//
	// # 为什么这里不能"宁可报错"
	//
	// 本仓有一条**既有契约**（`regmachine_test.go` 的 TestImportMinimalRegMachineRow，
	// 起因是用户的明确要求）：注册机导出可以精简到只剩
	// `{platform, session, region}` 三个字段，**必须照样导得进来** ——
	// 「用户哪天手删字段就导不进去了」正是那条测试要挡的事。
	// 所以"缺 userId 就报错"会直接推翻那条契约，不能这么改。
	//
	// # 那"孤儿文件"靠什么治：`SaveFile` 的迁移（B）
	//
	// 缺 userId 本身**不是 bug** —— 它的后果是 `UID()` 回退成
	// `"session-"+shortHash(Session)`，于是文件名是 `monkeycode-session-<hash>.json`。
	// 真正的 bug 是**同一个账号后来带 userId 再落盘时，旧那份没被带走** ⇒ 目录里两份、
	// 池里两行（真机实测：`monkeycode-session-b4cb99d….json` 与
	// `monkeycode-002d3463-….json` 是同一个账号，面板上出现重复行）。
	// 治本落在 `SaveFile` → `migrateLegacySessionFile`：身份升级时把同 session 的旧档
	// **改名**（不删）⇒ 不会再有两份。
	//
	// ⚠ 这里保留一条**可搜索的日志**：缺 userId 的行值得被看见（它是"两份文件"的前置条件），
	//   但看见不等于要拦 —— 拦会把合法的极简行一起拦掉。
	if a.UserID == "" {
		logf("monkeycode: 导入的这行缺 userId（只有 session）—— 将按 session 短哈希命名落盘；" +
			"该账号以后若带 userId 再落盘，旧档会被自动迁移（见 SaveFile 的 migrateLegacySessionFile）")
	}

	// 额度是**探测时写入**的字段，这里顺手带上：
	// 有了它导入后界面立刻有数，不必等第一轮额度刷新。
	a.Balance = jsonNumberToString(d.RawBalance)
	a.DailyToken = jsonNumberToString(d.RawDailyBal)
	a.DailyTokenLimit = jsonNumberToString(d.RawDailyLim)

	// 续期材料（2026-11-01 起）。空值就是空值 —— **不要**造假：
	// 注册机那边有 `hasPassword=true` 但没导出明文的形态（旧的裁剪配置），
	// 那种情况下 Password 必须留空，让 RenewSession 报"缺 email/password"，
	// 而不是拿一个编出来的值去 password-login（那会白撞一次上游 + 记一次失败）。
	a.Email = strings.TrimSpace(d.Email)
	a.Password = d.Password // ⚠ 不 TrimSpace：密码的前后空格可能是它的一部分

	// 注册机那边的 renewedAt 也带上（毫秒时间戳数字 → time.Time）。
	// 有它 ⇒ expiry.go 的 SessionAnchor 能用"真续期时刻 + 30 天"，
	// 而不是拿 AddedAt 估（那会把刚续过的号误报成"即将过期"）。
	if t, ok := ParseAuthTime(strings.TrimSpace(string(d.RenewedAt))); ok {
		a.SessionRenewedAt = t
	}

	// ⚠ 注册机记录里**没有** api_key / signing_secret —— 那是本仓独有的。
	// 导入后由 preparePasted / 首次调用时补签（见 login.go 的说明）。
	return a, nil
}

// normalizeRegion 把各种写法的区域归一。
func normalizeRegion(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case RegionIntl, "global", "international", "net":
		return RegionIntl
	case RegionCN, "com", "china", "domestic":
		return RegionCN
	}
	// 空/未知：留空让 probeStatus 自己探（cn → intl 各试一次）。
	return ""
}

// jsonNumberToString 把 json.Number/数字/字符串统一成十进制字符串。
//
// # 为什么要这个函数（而不是直接 json.Unmarshal 到 string）
//
// 注册机的 balance 是**数字**（200000），而本仓 Auth.Balance 是 **string**
// （因为它只用于展示、且要能表达"未知"）。直接把数字解进 string 会让
// 整个 Unmarshal 失败 —— 那正是导入一直报"没有可用的 session"的根因。
//
// 空 RawMessage / null / 解析不出数字 → 返回空串（= 未知），
// 而不是 "0"（0 是"确实为零"，与"没给"是两件事）。
func jsonNumberToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}
