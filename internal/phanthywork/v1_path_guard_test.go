// v1_path_guard_test.go v1 canonical 的 path 段**不能带 query**。
//
// 真源：magpie-PhanthyCode 的 `src/referrals.mjs` 里那条警告（逐字）：
//
//	⚠ `request()` 的 v1 canonical 用的是 `new URL(url).pathname`。
//	  本模块的两个端点都**没有 query**，所以签出来是对的；将来若要加
//	  `referrals/relations?limit=…`，必须先修 `upstreamPath`，否则必然 401。
//
// 也就是说：**URL 可以带 query，签名的 path 不能带。**
// 这是个"加个 `?limit=` 就 401、而错误码指不到根因"的陷阱 ——
// 本包当前的调用点都传无 query 的常量，靠这条判据把它钉住。
package phanthywork

import (
	"strings"
	"testing"
)

func TestSignV1RejectsQueryInPath(t *testing.T) {
	// ① 带 query 的 path：必须当场报错（而不是签出一个服务端不认的串）
	a := &Auth{}
	_, err := signV1(a, v1Params{
		Method: "GET",
		Path:   "/api/oauth/referrals/relations?limit=10",
	}, "1700000000000", "nonce")
	if err == nil {
		t.Fatal("path 带 query 时必须报错 —— 否则会签出一个服务端重算不出来的串，" +
			"上游只会回 401（与签名错的其他情形无法区分）")
	}
	if !strings.Contains(err.Error(), "query") {
		t.Errorf("错误信息应点明 query 这个原因，实得: %v", err)
	}

	// ② 反向：干净的 path 不该命中这条护栏
	//（它可能因为别的理由失败 —— 这里只断言"不是 query 那条"）
	_, err2 := signV1(a, v1Params{
		Method: "GET",
		Path:   "/api/oauth/referrals/relations",
	}, "1700000000000", "nonce")
	if err2 != nil && strings.Contains(err2.Error(), "query") {
		t.Errorf("不带 query 的 path 不该命中这条护栏，实得: %v", err2)
	}
}

// TestV1CanonicalPathSegmentHasNoQuery 直接钉住 canonical 第 3 段就是传进来的 path
//（没有再拼 query 的行为），与官方 `new URL(url).pathname` 同形。
func TestV1CanonicalPathSegmentHasNoQuery(t *testing.T) {
	canonical := V1Canonical("GET", "/api/oauth/referrals/relations", "1", "n", nil, "")
	segs := strings.Split(canonical, deviceV1Separator)
	if len(segs) != 7 {
		t.Fatalf("段数 = %d, want 7: %q", len(segs), canonical)
	}
	if segs[2] != "/api/oauth/referrals/relations" {
		t.Errorf("第 3 段（path）= %q", segs[2])
	}
	if strings.Contains(segs[2], "?") {
		t.Errorf("第 3 段不该含 query: %q", segs[2])
	}
}
