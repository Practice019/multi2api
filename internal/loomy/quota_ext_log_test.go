package loomy

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/logx"
)

// captureLog 把 log 输出接到 buffer，跑完恢复（日志文案测试的标准做法）。
func captureLog(fn func()) string {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	fn()
	// ⚠ 2026-11-12：这些行现在走 logx.Coalesce（**窗口折叠**，不是立即输出）⇒
	//   读之前必须先 flush，否则测试看到的是空串（假红）。
	for _, line := range logx.FlushCoalesced() {
		buf.WriteString(line + "\n")
	}
	return buf.String()
}

// TestQuotaFailReasonTiers 额度查询失败必须**按类型分档**（T-402）。
//
// # 为什么（用户原话："这个查询失败，其实有可能是网络的原因"）
//
// 改造前所有失败都打成同一句"积分网关查额度失败" ⇒ **网络抖动**与**账号被停用**
// 在日志里长得一模一样 ⇒ 用户会去删一个其实只是网络抖了一下的健康号。
func TestQuotaFailReasonTiers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"超时", context.DeadlineExceeded, "超时"},
		{"网络失败", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, "网络失败"},
		{"上游拒绝(业务码)", errors.New("loomy: 积分接口 /points/records?pageNo=1 返回 100002: 登录已失效，请重新登录"), "上游拒绝"},
		{"回执非 JSON", errors.New("loomy: 积分接口回执不是合法 JSON（HTTP 200，前 120 字节：<html>）"), "回执异常"},
		{"缺 balance 字段", errors.New("loomy: 积分账本回执里没有 balance/availableBalance 字段"), "回执异常"},
	}
	for _, c := range cases {
		got, hint := quotaFailReason(c.err)
		if got != c.want {
			t.Errorf("%s：档位 = %q，期望 %q", c.name, got, c.want)
		}
		if strings.TrimSpace(hint) == "" {
			t.Errorf("%s：缺 hint —— 用户需要知道「要不要管、会不会自动重试」", c.name)
		}
	}
	if r, _ := quotaFailReason(nil); r == "" {
		t.Error("nil error 也要给一档（不能返回空串，否则日志里会出现「（）」）")
	}
}

// TestLogQuotaFailFormatNoDoubledPrefix 失败日志的**文案形态**（T-402）。
//
// 三个要点：
//  1. 不得出现 `loomy: loomy:`（err 自身带前缀 + 日志前缀又带一遍）
//  2. 必须含档位词（网络失败 / 上游拒绝 / 回执异常 / 超时）
//  3. 不得再写「（回落到本机缓存）」—— 那条路径紧接着返回"额度未知"，并没有回落成功
func TestLogQuotaFailFormatNoDoubledPrefix(t *testing.T) {
	upstreamErr := errors.New("loomy: 积分接口 /points/records?pageNo=1&pageSize=1 返回 100003: 账号已被停用")
	out := captureLog(func() { logQuotaFail("t402-upstream", upstreamErr) })

	if strings.Contains(out, "loomy: loomy:") {
		t.Errorf("前缀重复了两遍（loomy: loomy:）：%q", strings.TrimSpace(out))
	}
	if !strings.Contains(out, "上游拒绝") {
		t.Errorf("没有按类型分档（应含「上游拒绝」）：%q", strings.TrimSpace(out))
	}
	if !strings.Contains(out, "100003") {
		t.Errorf("没有带上游业务码（用户要靠它去查上游文档）：%q", strings.TrimSpace(out))
	}
	if !strings.Contains(out, "账号已被停用") {
		t.Errorf("没有带上游原文：%q", strings.TrimSpace(out))
	}
	if strings.Contains(out, "回落到本机缓存") {
		t.Errorf("仍写着假的「（回落到本机缓存）」—— 这条路径并没有回落成功：%q", strings.TrimSpace(out))
	}
	// ⚠ 关于"URL 是常量噪音"：`?pageNo=1&pageSize=1` 出现在 **pointsCall 造的 error 原文**里，
	// 日志只是忠实回显它 ⇒ 要清掉得改 pointsCall 的文案（不在本轮三条改动内）。
	// 这里**不断言**它会消失（那会把"忠实回显上游原文"判成错），只在报告里记为观察项。

	netOut := captureLog(func() {
		logQuotaFail("t402-net", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")})
	})
	if !strings.Contains(netOut, "网络失败") {
		t.Errorf("网络失败没有单独分档（用户明确提醒过：失败可能是网络原因）：%q", strings.TrimSpace(netOut))
	}
	if !strings.Contains(netOut, "稍后会自动重试") {
		t.Errorf("网络失败那档没告诉用户「会不会自愈」：%q", strings.TrimSpace(netOut))
	}
}

// TestNoteCacheMissAggregates 3 个号的缓存不匹配 ⇒ 汇总**最多 1 条**（T-402）。
//
// 「本机缓存只对本机登录的那一个账号有效」是**设计内正常情况**，不是异常。
// 116 个号 × 每 5 分钟 = 每天最多 3 万条纯噪音（用户抱怨"可读性不好"的正是这类）。
func TestNoteCacheMissAggregates(t *testing.T) {
	cacheMiss.mu.Lock()
	cacheMiss.n = 0
	cacheMiss.last = time.Time{}
	cacheMiss.mu.Unlock()

	out := captureLog(func() {
		noteCacheMiss() // 第 1 个号 ⇒ 打汇总
		noteCacheMiss() // 第 2 个号 ⇒ 只计数
		noteCacheMiss() // 第 3 个号 ⇒ 只计数
	})
	n := strings.Count(out, "只能靠积分网关查")
	if n > 1 {
		t.Errorf("3 个号打出了 %d 条汇总，应 ≤ 1（每个号一条 = 每天 3 万条噪音）：%q", n, strings.TrimSpace(out))
	}
	if n == 0 {
		t.Error("一条都没打 ⇒ 该说明的情况没说明（不是「删掉了」而是「改死了」）")
	}
	if !strings.Contains(out, "设计内正常情况") {
		t.Errorf("汇总文案没有说明这是正常情况，用户会以为坏了：%q", strings.TrimSpace(out))
	}
}

// TestNoPerUIDCacheMismatchLog 源码里不得再有"逐号打印"的缓存不匹配日志（T-402）。
//
// 被删掉的那条原文是：
//
//	loomy: 额度未知 —— 本机客户端登录的是 %s，而被问的是 %s（缓存只对本机登录的那个账号有效）
//
// 它同时犯了两个错：① 「被问的」不是中文，且用户不知道存在本地客户端
// ② 这是设计内正常情况，却每轮每个号都打一条
func TestNoPerUIDCacheMismatchLog(t *testing.T) {
	b, err := os.ReadFile("quota_ext.go")
	if err != nil {
		t.Fatalf("读 quota_ext.go 失败：%v", err)
	}
	src := string(b)
	if strings.Contains(src, "而被问的是") {
		t.Error("仍存在逐号打印的「…而被问的是…」日志（看不懂，且每轮每号一条）")
	}
	if !strings.Contains(src, "noteCacheMiss()") {
		t.Error("缓存不匹配没有走汇总（缺 noteCacheMiss() 调用）")
	}
	if strings.Contains(src, "log.Printf(\"loomy: 额度未知 —— 本机客户端登录的是") {
		t.Error("那条逐号日志还在")
	}
}
