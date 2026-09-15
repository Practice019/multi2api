package loomy

import (
	"errors"
	"strings"
	"testing"
)

// TestTranslateBindError 邀请码绑定错误的翻译（用户报"填码总有错"的修复）。
func TestTranslateBindError(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"自绑", "loomy: 积分接口 /points/activation 返回 100001: 请求参数错误", "不能绑定自己账号生成的邀请码"},
		{"不存在", "loomy: 积分接口 /points/activation 返回 200002: 邀请码不存在", "邀请码不存在"},
		{"已用/失效", "loomy: 积分接口 /points/activation 返回 200003: 邀请码不可用", "邀请码不可用"},
		{"其它错误保留原文", "loomy: 调用积分接口失败: dial tcp timeout", "dial tcp timeout"},
	}
	for _, c := range cases {
		got := translateBindError(errors.New(c.in))
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q 应包含 %q", c.name, got, c.want)
		}
	}
	if got := translateBindError(nil); got == "" {
		t.Error("nil 错误也要有文案")
	}
}
