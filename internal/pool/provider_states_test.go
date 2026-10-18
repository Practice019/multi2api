package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// ⚠ 钉住「上游维度能被看到」这件事（2026-10-18 加）。
//
// # 为什么值得一条测试
//
// 上游级熔断（provState）此前只影响出口层的状态码与判决留痕，**界面上完全看不到** ⇒
// 用户遇到"整条上游不通"时，控制台里一切正常，只能靠日志反推。
// 这条测试守住三个容易在重构里丢掉的语义：
//
//	① 没有失败记录的上游**不出现**（列表为空 = 一切正常，而不是"未知"）
//	② 空的时候返回**空切片**而不是 nil（JSON 是 `[]`，前端不用判 null）
//	③ 达阈值后 Tripped 为真、并带上截止时刻与已熔断次数
func TestProviderStatesEmptyIsNotNil(t *testing.T) {
	p := New("")
	got := p.ProviderStates()
	if got == nil {
		t.Fatal("从未失败时应返回空切片而不是 nil —— 否则 JSON 里是 null，前端要多一处判空")
	}
	if len(got) != 0 {
		t.Fatalf("没有失败记录时不该有项，得到 %+v", got)
	}
}

func TestProviderStatesReflectsBreaker(t *testing.T) {
	p := New("")
	p.upsertLockedFor("loomy", &auth.Auth{UID: "u1"})

	// 阈值默认 3：喂 2 次还不该熔断，但那两次失败要能被看到
	p.NoteError("u1")
	p.NoteError("u1")
	got := p.ProviderStates()
	if len(got) != 1 {
		t.Fatalf("有失败记录就该出现，得到 %+v", got)
	}
	if got[0].Provider != "loomy" {
		t.Errorf("Provider = %q，想要 loomy", got[0].Provider)
	}
	if got[0].Tripped {
		t.Error("2 次失败（阈值 3）不该显示为已熔断 —— 否则界面会误报上游故障")
	}
	if got[0].Fails != 2 {
		t.Errorf("Fails = %d，想要 2（界面靠它显示「还差几次熔断」）", got[0].Fails)
	}

	// 第 3 次 ⇒ 熔断
	p.NoteError("u1")
	got = p.ProviderStates()
	if !got[0].Tripped {
		t.Error("3 次失败后应显示为已熔断")
	}
	if got[0].BreakerUntil.IsZero() {
		t.Error("熔断时应带上截止时刻（界面要显示「还有多久恢复」）")
	}
	if got[0].RetryCount != 1 {
		t.Errorf("RetryCount = %d，想要 1", got[0].RetryCount)
	}

	// ⚠ 一次成功 ⇒ 失败线清零，界面上这条记录应回到"未熔断"
	p.NoteSuccess("u1")
	got = p.ProviderStates()
	if len(got) != 1 || got[0].Tripped || got[0].Fails != 0 {
		t.Errorf("一次成功后应清零，得到 %+v", got)
	}
}
