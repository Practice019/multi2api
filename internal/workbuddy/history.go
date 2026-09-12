// history.go workbuddy 自己的历史记录与错误文案工具。
//
// 这两个函数原先在 internal/scheduler 里，但它们的调用点全部落在
// workbuddy 的业务路径上（旅行/成长的每一步都要记一条历史）。
// 核心调度器自身（签到/保活）另有一份独立实现，不依赖本文件。
package workbuddy

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"workbuddy2api/internal/checkinlog"
)

// record 写一条任务历史（未注入 Log 时静默丢弃）。
func (p *Provider) record(uid, kind, status, detail string, credits int64, trigger string) {
	if p.cfg.Log == nil {
		return
	}
	nick := ""
	if p.cfg.Pool != nil {
		if a := p.cfg.Pool.AuthByUID(uid); a != nil {
			nick = a.Nickname
		}
	}
	p.cfg.Log.Append(checkinlog.Record{
		At:       time.Now(),
		UID:      checkinlog.NormalizeUID(uid),
		Nickname: nick,
		Kind:     kind,
		Status:   status,
		Detail:   detail,
		Credits:  credits,
		Trigger:  trigger,
	})
}

// RefreshCredits 只查余额并同步进池（不签到）。
//
// 原先在 internal/scheduler。它被成长守卫的自动领奖路径调用（领奖后同步积分），
// 因此跟着业务一起搬过来。管理台的「刷新积分」端点也走这里。
func (p *Provider) RefreshCredits(uid, trigger string) (CreditsResult, bool) {
	if p.cfg.Pool == nil {
		return CreditsResult{UID: uid, Status: checkinlog.StatusFail, Detail: "账号池未接线"}, false
	}
	if !p.cfg.Pool.Has(uid) {
		return CreditsResult{UID: uid, Status: checkinlog.StatusFail, Detail: "账号不存在"}, false
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return CreditsResult{UID: uid, Status: checkinlog.StatusFail, Detail: "无可用凭证"}, true
	}
	res := CreditsResult{UID: uid, Status: checkinlog.StatusOK}
	remain, err := p.client.UserResource(a)
	if err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, shortErr(err)
		// 余额查询失败时补一句「上游怎么解释」。
		//
		// 为什么挂在这里而不是做成定时任务：实测该端点正常时**完全静默**
		// （三个账号都是 notifyCode=0、文案为空），平时查它只是多打一次上游。
		// 它唯一的价值是"出故障时给一句人话" —— 于是就该只在故障路径上查。
		// 失败不影响主流程：拿不到解释只是少一句话。
		if w := p.dosageHint(uid); w != "" {
			res.Detail += "；上游提示：" + w
		}
		p.record(uid, checkinlog.KindCredits, res.Status, res.Detail, 0, trigger)
		return res, true
	}
	res.Credits, res.HasQuota = remain, true
	p.cfg.Pool.SetCredits(uid, remain)
	p.cfg.Pool.ReenableIfCredits(uid, remain)
	p.record(uid, checkinlog.KindCredits, res.Status, "", remain, trigger)
	return res, true
}

// CreditsResult 余额刷新结果（与 scheduler.CheckinResult 结构一致，
// 便于 cmd/server 与 admin 在两者之间零成本转换）。
type CreditsResult struct {
	UID      string `json:"uid"`
	Status   string `json:"status"` // ok | already | fail | skip
	Detail   string `json:"detail,omitempty"`
	Credits  int64  `json:"credits"`
	HasQuota bool   `json:"has_quota"`
}

// dosageHint 查询上游的额度告警文案，拿不到或没有告警时返回空串。
//
// 刻意返回 string 而不是 (*DosageWarning, error)：调用方只想要"一句话"，
// 而这里所有失败分支的正确行为都一样 —— 静默跳过。让调用方去判断
// nil/err/nil-err 三种情况只会把一条提示变成三个分支。
//
// 入参是 uid 而不是 *auth.Auth：凭证在探测时刻从池里现取
// （余额查询刚失败，池里可能已标记该号状态）。
//
// defer recover 的考量：这是诊断路径，任何意外都不该把一次本该只是
// "余额查询失败"的操作升级成 panic。
func (p *Provider) dosageHint(uid string) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = ""
		}
	}()
	if p.cfg.Pool == nil {
		return ""
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return ""
	}
	w, err := p.client.DosageNotify(a)
	if err != nil || w == nil {
		return ""
	}
	// 走 shortErr 压行：这个文案要进历史文件，不能让上游的长文本撑爆一行。
	return shortErr(fmt.Errorf("%s", w.Message()))
}

// shortErr 把错误压成一行短文本，避免历史文件被长堆栈撑爆。
//
// 截断按**字符边界**退让，不直接切字节：上游的额度告警是中文（3 字节/字符），
// 按字节切 120 极易落在字符中间，产生非法 UTF-8 —— 实测 6 组样本里 3 组中招，
// 序列化进 JSON 后变成 \ufffd（"�"），用户看到的就是乱码尾巴。
//
// 这个值会经 res.Detail 落进历史，所以必须干净。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	// 换行与回车都压平：只替 \n 会留下裸 \r（"a\r\nb" → "a\r b"），
	// 而 \r 进落盘文件会让行式解析器/编辑器把一行当两行。
	s := strings.ReplaceAll(err.Error(), "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	const max = 120
	if len(s) <= max {
		return s
	}
	// 从 max 往前退，直到前缀是合法 UTF-8。最多退 3 字节（UTF-8 单字符上限）。
	n := max
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}
