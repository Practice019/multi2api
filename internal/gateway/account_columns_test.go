package gateway

import (
	"reflect"
	"testing"
)

// TestDefaultAccountColumnsExact 钉住默认列**逐列等于改造前 11 列去掉熔断/在途**。
//
// # 为什么这条断言的分量很重
//
// 用户明确要求 workbuddy「直接复用现在的标题」。而 workbuddy **不实现**
// AccountColumnsExt（后端零改动）—— 它走的就是这个默认值。
//
// 所以这个数组**就是** workbuddy 的表头契约：改它 = 改用户可见的观感。
// 本轮用户明确要求删掉「熔断」「在途」两列（workbuddy 与 loomy 都删，
// 见 DefaultAccountColumns 的注释）—— 这是**用户点名**的变化，所以
// 这里从"逐字等于 11 列"更新为"等于去掉那两列的 9 列"。顺序仍钉住。
func TestDefaultAccountColumnsExact(t *testing.T) {
	want := []string{
		"provider", "nickname", "uid", "quota", "status", "token",
		"checkin", "success", "ops",
	}
	got := DefaultAccountColumns()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("默认列集变了 —— workbuddy 的表头会跟着变\n"+
			"  got : %v\n  want: %v", got, want)
	}
	for _, removed := range []string{"breaker", "in_flight"} {
		for _, id := range got {
			if id == removed {
				t.Errorf("默认列集里仍有 %q —— 用户明确要求删掉「熔断」「在途」", removed)
			}
		}
	}
}

// TestDefaultAccountColumnsReturnsCopy 钉住"返回副本"。
//
// 调用方（admin 层）可能裁剪或排序。若返回的是同一个底层数组，
// 一次就地改写会**永久污染**后续所有调用 —— 而且不报错：
// 下一个上游拿到的"默认值"已经被上一个调用方改过了。
func TestDefaultAccountColumnsReturnsCopy(t *testing.T) {
	a := DefaultAccountColumns()
	if len(a) == 0 {
		t.Fatal("默认列是空的")
	}
	a[0] = "被改过了"

	b := DefaultAccountColumns()
	if b[0] != AccountColProvider {
		t.Fatalf("DefaultAccountColumns 返回的是共享底层数组 —— "+
			"上一次调用方的就地改写泄漏到了下一次（b[0]=%q）", b[0])
	}
}

// TestIsKnownAccountColumn 钉住词汇表的边界。
//
// 拼错的列 id 是**静默失效**：前端查不到就跳过那一列，页面上看不出
// 是漏了还是本来没有。所以词汇表必须能被可靠地问到。
func TestIsKnownAccountColumn(t *testing.T) {
	for _, id := range DefaultAccountColumns() {
		if !IsKnownAccountColumn(id) {
			t.Errorf("默认列 %q 却不在词汇表里 —— 两者必须一致", id)
		}
	}
	for _, id := range []string{AccountColTokenExpiry, AccountColWelfare} {
		if !IsKnownAccountColumn(id) {
			t.Errorf("新增列 %q 不在词汇表里", id)
		}
	}
	for _, bad := range []string{"", "token_expire", "TokenExpiry", "welfare ", "签到"} {
		if IsKnownAccountColumn(bad) {
			t.Errorf("%q 不该被判为已知列 id", bad)
		}
	}
}
