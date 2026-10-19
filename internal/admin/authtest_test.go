// authtest_test.go 包内测试的凭据助手。
//
// # 为什么需要它（T4 之后）
//
// 门禁改成"一律校验凭据"之后，**本机直连不再免令牌** ⇒ 所有"裸调管理端点"
// 的测试都会拿到 401。那不是测试坏了，是门禁在正常工作：
//
//	改造前 `localExempt()` 对"监听回环 + 上一跳回环 + 无转发头"的请求
//	无条件放行 —— 那正是用户报障「填错 admin token 居然能进 UI」的根因。
//	既有的 25 个测试都建立在"本机不用凭据"这个前提上，所以它们必须改。
//
// 这里给出一份唯一的测试令牌与包装函数，而不是让 25 个文件各写一份
// （写岔的后果是"某个测试悄悄用别的令牌"，排查起来毫无线索）。
//
// ⚠ 绝**不**加"测试期跳过门禁"的开关（例如 Config.TestBypass 或包内
// 可变字段）：那等于把刚关上的洞重新打开，而且会在生产代码里永久留下
// 一个能绕过鉴权的字段 —— 这类字段最后都会以 CVE 的形式被记起来。
// 测试要过门禁，就**真的带凭据**。
package admin

import "net/http"

// testAdminToken 包内测试统一使用的管理令牌（仅测试，绝不进生产配置）。
const testAdminToken = "test-admin-token-0123456789"

// withTestAuth 给请求带上测试管理令牌，返回同一个 req（便于链式使用）。
//
// 用法：`h.ServeHTTP(rec, withTestAuth(req))`。
func withTestAuth(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	return req
}
