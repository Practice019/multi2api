package server

import (
	"strings"
	"testing"
)

// TestAppendAttemptTrace 失败留痕的形状（用户实测痛点：日志看不出试了几个号）。
func TestAppendAttemptTrace(t *testing.T) {
	cases := []struct {
		name     string
		decision string
		attempts int
		spentMs  int64
		lastFail string
		want     string
	}{
		{"原 decision 为空", "", 3, 118000, "loomy server 408", "attempts=3 spentMs=118000 last=loomy server 408"},
		{"原 decision 非空", "upstream_5xx:system", 2, 40000, "loomy server 408", "upstream_5xx:system attempts=2 spentMs=40000 last=loomy server 408"},
		{"last 为空", "", 1, 5000, "", "attempts=1 spentMs=5000"},
		{"attempts=0 不追加", "x:y", 0, 999, "z", "x:y"},
	}
	for _, c := range cases {
		got := appendAttemptTrace(c.decision, c.attempts, c.spentMs, c.lastFail)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
		if c.attempts > 0 && strings.HasPrefix(got, " ") {
			t.Errorf("%s: 不该有前导空白: %q", c.name, got)
		}
	}
}

// TestFailBriefShape 一句话摘要含上游名与错误类别（可 grep）。
func TestFailBriefShape(t *testing.T) {
	if got := failBrief("loomy", 0, nil); got == "" {
		t.Error("不该返回空串：空串在日志里与没记录不可区分")
	}
	// kind=0（ErrKindNone）不得被写成一个叫 "none" 的分类 —— 那等于伪造分类。
	if got := failBrief("", 0, nil); got != "unknown_failure" {
		t.Errorf("什么都没挖到时应给明确占位，得到 %q", got)
	}
	got := failBrief("loomy", 0, nil)
	if !strings.Contains(got, "loomy") {
		t.Errorf("应含上游名: %q", got)
	}
}
