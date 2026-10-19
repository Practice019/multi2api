// statustest_test.go 本包测试访问 `/status` 的共享凭据。
//
// # 为什么值得单独一个文件
//
// `/status` 在 T7 从「认 api_key」改成「认控制台身份」之后，凡是打它的测试
// 都必须带一个**管理令牌**。把那把令牌与取请求的辅助函数集中在一处，避免
// 十几个测试各自硬编码字符串（改口径时只改这一处）。
//
// ⚠ 这些测试**没有**、将来也**不要**加"跳过门禁"的开关。凭据是真的、判据是
// 真的 —— 唯一被替换掉的只是"人怎么拿到它"（测试里直接给出）。
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// statusTestToken 测试用的管理令牌。
//
// ⚠ 明文常量在测试里是安全的（它不对应任何真实部署；服务端每次比较的也是
// 本次构造的 Config 里那把）。**不要**把它抄进别的包当默认值。
const statusTestToken = "status-test-admin-token-0123456789"

// statusReq 造一个带上管理令牌的 `/status` 请求。
func statusReq(t *testing.T) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.Header.Set("Authorization", "Bearer "+statusTestToken)
	return r
}
