// capI_parity_test.go capI 必须与 JS 原版**逐字节一致**。
//
// # 为什么这条测试是签到能不能用的唯一判据
//
// PoW 的输出只有两种结果：redeem 成功，或 redeem 回一句 `success:false`。
// 那句错误**不会**告诉你"你的 capI 序列错了"还是"位比较错了"。
//
// 而 capI 的错法极其隐蔽：我第一版把 p 写成 uint32，
// 编译通过、单元测试全绿（因为测试只断言"同输入同输出"）、
// 真机也能跑到 challenge/redeem —— 只是 redeem 永远失败。
//
// 所以必须有**外部标准样本**。下面的向量是用 node 跑 JS 原版算出来的，
// 不是从我的 Go 实现反推的。
//
// 复现命令（JS 原版）：
//
//	function capI(str,len){let p=2166136261;for(let i=0;i<str.length;i++){
//	  p=(p|0)^str.charCodeAt(i);
//	  p=p+(((p<<1)|0)+((p<<4)|0)+((p<<7)|0)+((p<<8)|0)+((p<<24)|0));}
//	  let u=p>>>0;const next=()=>{u=((u|0)^((u<<13)|0))>>>0;
//	  u=((u|0)^(u>>>17))>>>0;u=((u|0)^((u<<5)|0))>>>0;return u};
//	  let out='';while(out.length<len)out+=next().toString(16).padStart(8,'0');
//	  return out.substring(0,len)}
package monkeycode

import "testing"

// TestCapIMatchesJavascript 与 JS 原版的标准样本逐字节比对。
func TestCapIMatchesJavascript(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		length int
		want   string
	}{
		{"abc/32", "abc", 32, "0bb9adb8ffd8e55f8d1de826333f356a"},
		{"tokenabc1/32", "tokenabc1", 32, "db55214c90ef40b267dea50edc98aa91"},
		{"x/8", "x", 8, "948dea29"},
		{"hello world/16", "hello world", 16, "fd8f5598f0b79ec6"},
		{"0123456789abcdef/32", "0123456789abcdef", 32, "3aa9e9d0f5eea93939fab2fd9b0180cf"},
		{"salt/32", "salt", 32, "93346e3ec08789a707dd5ebe2d53dfdb"},
		{"a/32", "a", 32, "441aaeb828796d9ebc9f401bc71d40f5"},
		{"empty/32", "", 32, "4622a6774f65af21aabfbe41f381afba"},
	}
	for _, c := range cases {
		got := capI(c.input, c.length)
		if got != c.want {
			t.Errorf("%s: capI(%q, %d)\n got = %q\nwant = %q", c.name, c.input, c.length, got, c.want)
		}
	}
}

// TestCapIIntermediateValues 钉住"加法不截断"这条语义。
//
// # 为什么单独测这个
//
// capI 能不能对上，全看 p 在加法那一步有没有被截成 32 位。
// 直接测中间值，可以在实现被"优化"成 uint32 时**立刻**指出错在哪一步，
// 而不是只看到最终 hex 不同。
func TestCapIIntermediateValues(t *testing.T) {
	// 这三组中间值是用 node 跑 JS 原版逐步打印出来的（"abc"）。
	type step struct {
		p0      int32
		shifted int64
		p       int64
	}
	want := []step{
		{p0: -2128831068, shifted: -2635101304, p: -4763932372},
		{p0: -468965042, shifted: 1763236988, p: 1294271946},
		{p0: 1294271913, shifted: -5148318878, p: -3854046965},
	}
	str := "abc"
	p := float64(2166136261)
	for i := 0; i < len(str); i++ {
		p0 := toInt32(p) ^ int32(str[i])
		shifted := int64(toInt32f(float64(p0) * 2))
		shifted += int64(toInt32f(float64(p0) * 16))
		shifted += int64(toInt32f(float64(p0) * 128))
		shifted += int64(toInt32f(float64(p0) * 256))
		shifted += int64(toInt32f(float64(p0) * 16777216))
		p = float64(p0) + float64(shifted)
		if p0 != want[i].p0 {
			t.Errorf("i=%d p0: got %d want %d", i, p0, want[i].p0)
		}
		if shifted != want[i].shifted {
			t.Errorf("i=%d shifted: got %d want %d", i, shifted, want[i].shifted)
		}
		if int64(p) != want[i].p {
			t.Errorf("i=%d p: got %d want %d（加法必须不截断）", i, int64(p), want[i].p)
		}
	}
	if u := uint32(uint64(int64(p)) & 0xFFFFFFFF); u != 440920331 {
		t.Errorf("最终 p>>>0: got %d want 440920331", u)
	}
}
