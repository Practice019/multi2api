package gateway

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// usagetap_test.go 从对话回执旁听"这次花了多少"的守卫。
//
// # 这里守的是用户的想法（原话）
//
//	「我们去调用那个模型接口的时候，它会不会返回额度信息呢？这样我们就可以
//	  直接获取了，就不需要再额外发请求了。」
//
// 实测只有少数上游会写（见 usagetap.go 的文件头那张表），但做法完全一样 ——
// 所以它被抽到这里。而它是**热路径上的透传层**，最要紧的判据不是"读出来了"，
// 而是"**它没有改变任何东西**"。

// runTap 用**很小的读缓冲**反复 Read，逼出"关键字被读边界切开"这个真实形态
// （生产上 net 的读缓冲大小不可控）。
func runTap(t *testing.T, body string, spec UsageTapSpec) (string, []int64) {
	t.Helper()
	var got []int64
	tap, err := NewUsageTap(io.NopCloser(strings.NewReader(body)), spec, func(v int64) {
		got = append(got, v)
	})
	if err != nil {
		t.Fatalf("NewUsageTap: %v", err)
	}
	var out bytes.Buffer
	buf := make([]byte, 7) // 故意很小：切碎关键字
	for {
		n, rerr := tap.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if rerr != nil {
			break
		}
	}
	_ = tap.Close()
	return out.String(), got
}

func consumedSpec(key string) UsageTapSpec {
	return UsageTapSpec{Key: key, Scale: 1, Mode: UsageConsumed}
}

// ★★ 最要紧的一条：**不改一个字节**。
//
// 多一个空格、少一个换行都会破坏 SSE 的语义。
func TestUsageTapForwardsBytesUnchanged(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\r\n\r\n" +
		"data: {\"choices\":[],\"usage\":{\"points_consumed\":3}}\r\n\r\ndata: [DONE]\r\n\r\n"
	out, _ := runTap(t, body, consumedSpec("points_consumed"))
	if out != body {
		t.Errorf("★ 透传层改动了内容：\n  原样 %q\n  出来 %q\n"+
			"它必须逐字节相同（SSE 的换行与分隔符都有语义）。", body, out)
	}
}

// ★★ 抽象层的安全阀：**声明成「成本」就绝不许驱动扣减**。
//
// `cline` 的 `usage.cost`（美元成本）与 `loomy` 的 `usage.points_consumed`
// （消耗的积分）**长得都像一个数**。把成本当消耗去扣额度就是编数据 ——
// 会把一个还有额度的账号判成用完。
func TestUsageTapCostModeNeverDrivesDecrement(t *testing.T) {
	body := `{"usage":{"cost":1.47e-05,"points_consumed":9}}`
	// 同一份回执、同一个键，只把 Mode 从 consumed 换成 cost。
	out, got := runTap(t, body, UsageTapSpec{Key: "cost", Scale: 1e6, Mode: UsageCost})
	if out != body {
		t.Error("成本模式也必须逐字节透传")
	}
	if len(got) != 0 {
		t.Errorf("★ Mode=UsageCost 时不该回调（拿到 %v）——\n"+
			"\n"+
			"这是抽象层的核心安全阀：回执里的 cost 是**费用**，不是余额的减少量。\n"+
			"拿它去扣额度，会把一个还有额度的账号判成用完（或者整池被判成\n"+
			"「免费额度用完」而开始烧钱）。所以「有数字就扣」是错的，\n"+
			"必须由调用方显式声明语义。", got)
	}
}

// ⚠ Mode 没声明 ⇒ **装配期就报错**，而不是默认当成消耗。
//
// 默认当成消耗是"静默编数据"，而装配期报错会在第一次跑起来时就暴露。
func TestUsageTapRequiresExplicitMode(t *testing.T) {
	for _, spec := range []UsageTapSpec{
		{Key: "x"},                                 // Mode 缺省 = Unknown
		{Key: "x", Mode: UsageUnknown, Scale: 1},   // 显式 Unknown
		{Mode: UsageConsumed, Scale: 1},            // Key 为空
		{Key: "x", Mode: UsageConsumed},            // Scale 为 0
		{Key: "x", Mode: UsageConsumed, Scale: -1}, // 负 Scale
	} {
		if _, err := NewUsageTap(io.NopCloser(strings.NewReader("")), spec, func(int64) {}); err == nil {
			t.Errorf("非法 spec 应当报错：%+v", spec)
		}
	}
	// 合法的一对必须能过。
	if _, err := NewUsageTap(io.NopCloser(strings.NewReader("")), consumedSpec("x"), nil); err != nil {
		t.Errorf("合法 spec 不该报错: %v", err)
	}
}

// ⚠ 流式里 usage 会出现在**两块** SSE 中（实测：finish_reason 那块与最后那块，
// 值相同）⇒ 必须取**最后**一次，不能累加。
//
// 累加会翻倍扣减，让余额被**过早**判成用完。
func TestUsageTapDoesNotDoubleCount(t *testing.T) {
	body := "data: {\"usage\":{\"points_consumed\":5}}\n\n" +
		"data: {\"usage\":{\"points_consumed\":5}}\n\ndata: [DONE]\n\n"
	_, got := runTap(t, body, consumedSpec("points_consumed"))
	if len(got) != 1 {
		t.Fatalf("应当只回调一次（结束时一次），实际 %d 次：%v", len(got), got)
	}
	if got[0] != 5 {
		t.Errorf("★ 应当取**最后一次**的值 5，实际 %d ——\n"+
			"累加（会得到 10）会让余额被过早判成用完。", got[0])
	}
}

// 关键字被读边界切开也要能认出来。
func TestUsageTapHandlesSplitKey(t *testing.T) {
	body := "{\"pad\":\"xxx\",\"usage\":{\"points_consumed\":7}}"
	_, got := runTap(t, body, consumedSpec("points_consumed"))
	if len(got) != 1 || got[0] != 7 {
		t.Errorf("★ 跨读边界的关键字没被认出来（得到 %v，期望 [7]）——\n"+
			"没有 carry 尾巴的话，生产上会随机漏读（取决于网络分片）。", got)
	}
}

// 读不出来/格式变了 ⇒ 静默：不回调、不 panic、照样转发。
func TestUsageTapSilentOnGarbage(t *testing.T) {
	for _, body := range []string{"", "not json at all", `{"usage":{}}`,
		`{"points_consumed":"abc"}`, "\x00\x01\x02", `{"points_consumed":}`} {
		out, got := runTap(t, body, consumedSpec("points_consumed"))
		if out != body {
			t.Errorf("%q：透传被改动", body)
		}
		if len(got) != 0 {
			t.Errorf("%q：不该回调（得到 %v）", body, got)
		}
	}
}

// ⚠ Scale 要能处理**小数**（qoder 的 credits 是 0.006352…）与**科学计数法**
// （cline 的成本是 1.47e-05）。
//
// 而且累加必须用整数：本层回调的就是换算后的整数，调用方不必再碰浮点。
func TestUsageTapScalesFractionalAndScientific(t *testing.T) {
	cases := []struct {
		body  string
		key   string
		scale int64
		want  int64
	}{
		{`{"credits":0.006352698571428571}`, "credits", 1e6, 6352},
		{`{"credits":1.5}`, "credits", 1e6, 1500000},
		{`{"cost":1.47e-05}`, "cost", 1e6, 14},
		{`{"points_consumed":0}`, "points_consumed", 1, 0}, // 0 是合法值
	}
	for _, c := range cases {
		_, got := runTap(t, c.body, UsageTapSpec{Key: c.key, Scale: c.scale, Mode: UsageConsumed})
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s（scale=%d）：得到 %v，期望 [%d]", c.body, c.scale, got, c.want)
		}
	}
}

// ⚠ "读到 0" 必须回调（0 消耗是合法的：缓存命中的请求真的可能 0），
// 而"完全没读到"不回调。两者不能混。
func TestUsageTapZeroIsReportedButAbsentIsNot(t *testing.T) {
	if _, got := runTap(t, `{"points_consumed":0}`, consumedSpec("points_consumed")); len(got) != 1 {
		t.Errorf("读到 0 应当回调一次（0 消耗是合法值），实际 %v", got)
	}
	if _, got := runTap(t, `{"usage":{}}`, consumedSpec("points_consumed")); len(got) != 0 {
		t.Errorf("没读到不该回调，实际 %v", got)
	}
}

// nil 源不许 panic（上游给 2xx 却没 body 的形态）。
func TestUsageTapNilSourceDoesNotPanic(t *testing.T) {
	tap, err := NewUsageTap(nil, consumedSpec("x"), func(int64) {})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, rerr := tap.Read(buf); rerr != io.EOF {
		t.Errorf("nil 源应当立刻 EOF，得到 %v", rerr)
	}
	if cerr := tap.Close(); cerr != nil {
		t.Errorf("Close 不该报错: %v", cerr)
	}
}
