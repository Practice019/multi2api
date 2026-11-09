package gateway

import (
	"strings"
	"testing"
)

// TestShortUIDDistinguishesWithinUpstream 共享 ShortUID 的三条 seam（T-403）。
//
// # 为什么这条测试是这次改动的全部目的
//
// 用户原话："uid打印全了"。日志里 uid 的唯一用途就是让人**唯一确定**是哪个号，
// 所以判据不是"看起来更长了"，而是：**同一上游的一批号里，压缩后两两不同**。
//
// ⚠ 跨上游**允许**相同（前缀已经把上游区分开了）；同一上游内**必须**不同。
// ⚠ 测试数据用各上游的**真实形状**（不是编的），所以它同时是一份"各上游 uid 长什么样"的档案。
func TestShortUIDDistinguishesWithinUpstream(t *testing.T) {
	// 每个上游一组：同一天/同一秒创建的号 —— 这些前缀完全相同，只有区分位不同。
	groups := map[string][]string{
		"loomy": {
			"loomy-260915174611208423", "loomy-260915174611301268",
			"loomy-260915174611104551", "loomy-260915174611409672",
			"loomy-260915174611502880", "loomy-260927001031181940",
		},
		"monkeycode": {
			"monkeycode-002d3463-53d0-4a11-9c1a-77e01b2c8f33",
			"monkeycode-002d3463-53d0-4a11-9c1a-77e01b2c8f44",
			"monkeycode-002d3463-53d0-4a11-9c1a-88f01b2c8f55",
			"monkeycode-002d3464-11ab-4c22-bd33-99f01b2c8f66",
		},
		"zcode": {
			"zcode-79e98e60-5be9-4f21-8a77-112233445566",
			"zcode-79e98e60-5be9-4f21-8a77-112233445577",
			"zcode-79e98e61-6cf0-5a32-9b88-223344556688",
		},
		"codar": {
			"user_15083583411_7985", "user_15083583411_7986",
			"user_15083583411_8123", "user_15083583412_7985",
		},
		"trae": {
			"3929003642848586", "3929003642848587", "3929003642849001",
			"3929003642851234", "3929003643000001",
		},
		"cline": {
			"usr-01M3MJDCGSD05W4Y83JQC71XCT",
			"usr-01M3MJDCGSD05W4Y83JQC71XCU",
			"usr-01M3MJDCGSD05W4Y83JQD82YDT",
			"usr-01M3MJDCGSD05W4Y83JQE93ZEU",
		},
		"workbuddy": {
			"0234c548-963c-4d43-8a11-112233445566",
			"0234c548-963c-4d43-8a11-112233445577",
			"0234c548-963c-4d43-8a11-998877665544",
		},
	}
	total := 0
	rows := make([]string, 0, 64)
	for upstream, uids := range groups {
		seen := map[string]string{}
		for _, u := range uids {
			total++
			got := ShortUID(u)
			if prev, dup := seen[got]; dup {
				t.Errorf("[%s] ShortUID 碰撞：%q 与 %q 都压成 %q —— 日志里无法区分是哪个号", upstream, prev, u, got)
			}
			seen[got] = u
			rows = append(rows, "	"+upstream+"	"+u+"	"+got)
		}
	}
	if total < 20 {
		t.Errorf("样本只有 %d 个（要求 ≥20，跨 7 个上游的真实形状）", total)
	}
	t.Logf("压缩对照表（%d 条）：\n%s", total, strings.Join(rows, "\n"))
}

// TestShortUIDShortIDsPassThrough 短 uid（≤12）必须原样返回。
//
// 为什么单独钉：短 id 是本项目里真实存在的形态（lobsterai 的 `103668`、raccoon 的 `7497524`），
// 对它们做任何截断都只会**丢掉全部信息**（截 6 位出来只剩 `1036…`，比原值更差）。
func TestShortUIDShortIDsPassThrough(t *testing.T) {
	for _, u := range []string{"103668", "7497524", "abc", "", "abcdefghijkl", "  ", "loomy-260915"} {
		if got := ShortUID(u); got != u {
			t.Errorf("短 uid 被改动了：%q → %q（≤12 字符必须原样返回）", u, got)
		}
	}
}

// TestShortUIDKeepsTailAndHasNoOverlap 形态约束：必须含尾部 8 位、长度可控、不出现重叠。
func TestShortUIDKeepsTailAndHasNoOverlap(t *testing.T) {
	u := "loomy-260915174611208423"
	got := ShortUID(u)
	rs := []rune(u)
	if !strings.HasSuffix(got, string(rs[len(rs)-8:])) {
		t.Errorf("压缩后看不到尾部 8 位（尾部才是区分位）：%q", got)
	}
	if n := len([]rune(got)); n > 16 {
		t.Errorf("压缩后 %d 字符，太长（日志一行要读得下去）：%q", n, got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("压缩后没有省略号，看不出被截过：%q", got)
	}
	// 极短但 >12 的边界：head 必须被夹住，不能出现 head+8 > len 而重叠
	edge := "abcdefghijklm" // 13 字符
	e := ShortUID(edge)
	if len([]rune(e)) > len([]rune(edge)) {
		t.Errorf("压缩后反而变长：%q → %q", edge, e)
	}
}
