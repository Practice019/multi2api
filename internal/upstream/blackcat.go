// blackcat.go 夜猫子任务（black_cat）+ 新手礼包/补偿 API。
//
// # 移植说明（1:1 移植自 workbuddy2api-panel/internal/upstream/blackcat.go）
//
// 仅有的适配：
//
//	模块路径      → workbuddy2api/internal/auth
//	ListTasks     → GrowthTasks（本仓库的任务列表函数名）
//	t.Current 等  → t.Current() 等（本仓库用访问器，见 growth.go 的说明）
//	ChatStream    → ChatStreamWithIP(a, body, "")（本仓库多一个 IP 透传参数）
//
// 判据、端点、请求体、休眠节奏**一字未改**。
//
// # 判据（WorkBuddy-Daily 项目实测口径 + B 的网关验证）
//
// black_cat 要求在 **23:00–08:00（本地时区）窗口内**完成 3 次 glm-5.2 对话
// 并上报 chat 事件链；窗口外行为不计分。
// 真实对话走网关既有 ChatStream（glm-5.2），事件链用
// ReportChatActivityModel（chat_5 同款上报形状）。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// InNightWindow 当前是否处于夜猫子计数窗口（23:00–08:00 本地时区）。
//
// ⚠ 用**本地时区**（与 B 同口径），不是固定的 CST。这与本仓库其它
// "按上游自然日"的地方（如 travelDay 用固定 +08:00）刻意不同：
// 上游对这个窗口的判定依据尚未确认，B 是按本地时区实测通过的，
// 移植时保持原样而不是"顺手统一"——改了就等于引入一个未验证的变更。
func InNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// BlackcatNeed 查 black_cat 任务剩余差额（需要再完成几次对话）。
// 任务不存在返回 0（无可做）；拉取失败返回错误。
func (c *Client) BlackcatNeed(a *auth.Auth) (int64, error) {
	tasks, err := c.GrowthTasks(a)
	if err != nil {
		return 0, err
	}
	for i := range tasks {
		t := &tasks[i]
		if t.TaskCode == "black_cat" {
			if t.Claimed() || t.Current() >= t.Target() {
				return 0, nil
			}
			return t.Target() - t.Current(), nil
		}
	}
	return 0, nil
}

// RunNightChats 夜猫子：发 need 次 glm-5.2 真实对话（读干流）并上报事件链。
// 返回成功次数。对话内容极短（1+1），消耗可忽略。
func (c *Client) RunNightChats(a *auth.Auth, need int) (int64, error) {
	var ok int64
	for i := 0; i < need; i++ {
		body, _ := json.Marshal(map[string]any{
			"model":    "glm-5.2",
			"messages": []map[string]any{{"role": "user", "content": "1+1等于几？直接回答。"}},
			"stream":   true,
		})
		rc, status, respBody, err := c.ChatStreamWithIP(a, body, "")
		if err != nil || status >= 400 {
			if rc != nil {
				rc.Close()
			}
			return ok, fmt.Errorf("第 %d 次对话失败: http=%d err=%v body=%.120s", i+1, status, err, respBody)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		rc.Close()
		if err := c.ReportChatActivityModel(a, fmt.Sprintf("wb2api-night-%d-%d", time.Now().UnixMilli(), i), "", "glm-5.2", "GLM-5.2"); err != nil {
			return ok, fmt.Errorf("第 %d 次上报失败: %w", i+1, err)
		}
		ok++
		time.Sleep(4 * time.Second)
	}
	return ok, nil
}

// ClaimGift 领取新手礼包（每号一次，已领返回业务错误）。
func (c *Client) ClaimGift(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-gift", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// ClaimCompensation 领取活动补偿（有则领，无则业务错误）。
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-compensation", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// HeatmapYesterdayMissed 检查昨日是否漏签（heatmap cell score==0）。
func (c *Client) HeatmapYesterdayMissed(a *auth.Auth) (bool, error) {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	data, err := c.growthJSON(a, http.MethodGet, "/activity/growth/heatmap", nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// UseMakeupCard 对指定日期使用补签卡（保住连登连续天数；无卡返回业务错误）。
//
// ⚠ 与本仓库既有的 GrowthMakeup 是**同一个端点**（/activity/growth/makeup-cards/use）。
// 保留两份是因为调用语义不同：GrowthMakeup 走管理台的手动/自动补签路径，
// 本函数供夜猫子整理链路使用。两者请求体相同，行为一致。
func (c *Client) UseMakeupCard(a *auth.Auth, date string) error {
	_, err := c.growthJSON(a, http.MethodPost, "/activity/growth/makeup-cards/use",
		map[string]any{"target_date": date})
	return err
}
