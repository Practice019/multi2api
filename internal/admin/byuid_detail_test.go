// byuid_detail_test.go —— 富化维度（by_model_detail / by_uid_detail / by_provider_detail）的算术验收。
//
// # 这份测试要挡住的四件事
//
//  1. 算术本身：req/ok/fail/tokens/think_tokens/avg_ttfb_ms 每个数都手算比对；
//  2. **口径漂移**：ok+fail 可以小于 req（1xx/3xx 两个都不记）、tokens 只在 >0 时累加
//     （-1 = usage 缺失不当 0）、空 uid 不进 by_uid_detail —— 这些都是"与顶层标量同一口径"的
//     组成部分，任何一条被改掉，下面的断言就要报红；
//  3. **零除**：整桶没有任何 TTFB 样本时 avg_ttfb_ms **必须缺键**，不能是 0；
//  4. **旧字段没被动过**：by_model / by_uid 仍是 map[string]int，by_provider 仍是
//     map[string]providerStat —— 这是"纯加法"的回归守卫（前端与 tools/verify_stats.py 在消费它们）。
package admin

import (
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

// detailFixture 造一批覆盖"计数器边界"的条目：
// 2xx / 3xx / >=400 各状态、tokens=-1（usage 缺失）、负数 think_tokens、
// 空 uid、空 provider、ttfb=0（无样本）。
func detailFixture() []logbuf.Entry {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	return []logbuf.Entry{
		{At: base, Model: "loomy/m", Status: 200, UID: "u1", Provider: "loomy",
			TTFBMS: 100, Tokens: 10, ThinkTokens: 4, TotalMS: 1000},
		{At: base.Add(time.Minute), Model: "loomy/m", Status: 200, UID: "u2", Provider: "loomy",
			TTFBMS: 0, Tokens: 20, TotalMS: 2000},
		{At: base.Add(2 * time.Minute), Model: "wb/m", Status: 500, UID: "u1", Provider: "workbuddy",
			TTFBMS: 300, Tokens: -1, TotalMS: 3000},
		{At: base.Add(3 * time.Minute), Model: "wb/m", Status: 302, UID: "u1", Provider: "",
			TTFBMS: 0, Tokens: -1, ThinkTokens: -7, TotalMS: 10},
		{At: base.Add(4 * time.Minute), Model: "wb/m", Status: 401, UID: "", Provider: "workbuddy",
			TTFBMS: 50, Tokens: 5, ThinkTokens: 1, TotalMS: 20},
	}
}

func detailMap(t *testing.T, out map[string]any, key string) map[string]dimStat {
	t.Helper()
	m, ok := out[key].(map[string]dimStat)
	if !ok {
		t.Fatalf("%s 类型不是 map[string]dimStat，而是 %T（富化字段形状变了）", key, out[key])
	}
	return m
}

// 逐桶手算比对 + 交叉校验（求和的三个维度和必须与顶层标量一致）。
func TestAggregateChatLogDetailArithmetic(t *testing.T) {
	out := aggregateChatLog(detailFixture())

	// ---- 顶层标量（手算基准）----
	if out["total"].(int) != 5 {
		t.Errorf("total=%v，期望 5", out["total"])
	}
	if out["ok"].(int64) != 2 { // 两条 200
		t.Errorf("ok=%v，期望 2", out["ok"])
	}
	if out["fail"].(int64) != 2 { // 500 + 401（302 既不算 ok 也不算 fail）
		t.Errorf("fail=%v，期望 2", out["fail"])
	}
	if out["tokens"].(int64) != 35 { // 10+20+5（两条 -1 跳过）
		t.Errorf("tokens=%v，期望 35", out["tokens"])
	}
	if out["think_tokens"].(int64) != 5 { // 4+1（-7 跳过）
		t.Errorf("think_tokens=%v，期望 5", out["think_tokens"])
	}

	// ---- by_model_detail ----
	byModel := detailMap(t, out, "by_model_detail")
	lm := byModel["loomy/m"]
	if lm.Req != 2 || lm.OK != 2 || lm.Fail != 0 || lm.Tokens != 30 || lm.ThinkTokens != 4 {
		t.Errorf("by_model_detail[loomy/m]=%+v，期望 req2 ok2 fail0 tokens30 think4", lm)
	}
	if lm.TokensRaw != 30 { // 该桶没有负数 ⇒ 原始和 == 剔除负数后的和
		t.Errorf("by_model_detail[loomy/m].tokens_raw=%d，期望 30", lm.TokensRaw)
	}
	if lm.AvgTTFBMS == nil || *lm.AvgTTFBMS != 100 { // (100) / 1：ttfb=0 的那条不进样本
		t.Errorf("by_model_detail[loomy/m].avg_ttfb_ms=%v，期望 100", lm.AvgTTFBMS)
	}
	wbm := byModel["wb/m"]
	if wbm.Req != 3 || wbm.OK != 0 || wbm.Fail != 2 || wbm.Tokens != 5 || wbm.ThinkTokens != 1 {
		t.Errorf("by_model_detail[wb/m]=%+v，期望 req3 ok0 fail2 tokens5 think1", wbm)
	}
	// ⚠ 两个 tokens 口径必须都留着且差值可解释：5(剔负) vs 3(原始) 差 = 两条 -1
	if wbm.TokensRaw != 3 {
		t.Errorf("by_model_detail[wb/m].tokens_raw=%d，期望 3（5 + (-1) + (-1)）", wbm.TokensRaw)
	}
	if wbm.AvgTTFBMS == nil || *wbm.AvgTTFBMS != 175 { // (300+50)/2
		t.Errorf("by_model_detail[wb/m].avg_ttfb_ms=%v，期望 175", wbm.AvgTTFBMS)
	}

	// ---- by_uid_detail ----
	byUID := detailMap(t, out, "by_uid_detail")
	u1 := byUID["u1"]
	if u1.Req != 3 || u1.OK != 1 || u1.Fail != 1 || u1.Tokens != 10 || u1.ThinkTokens != 4 {
		t.Errorf("by_uid_detail[u1]=%+v，期望 req3 ok1 fail1 tokens10 think4", u1)
	}
	if u1.AvgTTFBMS == nil || *u1.AvgTTFBMS != 200 { // (100+300)/2
		t.Errorf("by_uid_detail[u1].avg_ttfb_ms=%v，期望 200", u1.AvgTTFBMS)
	}
	u2 := byUID["u2"]
	if u2.Req != 1 || u2.OK != 1 || u2.Fail != 0 || u2.Tokens != 20 {
		t.Errorf("by_uid_detail[u2]=%+v，期望 req1 ok1 fail0 tokens20", u2)
	}
	if u1.TokensRaw != 8 { // 10 + (-1) + (-1)
		t.Errorf("by_uid_detail[u1].tokens_raw=%d，期望 8", u1.TokensRaw)
	}
	// ⚠ 零除守卫：u2 唯一的条目 ttfb=0（无样本）⇒ 必须缺键，而不是 0。
	if u2.AvgTTFBMS != nil {
		t.Errorf("by_uid_detail[u2].avg_ttfb_ms=%v，期望缺键（不得用 0 冒充'没有样本'）", *u2.AvgTTFBMS)
	}
	// ⚠ 空 uid 的那条不得进 by_uid_detail：否则键集与 by_uid 对不上账
	if len(byUID) != 2 {
		t.Errorf("by_uid_detail 键数=%d，期望 2（空 uid 的那条必须被跳过）", len(byUID))
	}

	// ---- by_provider_detail ----
	byProv := detailMap(t, out, "by_provider_detail")
	lp := byProv["loomy"]
	if lp.Req != 2 || lp.OK != 2 || lp.Fail != 0 || lp.Tokens != 30 {
		t.Errorf("by_provider_detail[loomy]=%+v，期望 req2 ok2 fail0 tokens30", lp)
	}
	wp := byProv["workbuddy"]
	if wp.Req != 2 || wp.OK != 0 || wp.Fail != 2 || wp.Tokens != 5 || wp.ThinkTokens != 1 {
		t.Errorf("by_provider_detail[workbuddy]=%+v，期望 req2 ok0 fail2 tokens5 think1", wp)
	}
	if wp.AvgTTFBMS == nil || *wp.AvgTTFBMS != 175 {
		t.Errorf("by_provider_detail[workbuddy].avg_ttfb_ms=%v，期望 175", wp.AvgTTFBMS)
	}
	// 空 provider 归"未标注"桶（与 by_provider 同一键）
	up := byProv[unlabeledProvider]
	if up.Req != 1 || up.Fail != 0 || up.Tokens != 0 || up.ThinkTokens != 0 {
		t.Errorf("by_provider_detail[%s]=%+v，期望 req1 fail0 tokens0 think0", unlabeledProvider, up)
	}
	if up.AvgTTFBMS != nil {
		t.Errorf("by_provider_detail[%s].avg_ttfb_ms=%v，期望缺键", unlabeledProvider, *up.AvgTTFBMS)
	}

	// ---- 交叉校验：三个维度的求和必须与顶层标量一致（口径一致的机器证明）----
	sum := func(m map[string]dimStat) (req, ok, fail, tok, raw, think int64) {
		for _, d := range m {
			req += d.Req
			ok += d.OK
			fail += d.Fail
			tok += d.Tokens
			raw += d.TokensRaw
			think += d.ThinkTokens
		}
		return
	}
	req, ok, fail, tok, raw, think := sum(byModel)
	if req != 5 || ok != out["ok"].(int64) || fail != out["fail"].(int64) ||
		tok != out["tokens"].(int64) || think != out["think_tokens"].(int64) {
		t.Errorf("by_model_detail 求和=(%d,%d,%d,%d,%d)，与顶层(5,%v,%v,%v,%v)不一致",
			req, ok, fail, tok, think, out["ok"], out["fail"], out["tokens"], out["think_tokens"])
	}
	if raw != 33 { // 35(剔负) + (-1) + (-1)
		t.Errorf("by_model_detail tokens_raw 求和=%d，期望 33", raw)
	}
	// ⚠ by_uid_detail 比 total 少的正是"空 uid"那一条：req 4（不是 5），
	//   它的 5 tokens / 1 fail 也一并落在 by_uid_detail 之外 —— 这是与旧 by_uid
	//   "跳过空 uid"完全一致的刻意口径，不是漏算。
	r1, ok1, f1, t1, raw1, th1 := sum(byUID)
	if r1 != 4 || ok1 != 2 || f1 != 1 || t1 != 30 || raw1 != 28 || th1 != 4 {
		t.Errorf("by_uid_detail 求和=(%d,%d,%d,%d,%d,%d)，期望 (4,2,1,30,28,4)（req 比 total 少 1：空 uid）",
			r1, ok1, f1, t1, raw1, th1)
	}
	r2, ok2, f2, t2, raw2, th2 := sum(byProv)
	if r2 != 5 || ok2 != 2 || f2 != 2 || t2 != 35 || raw2 != 33 || th2 != 5 {
		t.Errorf("by_provider_detail 求和=(%d,%d,%d,%d,%d,%d)，期望 (5,2,2,35,33,5)",
			r2, ok2, f2, t2, raw2, th2)
	}
	// 同一个"token 总量"在三个维度上必须同一个数（口径一致的第二道守卫）
	if raw != raw1+5 || raw != raw2 {
		t.Errorf("tokens_raw 跨维度不一致：by_model=%d by_uid=%d(应+空uid那条5) by_provider=%d",
			raw, raw1, raw2)
	}
}

// 纯加法守卫：旧字段的名字与形状必须一字不变——它们已被消费。
func TestAggregateChatLogLegacyDimsUnchanged(t *testing.T) {
	out := aggregateChatLog(detailFixture())

	byModel, ok := out["by_model"].(map[string]int)
	if !ok {
		t.Fatalf("by_model 已不是 map[string]int，而是 %T —— 前端按它过滤倍率表，这属破坏性改动", out["by_model"])
	}
	if byModel["loomy/m"] != 2 || byModel["wb/m"] != 3 {
		t.Errorf("by_model=%v，期望 loomy/m:2 wb/m:3", byModel)
	}
	byUID, ok := out["by_uid"].(map[string]int)
	if !ok {
		t.Fatalf("by_uid 已不是 map[string]int，而是 %T —— tools/verify_stats.py 在消费它", out["by_uid"])
	}
	if byUID["u1"] != 3 || byUID["u2"] != 1 {
		t.Errorf("by_uid=%v，期望 u1:3 u2:1", byUID)
	}
	byProv, ok := out["by_provider"].(map[string]providerStat)
	if !ok {
		t.Fatalf("by_provider 已不是 map[string]providerStat，而是 %T", out["by_provider"])
	}
	if byProv["loomy"].Calls != 2 || byProv["workbuddy"].Calls != 2 || byProv[unlabeledProvider].Calls != 1 {
		t.Errorf("by_provider=%v，期望 loomy:2 workbuddy:2 未标注:1", byProv)
	}
	// 富化字段是"多出来的一列"，不能顶替旧字段
	for _, k := range []string{"by_model", "by_uid", "by_provider"} {
		if _, exists := out[k]; !exists {
			t.Errorf("旧字段 %s 消失了", k)
		}
	}
}

// 空集：三个富化维度应恒输出为**空对象**（不是 nil、不是缺键）——
// 前端因此只有一种形状要渲染。
func TestAggregateChatLogDetailEmpty(t *testing.T) {
	out := aggregateChatLog(nil)
	for _, k := range []string{"by_model_detail", "by_uid_detail", "by_provider_detail"} {
		m := detailMap(t, out, k)
		if len(m) != 0 {
			t.Errorf("空集时 %s 应为空对象，得到 %v", k, m)
		}
	}
	// 空集下旧字段同样必须仍在（它们的形状不许因为富化而漂）
	if _, ok := out["by_model"].(map[string]int); !ok {
		t.Error("空集时 by_model 形状变了")
	}
	if _, ok := out["by_uid"].(map[string]int); !ok {
		t.Error("空集时 by_uid 形状变了")
	}
}
