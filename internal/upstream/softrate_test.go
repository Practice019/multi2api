// softrate_test.go 429 code=6004 识别与「将在 … 重置」解析。
//
// # 这一层的失败模式
//
// 两种方向都会错，且都不报错：
//
//	漏判（该收窄没收窄）→ 账号被整体冷掉，换模型也不能用（用户以为号废了）
//	误判（不该收窄却收窄）→ 账号被错误豁免 + 提前解冻，继续撞真实限流
//
// 所以两侧都要有断言：既要认出真的 6004，也要**拒绝**长得像但不是的输入。
package upstream

import (
	"testing"
	"time"
)

// TestIsModelRateLimit 6004 的识别（含 JSON 空格/引号容差）。
func TestIsModelRateLimit(t *testing.T) {
	yes := []string{
		`{"code":6004,"msg":"..."}`,
		`{"code": 6004, "msg":"..."}`,   // 冒号后有空格
		`{"code":"6004","msg":"..."}`,   // 码是字符串
		`{"code" : "6004" , "msg":"x"}`, // 键与值两侧都有空格
		`前缀 {"code":6004} 后缀`,           // 被别的文本包裹
	}
	for _, b := range yes {
		if !IsModelRateLimit(b) {
			t.Errorf("应识别为模型级限流：%q", b)
		}
	}

	no := []string{
		``,
		`{"code":6005}`,
		`{"code":60040}`, // 数字边界：前缀匹配不得命中 —— 本用例抓到过一个真 bug
		`{"code":60041}`,
		`{"code":16004}`,
		`{"code":60045}`,
		`{"msg":"6004"}`, // 只有裸数字、没有 code 键 —— 不得命中
		`{"code":429}`,
		`timestamp=1760000000 code=6004`, // 非 JSON 键形态
	}
	for _, b := range no {
		if IsModelRateLimit(b) {
			t.Errorf("不该识别为模型级限流：%q", b)
		}
	}
	// 串尾边界也要能命中（"6004" 后没有任何字符）。
	if !IsModelRateLimit(`{"code":6004}`) {
		t.Error("串尾形态应命中")
	}
	// ⚠ 刻意**不**断言 `{"code":6004}0` 这类"数字紧跟闭合括号"的输入：
	// 那不是数字延续，而是两段合法 token 相邻（真到这一步 JSON 已经畸形）。
	// 数字边界的目的是挡住 `60040` 这种**同一个数字**的前缀匹配，
	// 而不是要求匹配吃掉匹配之后的任意字符。
}

// TestIsModelRateLimitDoesNotMatchBareNumber 裸数字 6004 **不得**命中。
//
// # 为什么这是硬判据
//
// 错误体里 6004 可能出现在任何地方（请求 id、时间戳、别的字段）。
// 只搜裸数字会把一次**账号级**限流误判成模型级 ——
// 后果是账号被错误豁免 + 冷却被收窄，继续撞真实限流。
//
// 注意这与 sanitize.go 里"上游按裸数字 11128 拦截"是**相反**的要求：
// 那里上游的判据是裸数字，我们必须按裸数字改写；
// 这里是**我们**的判据，必须按键名找。两件事不能混。
func TestIsModelRateLimitDoesNotMatchBareNumber(t *testing.T) {
	for _, b := range []string{
		`{"requestId":"6004"}`,
		`{"ts":17600006004}`,
		`错误 6004`,
	} {
		if IsModelRateLimit(b) {
			t.Errorf("裸数字 6004 不该被判成模型级限流：%q", b)
		}
	}
}

// TestParseSoftRateReset 正常文案解析（按 UTC+8 解释）。
func TestParseSoftRateReset(t *testing.T) {
	body := `{"code":6004,"msg":"当前模型使用量已达上限，将在 2026-09-15 09:30:00 重置"}`
	got, ok := ParseSoftRateReset(body)
	if !ok {
		t.Fatalf("应解析成功：%q", body)
	}
	want := time.Date(2026, 9, 15, 9, 30, 0, 0, SoftRateResetLoc())
	if !got.Equal(want) {
		t.Errorf("解析出 %v，期望 %v（UTC+8 墙钟）", got, want)
	}
}

// TestParseSoftRateResetTimezoneIsFixedUTC8 时区固定 UTC+8，与宿主 TZ 无关。
//
// 用本地时区解释会在 UTC 容器里整整偏 8 小时 —— 冷却要么提前结束
// （继续撞 429），要么多冷 8 小时（账号白白闲置）。
func TestParseSoftRateResetTimezoneIsFixedUTC8(t *testing.T) {
	body := `{"code":6004,"msg":"将在 2026-09-15 09:30:00 重置"}`
	got, ok := ParseSoftRateReset(body)
	if !ok {
		t.Fatal("应解析成功")
	}
	// 用 UTC 表达同一个瞬时：09:30 CST = 01:30 UTC。
	wantUTC := time.Date(2026, 9, 15, 1, 30, 0, 0, time.UTC)
	if !got.UTC().Equal(wantUTC) {
		t.Errorf("解析出 %v（UTC %v），期望 UTC %v —— 时区口径不对",
			got, got.UTC(), wantUTC)
	}
}

// TestParseSoftRateResetWithTZSuffix 文案带 " UTC+8" 后缀也能解析。
func TestParseSoftRateResetWithTZSuffix(t *testing.T) {
	body := `{"code":6004,"msg":"将在 2026-09-15 09:30:00 UTC+8 重置"}`
	got, ok := ParseSoftRateReset(body)
	if !ok {
		t.Fatal("带 UTC+8 后缀应仍能解析")
	}
	want := time.Date(2026, 9, 15, 9, 30, 0, 0, SoftRateResetLoc())
	if !got.Equal(want) {
		t.Errorf("解析出 %v，期望 %v", got, want)
	}
}

// TestParseSoftRateResetRejectsNon6004 非 6004 一律不解析，**即使文案里有"重置"**。
//
// # 为什么这条是硬判据
//
// 别处的限流提示也可能带"重置"字样（例如通用限流说明），
// 那种重置时间**没有冷却语义**。解析出来会让冷却被错误收窄 ——
// 把一个本该账号级冷却的问题当成模型级豁免掉。
func TestParseSoftRateResetRejectsNon6004(t *testing.T) {
	bodies := []string{
		`{"code":11140,"msg":"请求过于频繁，将在 2026-09-15 09:30:00 重置"}`,
		`{"code":429,"msg":"将在 2026-09-15 09:30:00 重置"}`,
		`{"msg":"将在 2026-09-15 09:30:00 重置"}`, // 没有 code
		`{"code":6004,"msg":"当前模型使用量已达上限"}`,   // 是 6004 但没有时间文案
		`{"code":6004,"msg":"将在 明天 重置"}`,      // 时间格式不对
		``,
	}
	for _, b := range bodies {
		if ts, ok := ParseSoftRateReset(b); ok {
			t.Errorf("不该解析出时刻：%q → %v", b, ts)
		}
	}
}

// TestParseSoftRateResetNonGreedyCapture 非贪婪捕获：文案后面还有内容也能解析。
//
// 用贪婪匹配会把后续内容一起吞进时间串，导致 Parse 失败 ——
// 那会让"能解析的文案"静默退化成"永远退回指数退避"。
func TestParseSoftRateResetNonGreedyCapture(t *testing.T) {
	body := `{"code":6004,"msg":"当前模型使用量已达上限，将在 2026-09-15 09:30:00 重置，届时可继续使用"}`
	got, ok := ParseSoftRateReset(body)
	if !ok {
		t.Fatalf("文案后面还有内容时应仍能解析：%q", body)
	}
	want := time.Date(2026, 9, 15, 9, 30, 0, 0, SoftRateResetLoc())
	if !got.Equal(want) {
		t.Errorf("解析出 %v，期望 %v", got, want)
	}
}

// TestParseSoftRateResetNeverPanics 任意输入都不得 panic。
//
// 它在出站循环的每次软限流上被调用 —— 一次 panic 会带崩整个请求。
func TestParseSoftRateResetNeverPanics(t *testing.T) {
	inputs := []string{
		"", "{", "将在", "将在  重置", "将在 重置", "将在 xx 重置",
		`{"code":6004,"msg":"将在 9999-99-99 99:99:99 重置"}`,
		"将在 " + string(make([]byte, 10000)) + " 重置",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("输入 %q 触发 panic: %v", in, r)
				}
			}()
			_, _ = ParseSoftRateReset(in)
			_ = IsModelRateLimit(in)
		}()
	}
}
