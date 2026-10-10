// refresh_pairing_test.go 跨上游不变量：**RefreshSkew 回正数 ⇒ 必须实现 CredentialRefresher**。
//
// # 这条守卫是从一次真实报障长出来的
//
// 用户报：minimax 的 Token 列一直显示「已过期」，账号不能用 —— 而它本应自动续期。
//
// 查下去发现两个新上游都掉了链子，而且**掉的方式相反**：
//
//	minimax  实现了 RefreshSkewExt（"剩 5 分钟该刷了"），**漏了** CredentialRefresher
//	zcode    实现了 RefreshSkewExt（正数 10 分钟），但 JWT 官方**根本没有** refresh 接口
//
// 两者的共同点是最要命的：**这条链断掉时不报任何错**。
//
//  1. 核心问 needsRefreshVia → 上游说"该刷了" → true
//  2. 核心调 refreshCredential → ExtOf[CredentialRefresher] **失败**
//  3. handler.go 把它解释成「该上游的凭证不需要刷新」→ **静默跳过**
//  4. token 就一直过期着，日志零字
//
// 第 3 步那条语义本身没错（纯 API Key 的上游确实不用刷），但它让
// 「我承诺要刷却没实现」与「我本来就不用刷」**共用同一个出口**。
//
// # 判据
//
//	(skew > 0, ok=true)  ⇒  必须实现 CredentialRefresher
//	(0, true)            →  合法（"明确不需要提前刷"，zcode 修好后正是这个形态）
//	(_, false)           →  合法（"这份凭证没有窗口信息"，核心用自己的兜底）
//
// # ⚠ 为什么必须传**各上游自己的**凭证
//
// 我第一版传 `gateway.Credential{Provider: id}`（Secret 为 nil）。结果每个上游
// 的类型断言都失败 ⇒ 全回 ok=false ⇒ 每条子用例都 skip ——
// 这条守卫因此**永远不会红**。而"抓不住目标的守卫"比没有守卫更糟：
// 它给人"已经防住了"的错觉。所以这里为出过事的两个上游构造真实凭证，
// 并用 TestRefreshPairingCoversTheNewUpstreams 钉住"必须真的检查到"。
package main

import (
	"fmt"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/minimax"
	"workbuddy2api/internal/zcode"
)

// refreshableCredOf 给指定上游造一份**会触发续期判定**的凭证。
//
// 造不出来的返回 ok=false（该上游的子用例跳过，并被覆盖断言点名）。
func refreshableCredOf(id string) (gateway.Credential, bool) {
	now := time.Now()
	switch id {
	case "minimax":
		// 寿命 1 小时、已发证 59 分钟 ⇒ 无论按寿命一半还是固定窗口都该刷了。
		return gateway.Credential{
			Provider: id,
			UID:      "pairing",
			Secret: &minimax.Auth{
				AccessToken:  "mmoat_pairing",
				RefreshToken: "mmort_pairing",
				IssuedAt:     millis(now.Add(-59 * time.Minute)),
				ExpiresAt:    millis(now.Add(time.Minute)),
			},
		}, true
	case "zcode":
		// JWT 通道：有过期时刻，但上游刷不了。
		return gateway.Credential{
			Provider: id,
			UID:      "pairing",
			Secret: &zcode.Auth{
				Kind:      "jwt",
				JWT:       "zcode-pairing-jwt",
				ExpiresAt: now.Add(time.Minute).Unix(),
			},
		}, true
	default:
		return gateway.Credential{}, false
	}
}

// millis 毫秒时间戳字符串（minimax 凭证的原形状）。
func millis(t time.Time) string { return fmt.Sprintf("%d", t.UnixMilli()) }

func TestRefreshSkewImpliesRefresher(t *testing.T) {
	checked := 0
	for _, u := range allUpstreams(t) {
		cred, canCraft := refreshableCredOf(u.id)
		if !canCraft {
			continue
		}
		skewExt, hasSkew := gateway.ExtOf[gateway.RefreshSkewExt](u.p)
		if !hasSkew {
			continue
		}
		skew, has := skewExt.RefreshSkew(cred)
		if !has {
			t.Logf("%s: 对真实凭证回 ok=false，未纳入检查", u.label)
			continue
		}
		checked++
		if skew <= 0 {
			t.Logf("%s: skew=0（明确不需要提前刷）— 合法。续期实现:%v", u.label, hasRefresher(u.p))
			continue
		}
		if !hasRefresher(u.p) {
			t.Errorf("%s: RefreshSkew 对真实凭证回了正数窗口 %v，却**没有**实现 CredentialRefresher。\n"+
				"这条组合是**静默失败**：核心每次判「该刷了」→ 找不到续期实现 →\n"+
				"被解释成「该上游的凭证不需要刷新」而跳过 ⇒ 凭证一直过期、日志零字。\n"+
				"（用户报的「Token 已过期不能用」就是这个形态。）\n"+
				"\n"+
				"修法二选一：\n"+
				"  a) 能刷 → 实现 RefreshCredential（原地更新池里那份 + 落盘）\n"+
				"  b) 刷不了 → RefreshSkew 改回 (0, true) 明确声明不需要提前刷\n"+
				"     注意不能回 ok=false：那会让核心用它自己的兜底窗口，等于没修",
				u.label, skew)
		}
	}
	if checked == 0 {
		t.Error("一条都没检查到 —— refreshableCredOf 造不出任何上游凭证时，本守卫形同虚设。")
	}
}

// 出过事的两个上游**必须**被这条守卫真的检查到（防它悄悄退化成 skip）。
func TestRefreshPairingCoversTheNewUpstreams(t *testing.T) {
	covered := map[string]bool{"minimax": false, "zcode": false}
	for _, u := range allUpstreams(t) {
		if _, interested := covered[u.label]; !interested {
			continue
		}
		cred, ok := refreshableCredOf(u.id)
		if !ok {
			continue
		}
		skewExt, has := gateway.ExtOf[gateway.RefreshSkewExt](u.p)
		if !has {
			continue
		}
		if _, ok := skewExt.RefreshSkew(cred); ok {
			covered[u.label] = true
		}
	}
	for label, got := range covered {
		if !got {
			t.Errorf("%s 没被成对守卫覆盖 —— 它正是出过事的两个上游之一，"+
				"守卫必须对它真的做判断，而不是 skip。", label)
		}
	}
}

// hasRefresher 该上游是否实现了续期入口。
func hasRefresher(pv gateway.Provider) bool {
	_, ok := gateway.ExtOf[gateway.CredentialRefresher](pv)
	return ok
}

// 另一半不变量：能续期的上游必须也能报过期时刻。
//
// 否则续期后新 token 的寿命信息被丢掉，核心只能靠兜底判"要不要刷" ——
// 本仓为这个形态付过代价（投影 ExpiresAt 恒 0 ⇒ 每个请求都续期一次）。
func TestRefresherUpstreamsAlsoReportExpiry(t *testing.T) {
	for _, u := range allUpstreams(t) {
		if !hasRefresher(u.p) {
			continue
		}
		if _, ok := gateway.ExtOf[gateway.CredentialExpiryExt](u.p); !ok {
			t.Errorf("%s: 能续期却不报过期时刻 —— 续期后的新寿命无处可取。", u.label)
		}
	}
}

// 提前量必须有限（超过 1 小时意味着几乎每次请求都续期，而续期有网络往返）。
func TestRefreshSkewIsSaneWhenKnown(t *testing.T) {
	for _, u := range allUpstreams(t) {
		cred, ok := refreshableCredOf(u.id)
		if !ok {
			continue
		}
		skewExt, has := gateway.ExtOf[gateway.RefreshSkewExt](u.p)
		if !has {
			continue
		}
		skew, ok := skewExt.RefreshSkew(cred)
		if !ok || skew <= 0 {
			continue
		}
		if skew > time.Hour {
			t.Errorf("%s: 提前量 %v 超过 1 小时 —— 那几乎等于每次请求都续期", u.label, skew)
		}
	}
}
