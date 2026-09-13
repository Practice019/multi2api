package gateway

import (
	"context"
	"testing"
)

// T2：额度扩展点的**不变量**测试。
//
// # 为什么这些断言值得单写一个文件
//
// 整个 T2 要修的是一个"把未知伪装成确定的零"的缺陷。
// 这个缺陷的防线**就是 HasData 这一个字段** —— 它在三处独立出现
//（gateway.QuotaView / pool.QuotaView / 落盘 JSON），
// 任何一处的语义漂移都会让缺陷复发。
//
// 所以这里把"三态必须可区分"钉成测试，而不是只写在注释里。

// TestCreditsQuotaCarriesHasData 「确实是 0」必须与「不知道」可区分。
//
// 构造函数存在的意义就在这条：让上游不必手写结构体字面量。
// 手写极易漏掉 HasData —— 漏了之后 `CreditsQuota(0)` 会退化成"未知"，
// 于是"这个号真的没额度了"显示成 `—`（另一个方向的错）。
func TestCreditsQuotaCarriesHasData(t *testing.T) {
	// 合法零：查到就是 0，必须显示成 0（不是 —）。
	zero := CreditsQuota(0)
	if !zero.HasData {
		t.Error("CreditsQuota(0) 必须 HasData=true —— 「确实是 0」与「不知道」必须能区分")
	}
	if zero.Remaining != 0 {
		t.Errorf("Remaining 应为 0，得到 %d", zero.Remaining)
	}
	if zero.Kind != QuotaKindCredits {
		t.Errorf("Kind 应为 %q，得到 %q", QuotaKindCredits, zero.Kind)
	}

	// 正常值。
	rich := CreditsQuota(7474)
	if !rich.HasData || rich.Remaining != 7474 {
		t.Errorf("CreditsQuota(7474) 形态不对：%+v", rich)
	}
}

// TestUnknownQuotaIsUnknown 未知额度的规范表示。
func TestUnknownQuotaIsUnknown(t *testing.T) {
	unk := UnknownQuota()
	if unk.HasData {
		t.Error("UnknownQuota 不该 HasData=true")
	}
	if unk.Remaining != 0 {
		t.Errorf("未知额度不该带数值（会被读成确定值）：%d", unk.Remaining)
	}
	if unk.Kind != "" {
		t.Errorf("未知额度不该自称某种形态：%q", unk.Kind)
	}
}

// TestPerModelQuotaEmptyMeansUnknown 空的 per_model 视为未知而非零额度。
//
// 一个没有任何模型条目的 per_model 不携带信息；把它当成"可信的 0"
// 会让账号被静默降权（pool 侧同一条判据见 restoreQuota 的 hasContent）。
func TestPerModelQuotaEmptyMeansUnknown(t *testing.T) {
	if qv := PerModelQuota(nil); qv.HasData {
		t.Errorf("空表应视为未知，得到 %+v", qv)
	}
	if qv := PerModelQuota(map[string]int64{}); qv.HasData {
		t.Errorf("空表应视为未知，得到 %+v", qv)
	}
	// 非空表（哪怕值是 0）是有数据的 —— 某个模型确实配额为 0。
	if qv := PerModelQuota(map[string]int64{"m": 0}); !qv.HasData {
		t.Errorf("非空表应视为有数据，得到 %+v", qv)
	}
}

// TestQuotaKindConstantsMatchPool 常量取值必须与 pool 侧逐字一致。
//
// # 为什么需要这条
//
// gateway 不能依赖 internal/pool（它是零核心依赖的接缝），
// 所以两边各定义了一份常量。**两份字面量必须相同** ——
// 否则 admin 的 toPoolQuota 直译过去之后，pool 会认不出形态，
// 落到 `default:` 分支当成 credits 处理（静默的行为漂移，不报错）。
//
// 这里写死期望值：改任何一边而不同步，测试立刻红。
// 比"加一层运行时校验"便宜得多。
func TestQuotaKindConstantsMatchPool(t *testing.T) {
	// ⚠ 这三个值必须与 internal/pool/quota.go 的 QuotaKind* 完全一致。
	// 改 pool 那边时**必须**同步改这里（这正是本测试的作用：逼你想起）。
	want := map[string]string{
		"QuotaKindCredits":   "credits",
		"QuotaKindPerModel":  "per_model",
		"QuotaKindUnlimited": "unlimited",
	}
	got := map[string]string{
		"QuotaKindCredits":   QuotaKindCredits,
		"QuotaKindPerModel":  QuotaKindPerModel,
		"QuotaKindUnlimited": QuotaKindUnlimited,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s 漂移了：gateway 是 %q，期望 %q（pool 侧定义的那份）", k, got[k], w)
		}
	}
}

// TestQuotaExtIsDiscoverable 扩展点必须能被 ExtOf 认出。
//
// 这条守着"接口选型没选错"：QuotaExt 的方法集必须只有一个方法，
// 这样任何想实现它的上游都能轻松实现（深模块原则，见 provider.go 的注释）。
func TestQuotaExtIsDiscoverable(t *testing.T) {
	if _, ok := ExtOf[QuotaExt](nil); ok {
		t.Error("nil Provider 不该被认出实现了 QuotaExt（ExtOf 有 nil 保护）")
	}

	// 一个实现了 QuotaExt 的最小 Provider 必须能被断言出来。
	var p Provider = &quotaExtStub{}
	ext, ok := ExtOf[QuotaExt](p)
	if !ok {
		t.Fatal("实现了 QuotaExt 的 Provider 应能被 ExtOf 发现")
	}
	qv, _ := ext.RefreshQuota("u1")
	if !qv.HasData || qv.Remaining != 42 {
		t.Errorf("扩展点调用结果不对：%+v", qv)
	}
}

// quotaExtStub 是最小的 Provider + QuotaExt 实现。
type quotaExtStub struct{}

func (s *quotaExtStub) ID() string               { return "stub" }
func (s *quotaExtStub) Caps() Capability         { return CapChat }
func (s *quotaExtStub) Chat(ctx context.Context, c Credential, b []byte) (ChatStream, error) {
	return ChatStream{}, nil
}
func (s *quotaExtStub) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return nil, nil
}
func (s *quotaExtStub) RefreshQuota(uid string) (QuotaView, bool) {
	return CreditsQuota(42), true
}
