package admin

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestQuotaRefreshIsConcurrent 额度刷新必须**真并发**且**有上限**（T-436）。
//
// # 用户需求
//
//	「账号池刷新本上游额度的按钮，它应该是没有并发的吧？我希望统一设置并发为20。
//	  因为我现在感觉特别慢。」
//
// 改造前：`refreshQuotas` 完全串行（106 个号 × 每个约 1 秒 ≈ 106 秒）。
//
// # ⚠ 这条测试的局限（如实登记）
//
// 它是**源码级**断言，不是行为测试 —— 它守的是"并发结构与上限常量存在"，
// **证明不了**"峰值确实 ≤ 20"与"总耗时确实下降"。
// 那两条需要给 Handler 造 Pool/Registry 的假实现（工作量大），
// 登记为待办（见 T-436 回报的未决）：
//
//	· 行为测试 A：N 个 Sleep(100ms) 的假 QuotaExt ⇒ 总耗时 < N/2 × 100ms
//	· 行为测试 B：记录在飞峰值 ⇒ ≤ quotaRefreshConcurrency
//	· 行为测试 C：并发版与串行版的 Updated/Unknown/Failed/Skipped **逐字相同**
//
// ⚠ 反面证据（这条测试**会**抓到的）：把并发改回串行、或删掉信号量 ⇒ 红。
func TestQuotaRefreshIsConcurrent(t *testing.T) {
	// ① 并发上限是 20（用户明确要求的值）
	if quotaRefreshConcurrency != 20 {
		t.Errorf("quotaRefreshConcurrency = %d，用户明确要求 **20**"+
			"（「我希望统一设置并发为20。因为我现在感觉特别慢。」）", quotaRefreshConcurrency)
	}
	b, err := os.ReadFile("quota_refresh.go")
	if err != nil {
		t.Fatalf("读 quota_refresh.go 失败：%v", err)
	}
	src := string(b)
	// ② 信号量形态（带缓冲的 channel 当令牌桶）—— 没有它 = 无上限并发 = 风控事故
	if !strings.Contains(src, "make(chan struct{}, quotaRefreshConcurrency)") {
		t.Error("找不到信号量 `make(chan struct{}, quotaRefreshConcurrency)` —— " +
			"没有它就会给每个 uid 起一个 goroutine（106 个并发请求打上游），那是风控事故")
	}
	// ③ 必须等全部 worker 收工（否则计数/写回会读到半成品）
	if !strings.Contains(src, "wg.Wait()") {
		t.Error("找不到 wg.Wait() —— 不等 worker 结束就聚合，会漏计数")
	}
	// ④ 结果按**下标**写回（无锁、无竞态、顺序无关）——
	//    如果改成 append 共享切片，就是数据竞争
	if !regexp.MustCompile(`results\[i\]\s*=\s*quotaRes\{`).MatchString(src) {
		t.Error("并发结果不是按**下标**写回的（应 `results[i] = quotaRes{…}`）—— " +
			"用 append 共享切片就是数据竞争（go test -race 会红）")
	}
	// ⑤ 聚合仍在串行段（计数与写回不能进 worker）
	if !strings.Contains(src, "第三阶段（**串行**）") {
		t.Error("找不到「第三阶段（串行）」的聚合段 —— 计数与 SetQuota 写池" +
			"不是并发安全的，必须留在主 goroutine 里（无锁设计的前提）")
	}
}
