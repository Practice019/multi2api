// usagetap_test.go 从对话回执旁听 points_consumed 的守卫。
//
// # 这里守的是用户的想法（原话）
//
//	「如果发一次请求就有返回额度的话，那就直接每次发请求之后，我们就把那个
//	  额度给更新掉就行了，把那个缓存给更新掉。」
//
// 实测上游两种模式都带 `usage.points_consumed`（见 usagetap.go 的文件头），
// 于是"想更新缓存就必须额外发请求"这个困境消失了。
//
// ⚠ 但它是**热路径上的透传层**，所以最要紧的判据不是"读出来了"，而是
// **"它没有改变任何东西"**。
package loomy

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// readAllThroughTap 用**很小的读缓冲**反复 Read，逼出"关键字被读边界切开"
// 这个真实形态（生产上 net 的读缓冲大小不可控）。
func readAllThroughTap(t *testing.T, body string, uid string, cb func(string, int64)) (string, []int64) {
	t.Helper()
	var got []int64
	onC := func(u string, pts int64) {
		if cb != nil {
			cb(u, pts)
		}
		got = append(got, pts)
	}
	src := io.NopCloser(strings.NewReader(body))
	tap := newUsageTap(src, uid, onC)
	var out bytes.Buffer
	buf := make([]byte, 7) // 故意很小：切碎关键字
	for {
		n, err := tap.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	_ = tap.Close()
	return out.String(), got
}

// ★★ 最要紧的一条：**不改一个字节**。
//
// 它是透传层 —— 转发出去的内容必须与不挂它时逐字节相同。多一个空格、
// 少一个换行都会破坏 SSE 的语义。
func TestUsageTapForwardsBytesUnchanged(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\r\n\r\n" +
		"data: {\"choices\":[],\"usage\":{\"points_consumed\":3}}\r\n\r\ndata: [DONE]\r\n\r\n"
	out, _ := readAllThroughTap(t, body, "U1", nil)
	if out != body {
		t.Errorf("★ 透传层改动了内容：\n  原样 %q\n  出来 %q\n"+
			"它必须逐字节相同（SSE 的换行与分隔符都有语义）。", body, out)
	}
}

// ⚠ 流式里 usage 会出现在**两块** SSE 中（实测：finish_reason 那块与最后那块，
// 值相同）⇒ 必须取**最后**一次，不能累加。
//
// 累加会翻倍扣减，让账号的免费额度被**过早**判成用完 —— 那会把一个还有
// 免费额度的号踢出池子（正是用户最不想要的"没用完就去动永久"的镜像）。
func TestUsageTapDoesNotDoubleCount(t *testing.T) {
	body := "data: {\"usage\":{\"points_consumed\":5}}\n\n" +
		"data: {\"usage\":{\"points_consumed\":5}}\n\ndata: [DONE]\n\n"
	_, got := readAllThroughTap(t, body, "U1", nil)
	if len(got) != 1 {
		t.Fatalf("应当只回调一次（结束时一次），实际 %d 次：%v", len(got), got)
	}
	if got[0] != 5 {
		t.Errorf("★ 应当取**最后一次**的值 5，实际 %d ——\n"+
			"累加（会得到 10）会让免费额度被过早判成用完。", got[0])
	}
}

// 关键字被读边界切开也要能认出来（生产上读缓冲大小不可控）。
func TestUsageTapHandlesSplitKey(t *testing.T) {
	// 让 `"points_consumed"` 恰好跨在 7 字节的读边界上。
	pad := strings.Repeat("x", 3)
	body := "{\"pad\":\"" + pad + "\",\"usage\":{\"points_consumed\":7}}"
	_, got := readAllThroughTap(t, body, "U1", nil)
	if len(got) != 1 || got[0] != 7 {
		t.Errorf("★ 跨读边界的关键字没被认出来（得到 %v，期望 [7]）——\n"+
			"没有 carry 尾巴的话，生产上会随机漏读（取决于网络分片）。", got)
	}
}

// 读不出来/格式变了 ⇒ 静默：不回调、不 panic、照样转发。
func TestUsageTapSilentOnGarbage(t *testing.T) {
	for _, body := range []string{"", "not json at all", `{"usage":{}}`,
		`{"points_consumed":"abc"}`, "\x00\x01\x02"} {
		out, got := readAllThroughTap(t, body, "U1", nil)
		if out != body {
			t.Errorf("%q：透传被改动", body)
		}
		if len(got) != 0 {
			t.Errorf("%q：不该回调（得到 %v）", body, got)
		}
	}
}

// uid 为空时不回调（池子以 uid 为键，空键会污染池子）。
func TestUsageTapSkipsEmptyUID(t *testing.T) {
	body := `{"usage":{"points_consumed":3}}`
	_, got := readAllThroughTap(t, body, "", nil)
	if len(got) != 0 {
		t.Errorf("uid 为空时不该回调，实际 %v", got)
	}
}

// ── 池子侧：减法 ─────────────────────────────────────────────────────

// ★ 核心效果：扣到 ≤0 ⇒ 这个号**从池子里出去**（用户要的那条）。
func TestPoolConsumedMovesAccountOut(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()
	po := p.pool()
	po.noteQuota("HAVE", 3, true, now) // 剩 3
	po.noteQuota("KEEP", 100, true, now)

	if got := p.CandidateTiers("", []string{"HAVE"}); got["HAVE"] != tierFree {
		t.Fatalf("前置：HAVE 应在池子里（实际 %d）", got["HAVE"])
	}

	po.noteConsumed("HAVE", 2, now) // 剩 1
	if got := p.CandidateTiers("", []string{"HAVE"}); got["HAVE"] != tierFree {
		t.Errorf("还剩 1 时应当仍在池子里（实际 %d）", got["HAVE"])
	}

	po.noteConsumed("HAVE", 2, now) // 1-2 = -1 ≤ 0 ⇒ 用完
	if got := p.CandidateTiers("", []string{"HAVE", "KEEP"}); got["HAVE"] != tierPaid {
		t.Errorf("★ 扣到 ≤0 之后该号应当落 tierPaid(%d)（实际 %d）——\n"+
			"\n"+
			"用户要的正是「免费额度为0就从池子里出去」。\n"+
			"不移出去它会继续被优先选中，也就是继续烧永久额度。",
			tierPaid, got["HAVE"])
	}
}

// ⚠ 没有绝对值基准时**什么都不做**：凭空减等于造数，而造出来的"已用完"
// 会让一个还有免费额度的号被跳过。
func TestPoolConsumedWithoutBaselineDoesNothing(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	po.mu.Lock()
	po.have["A"] = true // 在池子里，但**没有** free 绝对值
	po.mu.Unlock()

	po.noteConsumed("A", 9999, time.Now())

	if have, _ := po.snapshot(time.Now()); !have["A"] {
		t.Error("★ 没有绝对值基准时不该把账号移出池子 ——\n" +
			"那等于凭一个猜出来的数说「免费额度用完了」，\n" +
			"而它可能还有很多。")
	}
}

// ⚠ 对**完全未知**的账号扣减不该把它标成 known-0（未知 ≠ 已用完）。
func TestPoolConsumedIgnoresKnownZero(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	po.noteConsumed("ZERO", 5, time.Now())
	if _, zero := po.snapshot(time.Now()); zero["ZERO"] {
		t.Error("★ 对未知账号扣减把它标成了 known-0 ——\n" +
			"「未知」与「已用完」是两件事。标成已用完会让一个**可能还有免费额度**的号\n" +
			"被跳过（而当池子里再没有别的号时，整池都会被判成\"免费额度用完\"" +
			"，于是直接开始烧永久额度）。")
	}
}
