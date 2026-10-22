// checkin.go 每日签到：接到共享驱动 internal/dailycheckin。
//
// # 为什么用共享驱动而不是自己写一套
//
// 共享驱动把「按钮 / 自动化 / 写历史 / 批量端点」收在一处，
// 上游只描述自己的事实（列账号、查状态、领取）。
//
// 自己写一套的代价本仓有先例 —— lobsterai 漏了写历史，表现是
// 「签到按钮点了、列还是空」：不报错、编译通过、契约测试也查不到。
package monkeycode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// 签到端点。
const pathCheckinStatus = "/api/v1/users/wallet/checkin"

// CheckinStatus 查某账号今天签过没有。
//
// 返回 (已签, 上游是否开放, 错误)。三者必须分开：
//
//	false,false,nil  上游今天不开放（正常，不该写历史）
//	false,true,err   我们没查到（要让用户看见）
func (p *Provider) CheckinStatus(ctx context.Context, a *Auth) (signedIn bool, enabled bool, err error) {
	h := sessionHeaders(a)
	code, raw, err := p.client.doJSON(ctx, a, http.MethodGet, a.Host()+pathCheckinStatus, h, nil)
	if err != nil {
		return false, true, err
	}
	if code != http.StatusOK {
		return false, true, fmt.Errorf("monkeycode: 查签到状态返回 %d：%s", code, truncate(raw, 160))
	}
	body, uerr := unwrap(raw)
	if uerr != nil {
		return false, true, uerr
	}
	var st struct {
		CheckedIn bool `json:"checked_in"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return false, true, fmt.Errorf("monkeycode: 解析签到状态失败：%w", err)
	}
	return st.CheckedIn, true, nil
}

// ClaimCheckin 替一个账号签到。
//
// 流程：查状态 → 已签则早退 → 过 PoW 拿 captcha_token → POST。
//
// ⚠ **已签必须早退**：重复签到在服务端是错误码，会污染历史。
func (p *Provider) ClaimCheckin(ctx context.Context, a *Auth) (int64, bool, error) {
	signedIn, enabled, err := p.CheckinStatus(ctx, a)
	if err != nil {
		return 0, false, err
	}
	if !enabled {
		return 0, false, nil
	}
	if signedIn {
		return 0, false, nil
	}

	captchaToken, err := p.client.solveCaptcha(ctx, a)
	if err != nil {
		return 0, false, fmt.Errorf("验证码：%w", err)
	}

	h := sessionHeaders(a)
	h.Set("Content-Type", "application/json")
	body, _ := json.Marshal(map[string]string{"captcha_token": captchaToken})
	code, raw, err := p.client.doJSON(ctx, a, http.MethodPost, a.Host()+pathCheckinStatus, h, body)
	if err != nil {
		return 0, false, err
	}
	if code != http.StatusOK {
		return 0, false, fmt.Errorf("monkeycode: 签到返回 %d：%s", code, truncate(raw, 160))
	}
	if _, uerr := unwrap(raw); uerr != nil {
		return 0, false, uerr
	}
	// 积分数量上游不返回（签到给的是每日 token 额度，不是积分），这里回 0。
	return 0, false, nil
}
