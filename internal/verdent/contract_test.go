// contract_test.go 接入全仓统一的上游契约检查。
//
// 这一行不是"形式上的接入"：它真的会跑 Chat / Models / 取消 / Close 四类行为，
// 而伪造凭证下这些方法**必须优雅失败**（返回 error 而不是 panic）——
// 而这恰好是本上游最容易写错的地方（token 为空 / JWT 解不开 / 上游 401
// 三种情况都要走"报错"而不是"崩"）。
package verdent

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestContract 上游行为契约。
func TestContract(t *testing.T) {
	gateway.RunProviderContract(t, func() gateway.Provider { return NewProvider() })
}
