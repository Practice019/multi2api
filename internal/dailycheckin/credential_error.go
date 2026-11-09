// credential_error.go 判定「签到失败是不是凭据失效」（T-431）。
//
// # 用户需求
//
//	「像这种签到失败的自动禁用啊……上游反馈要什么重新登录之类的，或者什么失效了，
//	  这种情况呢，就要自动禁用该账号」
//
// # ★ 本文件是全任务**误杀风险**的所在（design-notes-disabled §1 的头号热点）
//
// 误判一个账号为"凭据失效"= 把一个**好号**踢下线（用户额度损失 + 人工恢复）。
// 所以判据是**保守白名单**，不是"非网络即凭据"：
//
//	① 先看**网络类**词 —— 命中就一律 false（不判凭据）
//	② 再看**凭据类**白名单 —— 命中才 true
//	③ 都没命中 ⇒ **false**（分不清就不要动）
//
// ⚠ 第 ③ 条是刻意的：漏判的代价是"那个号一直签到失败、用户看得见"，
//
//	误判的代价是"好号被静默踢下线、用户不知道为什么"。前者可接受，后者不可接受。
//
// ⚠ 为什么不给各上游加类型化的 ErrKind：`Status()/Claim()` 返回的是 `error`
// （字符串形态）⇒ 要类型化就得改 10 个上游的签名 + gateway 契约，
//
//	而收益只是"少一次字符串匹配"（design-notes-disabled §2 Q6 否决）。
package dailycheckin

import "strings"

// networkHints 明确的**网络/超时/上游维护**措辞 ⇒ 一律不判凭据失效。
//
// ⚠ `网络` 这个词本身就在名单里：上游的错误正文常写成
// 「网络 / 凭据 / 响应形状异常」这种**分不清**的混合措辞（qoder 实测就是这句）——
// 既然它自己都说可能是网络，我们就不能替它断言是凭据。
var networkHints = []string{
	"timeout", "timed out", "dial tcp", "connection refused", "connection reset",
	"i/o timeout", "no such host", "context deadline", "context canceled",
	"broken pipe", "network is unreachable", "temporary failure",
	"eof", "网络", "超时", "连接失败",
}

// credentialHints 明确的**凭据失效**措辞 ⇒ 判 true（保守白名单）。
var credentialHints = []string{
	"401",            // 上游明确回 401（lobsterai 实测：领取 HTTP 401）
	"unauthorized",   // monkeycode 实测：查签到状态返回 401：Unauthorized
	"not logged in",  //
	"login required", //
	"authentication failed",
	"invalid token",   //
	"token expired",   //
	"refresh_token",   //
	"session expired", //
	"请先登录",            // lobsterai 实测：{"code":51102,"message":"请先登录"}
	"未登录",             //
	"登录已失效",           //
	"需要重新登录",          //
	"apig.0602",       // 华为系：token expired
}

// CredentialError 该错误是否**确定**是凭据失效（导出：上游侧与测试都用得到）。
//
// 判定顺序是判据的一部分（先排网络、再看白名单、都不命中就 false）。
func CredentialError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, n := range networkHints {
		if strings.Contains(s, n) {
			return false // ★ 网络类优先排除
		}
	}
	for _, c := range credentialHints {
		if strings.Contains(s, c) {
			return true
		}
	}
	return false // ★ 分不清就不动
}
