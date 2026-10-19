package authlimit

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestCaptcha 出题器（上限给足：这些用例一次会发上百道题，题目本身不消费）。
func newTestCaptcha(t *testing.T) *Captcha {
	t.Helper()
	return NewCaptcha(5*time.Minute, 512)
}

// pendingAnswer 读出某道题面的真答案。
//
// ⚠ 只有同包用例能做这件事：对外接口刻意**不**提供"查答案"（那等于把答案
// 送回去）。所以用例必须先 Issue、再立刻取，取完自己做断言。
func pendingAnswer(t *testing.T, c *Captcha, id string) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[id]
	if !ok {
		t.Fatalf("题面 %q 不在待校验表里", id)
	}
	return item.answer
}

// TestCaptchaCorrectAnswerPassesAndIsOneShot 答对放行；同一 id 第二次必拒。
func TestCaptchaCorrectAnswerPassesAndIsOneShot(t *testing.T) {
	c := newTestCaptcha(t)
	now := baseTime()

	id, svg := c.Issue(now)
	if id == "" || svg == "" {
		t.Fatalf("应当发出题面，得到 id=%q svg=%q", id, svg)
	}
	if !strings.HasPrefix(id, "c-") || len(id) != len("c-")+16 {
		t.Errorf("题面 ID 形态应为 c-<16 hex>（crypto/rand），得到 %q", id)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Errorf("SVG 头尾不对：%.60q", svg)
	}

	ans := strconv.Itoa(pendingAnswer(t, c, id))
	if !c.Verify(id, ans, now.Add(time.Second)) {
		t.Fatalf("正确答案 %q 应当放行", ans)
	}
	// ⚠ 一次性：同一个 id 第二次必拒。答案空间只有 2..18 共 17 个取值，
	// 只要"答对不消费"，同一道题就能被反复利用。
	if c.Verify(id, ans, now.Add(time.Second)) {
		t.Error("★ 同一题面第二次必须被拒（用过即废）")
	}
}

// TestCaptchaWrongAnswerRejectedAndStillConsumed 答错也消费。
func TestCaptchaWrongAnswerRejectedAndStillConsumed(t *testing.T) {
	c := newTestCaptcha(t)
	now := baseTime()

	id, _ := c.Issue(now)
	right := pendingAnswer(t, c, id)
	wrong := strconv.Itoa(right + 1) // 一定不等于正确答案

	if c.Verify(id, wrong, now) {
		t.Fatal("错误答案不该放行")
	}
	// ⚠ 这是"一次性"的另一半：错答之后再拿正确答案来也不行。
	// 若答错不消费，17 个取值就是 17 次免费尝试，一个 for 循环能过 ——
	// 这层人机校验等于没装。
	if c.Verify(id, strconv.Itoa(right), now) {
		t.Error("★ 错答之后正确作答也不能再用：答错同样消费题面")
	}
}

// TestCaptchaExpiry 过期即废（与答对答错无关）。
func TestCaptchaExpiry(t *testing.T) {
	const ttl = 2 * time.Minute
	c := NewCaptcha(ttl, 8)
	now := baseTime()

	id, _ := c.Issue(now)
	ans := strconv.Itoa(pendingAnswer(t, c, id))
	if !c.Verify(id, ans, now.Add(ttl-time.Millisecond)) {
		t.Error("到期前一毫秒应当仍然有效")
	}

	id2, _ := c.Issue(now)
	ans2 := strconv.Itoa(pendingAnswer(t, c, id2))
	if c.Verify(id2, ans2, now.Add(ttl)) {
		t.Error("★ 正好到期必须作废")
	}

	id3, _ := c.Issue(now)
	ans3 := strconv.Itoa(pendingAnswer(t, c, id3))
	if c.Verify(id3, ans3, now.Add(ttl+time.Hour)) {
		t.Error("★ 过期题面必须作废（题面不能长期有效，否则可以离线慢慢识别）")
	}
}

// TestCaptchaDefaultTTL ttl 没给（<=0）时用 defaultCaptchaTTL。
func TestCaptchaDefaultTTL(t *testing.T) {
	c := NewCaptcha(0, 4)
	now := baseTime()

	id, _ := c.Issue(now)
	ans := strconv.Itoa(pendingAnswer(t, c, id))
	if !c.Verify(id, ans, now.Add(defaultCaptchaTTL-time.Second)) {
		t.Error("默认有效期应当是 defaultCaptchaTTL（少一秒仍有效）")
	}

	id2, _ := c.Issue(now)
	ans2 := strconv.Itoa(pendingAnswer(t, c, id2))
	if c.Verify(id2, ans2, now.Add(defaultCaptchaTTL)) {
		t.Error("默认有效期到点必须作废")
	}
}

// TestCaptchaPendingLimitAndCleanup 待校验条数封顶：满了就不发题（响亮失败），
// 过期清掉之后又能发。
func TestCaptchaPendingLimitAndCleanup(t *testing.T) {
	c := NewCaptcha(time.Minute, 2)
	now := baseTime()

	id1, _ := c.Issue(now)
	id2, _ := c.Issue(now.Add(time.Second))
	if id1 == "" || id2 == "" {
		t.Fatal("前两道应当发得出来")
	}
	if id3, svg := c.Issue(now.Add(2 * time.Second)); id3 != "" || svg != "" {
		t.Errorf("超过 maxPending 时必须不发题（空 id + 空 svg），得到 id=%q", id3)
	}
	if n := c.Cleanup(now.Add(2 * time.Minute)); n != 2 {
		t.Errorf("两条都过期，应当清掉 2 条，清了 %d 条", n)
	}
	if id4, _ := c.Issue(now.Add(2 * time.Minute)); id4 == "" {
		t.Error("过期题面清掉之后应当又能出题")
	}
	if n := c.Cleanup(now.Add(2 * time.Minute)); n != 0 {
		t.Errorf("没有过期题面时应清 0 条，清了 %d 条", n)
	}
}

// attrValueRe 匹配 SVG 属性值（`d="M12 8..."`、`width="96"` 之类）。
var attrValueRe = regexp.MustCompile(`="[^"]*"`)

// stripAttrValues 把所有属性值整体挖掉，只留标签名与文本节点。
func stripAttrValues(svg string) string {
	return attrValueRe.ReplaceAllString(svg, `=""`)
}

// TestCaptchaSVGLeaksNoAnswerText ★ 本文件的核心判据：SVG 里搜不到答案。
//
// 需求是"挡住脚本批量注册"。而 `<text>7</text>` 用一条正则就能读出来，
// `aria-label="10"` 更省事，一把 grep 就够 —— 那这层等于没装。所以断言三件事：
//
//	① 原始 SVG 里没有文本节点形态的答案（`<text>7</text>` 就是 `>7<`）；
//	② 把**几何数据**整体剥掉后，剩下的骨架里也搜不到答案 —— 说明答案只是
//	   坐标点集，没有别的下落；
//	③ 可读文本的载体（<text/<title/<desc/aria-label）一个都不许有。
//
// ⚠ 为什么不直接 `strings.Contains(svg, ansStr)` 了事：SVG 的坐标本身就是
// 十进制数（`M72 8` 里就含 "7"），那种断言会在正确答案上随机变红。会随机红的
// 断言等于没有断言 —— 它挡不住任何东西，只会被人加个 //nolint 绕掉。
//
// 跑 100 轮是因为渲染带随机（字距/偏移/干扰线），单轮只能证明"这一张图没漏"。
func TestCaptchaSVGLeaksNoAnswerText(t *testing.T) {
	c := newTestCaptcha(t)
	now := baseTime()

	const rounds = 100
	for i := 0; i < rounds; i++ {
		id, svg := c.Issue(now)
		if id == "" {
			t.Fatalf("第 %d 轮发不出题面", i)
		}
		ansStr := strconv.Itoa(pendingAnswer(t, c, id))

		if strings.Contains(svg, ">"+ansStr+"<") {
			t.Fatalf("★ 第 %d 轮 SVG 里有文本节点形式的答案 %s：%s", i, ansStr, svg)
		}
		if bare := stripAttrValues(svg); strings.Contains(bare, ansStr) {
			t.Fatalf("★ 第 %d 轮剥掉几何后仍能搜到答案 %s：%s", i, ansStr, bare)
		}
		for _, banned := range []string{"<text", "<title", "<desc", "aria-label", "font-family"} {
			if strings.Contains(svg, banned) {
				t.Fatalf("★ 第 %d 轮 SVG 里出现了可读文本载体 %q：%s", i, banned, svg)
			}
		}
	}
}

// TestCaptchaSameQuestionRendersDifferently 同一道题每次渲染都不一样。
//
// 加数只有 operandMax² 种组合。如果同一道题永远画出同一张图，人工解一次就得到
// 了"这张图 = 这个和"这条记录，之后脚本按图查表即可 —— 人机校验白装。所以
// 必须随机化（见 drawQuestion 的注释）。
func TestCaptchaSameQuestionRendersDifferently(t *testing.T) {
	const rounds = 50
	seen := make(map[string]bool, rounds)
	for i := 0; i < rounds; i++ {
		seen[drawQuestion("7+3=?")] = true
	}
	if len(seen) < 45 {
		t.Errorf("★ 同一道题 %d 次渲染只有 %d 种图（随机化不足，可被查表破解）", rounds, len(seen))
	}
}

// TestCaptchaGlyphTable 守住字模形状：5 列 × 7 行，只出现 '#' 与 '.'。
//
// 手写字模最容易犯的错是某一行多打或少打一格 —— 那样画出来的字只是有点歪，
// 看图的人多半照样认得出，于是这个错会一直留在代码里。这条断言把它变成红灯。
func TestCaptchaGlyphTable(t *testing.T) {
	if len(glyphs) == 0 {
		t.Fatal("字模表空了")
	}
	for ch, rows := range glyphs {
		if len(rows) != glyphRows {
			t.Errorf("字形 %q 行数应为 %d，得到 %d", ch, glyphRows, len(rows))
		}
		for r, row := range rows {
			if len(row) != glyphCols {
				t.Errorf("字形 %q 第 %d 行长度应为 %d，得到 %d（%q）", ch, r, glyphCols, len(row), row)
			}
			for _, c := range row {
				if c != '#' && c != '.' {
					t.Errorf("字形 %q 第 %d 行有非法字符 %q（只允许 '#' 与 '.'）", ch, r, c)
				}
			}
		}
	}
	// 题面构造只用到这些字符；字模缺一个，那位就会画成空白。
	for _, ch := range "0123456789+=?" {
		if _, ok := glyphs[ch]; !ok {
			t.Errorf("字模缺少题面要用的字符 %q", ch)
		}
	}
}

// TestCaptchaConcurrentIssueDistinctIDs 并发出题：ID 互不重复、条数一道不少。
//
// 题面 ID 可预测或可重复的后果很实际：拿别人的 ID 反复 Verify 就能把它烧掉
// （Verify 是一次性的），于是那个人永远过不了校验。这里用"200 道题必须给出
// 200 个不同 ID"来守这条（-race 在本机起不来，见 limit_test.go 的说明）。
func TestCaptchaConcurrentIssueDistinctIDs(t *testing.T) {
	const (
		goroutines = 8
		perG        = 25
		total       = goroutines * perG
	)
	c := NewCaptcha(time.Minute, total)
	now := baseTime()

	ids := make(chan string, total)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				id, svg := c.Issue(now)
				if id == "" || svg == "" {
					return // 记成"发少了"，由下面的断言报出来
				}
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool, total)
	for id := range ids {
		if seen[id] {
			t.Errorf("★ 并发下发出了重复的题面 ID %q", id)
		}
		seen[id] = true
	}
	if len(seen) != total {
		t.Errorf("应当发出 %d 道互不相同的题面，得到 %d", total, len(seen))
	}

	c.mu.Lock()
	pending := len(c.items)
	c.mu.Unlock()
	if pending != total {
		t.Errorf("待校验表里应有 %d 条，得到 %d", total, pending)
	}
}

// TestCaptchaAnswerComparisonAndUnknownID 答案按整数比（trim + 前导 0 不影响），
// 未知/空题面 ID 一律拒绝。
func TestCaptchaAnswerComparisonAndUnknownID(t *testing.T) {
	c := newTestCaptcha(t)
	now := baseTime()

	// 每道题只能用一次，所以每种写法各发一道。
	wrappers := []struct {
		name string
		f    func(string) string
	}{
		{"首尾空白", func(s string) string { return "  " + s + "  " }},
		{"尾部换行", func(s string) string { return s + "\n" }},
		{"前导零", func(s string) string { return "0" + s }},
	}
	for _, w := range wrappers {
		id, _ := c.Issue(now)
		ans := strconv.Itoa(pendingAnswer(t, c, id))
		if !c.Verify(id, w.f(ans), now) {
			t.Errorf("%s：答案 %q 写成 %q 应当放行", w.name, ans, w.f(ans))
		}
	}

	for _, bad := range []string{"", " ", "\t", "c-not-exist"} {
		if c.Verify(bad, "1", now) {
			t.Errorf("题面 ID %q 不存在/为空，必须拒绝", bad)
		}
	}
	// 非数字答案（含空、含算式）一律拒，不该因为 Atoi 失败而 panic。
	id, _ := c.Issue(now)
	for _, bad := range []string{"", "  ", "abc", "7+3", "1.0"} {
		if c.Verify(id, bad, now) {
			t.Errorf("非数字答案 %q 不该放行", bad)
		}
	}
}
