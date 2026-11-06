package gateway

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncate 钉住 Truncate 的语义（T-234：从 5 个上游包里提出来的共享件）。
//
// # 为什么期望值里有表达式而不是全写死字面量
//
// "中文测试" 那个用例的期望值**故意**写成 `"中文测试"[:5] + "…"` 而不是手抄一个
// 看起来像"中文…"的字面量：字节截断的第 5 个字节落在**第三个字节组中间**，
// 手抄很容易抄成"看起来对、实际差一个字节"的期望值 —— 那种测试会一直绿，
// 却不再测任何东西。配合下面的属性断言（长度 + 非法 UTF-8），意图才被钉住。
func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"空串", "", 5, ""},
		{"短于上限", "abc", 5, "abc"},
		{"恰好等于上限不截", "abcdef", 6, "abcdef"},
		{"超长补省略号", "abcdef", 3, "abc…"},
		{"超长一个字节也截", "abcdef", 5, "abcde…"},
		{"n<=0 原样返回", "abcdef", 0, "abcdef"},
		{"n 为负原样返回", "abcdef", -1, "abcdef"},
		{"中文按字节截（会切坏）", "中文测试", 5, "中文测试"[:5] + "…"},
	}
	for _, c := range cases {
		if got := Truncate(c.in, c.n); got != c.want {
			t.Errorf("%s: Truncate(%q, %d) = %q，want %q", c.name, c.in, c.n, got, c.want)
		}
	}
}

// TestTruncateCutsAtByteBoundaryNotRuneBoundary 反向断言：**知道**自己会切坏字符。
//
// 这条是"字节语义是刻意的"这个决定的守卫：若有人把实现改成 rune-safe
// （看起来是"修 bug"），这里会立刻红，逼他去看 truncate.go 的注释再决定 ——
// 要么恢复字节语义，要么新建 TruncateRunes 而不是偷改这个。
func TestTruncateCutsAtByteBoundaryNotRuneBoundary(t *testing.T) {
	const in = "中文测试" // 12 字节 / 4 rune
	got := Truncate(in, 5)

	if len(got) != 5+len("…") {
		t.Fatalf("长度 = %d，want %d（应为 n 字节 + 省略号）", len(got), 5+len("…"))
	}
	if !strings.HasPrefix(got, in[:5]) {
		t.Errorf("前缀 = %q，want %q（必须是原始字节前缀）", got, in[:5])
	}
	if utf8.ValidString(got[:5]) {
		t.Error("前缀竟然是合法 UTF-8 —— 实现已被改成 rune-safe（见 truncate.go 的『为什么是字节』）")
	}
	if !utf8.ValidString(in) {
		t.Fatal("夹具本身必须是合法 UTF-8，否则这条测试没有意义")
	}
}

// TestTruncateNeverGrows 截断只会变短（或不变）—— 与原件同款的基本不变量。
func TestTruncateNeverGrows(t *testing.T) {
	for _, n := range []int{0, 1, 3, 8, 64, 1024} {
		for _, in := range []string{"", "a", "中文测试", strings.Repeat("x", 2048)} {
			got := Truncate(in, n)
			if len(got) > len(in) && len(in) > n {
				t.Errorf("Truncate(len=%d, n=%d) 变成了 %d 字节（比原串还长）", len(in), n, len(got))
			}
			if n > 0 && len(in) > n && !strings.HasSuffix(got, "…") {
				t.Errorf("Truncate(len=%d, n=%d) 没有省略号，得到 %q", len(in), n, got)
			}
		}
	}
}
