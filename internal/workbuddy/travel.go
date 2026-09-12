// travel.go 猫猫旅行巡检状态机：随签到时点对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
//
// # 搬运说明（Task 3b）
//
// 本文件原先在 internal/scheduler/travel.go。猫猫旅行是 CodeBuddy 专属业务
// （codearts 没有 Buddy 体系），留在核心调度器里等于让核心理解某个上游的领域概念。
package workbuddy

import (
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/upstream"
)

const (
	// TravelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	TravelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// 触发来源标识（写入历史，用于区分定时任务与人工点击）。
const (
	triggerSchedule = "schedule"
	triggerManual   = "manual"
	// triggerAuto 自动（守卫领奖）。
	triggerAuto = "auto"
)

// TravelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
//
// 导出是为了让搬运过来的测试仍能把它置 0（原先它是 scheduler 包内变量）。
var TravelAccountDelay = 800 * time.Millisecond

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检（定时任务入口）。
// 禁用账号跳过；401/查询失败只跳过该账号本轮（不强刷 token，交 keepalive）；
// 账号间限速 TravelAccountDelay。
func (p *Provider) RunTravelNow() { p.runTravel(triggerSchedule) }

// RunTravelManual 手动触发全量旅行巡检（管理台入口，结果按 manual 记入历史）。
func (p *Provider) RunTravelManual() { p.runTravel(triggerManual) }

func (p *Provider) runTravel(trigger string) {
	first := true
	// 按上游取号：本上游的旅行接口不认别家上游的凭证，
	// 遍历全池只会对 codearts 的号发一串注定失败的请求。
	for _, st := range p.ownAccounts() {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if !first {
			time.Sleep(TravelAccountDelay)
		}
		first = false
		p.travelOne(a, trigger)
	}
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
func (p *Provider) travelOne(a *auth.Auth, trigger string) {
	buddy, err := p.client.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", a.UID, err)
		return
	}
	if buddy == nil {
		p.travelAdopt(a, trigger)
		return
	}
	ts, err := p.client.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", a.UID, err)
		return
	}
	switch ts.State {
	case travelStateArrived:
		p.travelClaim(a, ts, trigger)
	case travelStateIdle:
		p.travelDepart(a, ts, trigger)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", a.UID, ts.RecordID)
	default:
		log.Printf("travel %s: skip (unknown state %q)", a.UID, ts.State)
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (p *Provider) travelDepart(a *auth.Auth, ts *upstream.TravelState, trigger string) {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", a.UID)
		p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusSkip, "今日已派出", 0, trigger)
		return
	}
	if err := p.client.TravelDepart(a, TravelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", a.UID, err)
		p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusFail, "派出: "+shortErr(err), 0, trigger)
		return
	}
	log.Printf("travel %s: depart ok location=%d", a.UID, TravelLocationID)
	p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusOK, "已派出", 0, trigger)
}

// travelClaim 到站领奖（必须带 record_id）。
func (p *Provider) travelClaim(a *auth.Auth, ts *upstream.TravelState, trigger string) {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", a.UID)
		p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusSkip, "到站但无 record_id", 0, trigger)
		return
	}
	reward, err := p.client.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", a.UID, ts.RecordID, err)
		p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusFail, "领奖: "+shortErr(err), 0, trigger)
		return
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", a.UID, ts.RecordID, reward)
	p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusOK, "已领奖", reward, trigger)
}

// travelAdopt 无猫时领养：先同意协议（幂等）再 buddy/first。
// conversation 门槛未达标（HTTP 400 first_buddy task not completed yet）属预期行为，
// 记一次当日已试后静默跳过，不再重试。
func (p *Provider) travelAdopt(a *auth.Auth, trigger string) {
	if p.adoptTriedToday(a.UID) {
		return
	}
	if err := p.client.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", a.UID, err)
		p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusFail, "协议: "+shortErr(err), 0, trigger)
		return
	}
	err := p.client.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", a.UID)
		p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusOK, "已领养 (+300)", 300, trigger)
	case upstream.IsBuddyTaskIncomplete(err):
		p.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", a.UID)
		p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusSkip, "对话门槛未达，明日再试", 0, trigger)
	default:
		log.Printf("travel %s: adopt: %v", a.UID, err)
		p.record(a.UID, checkinlog.KindTravel, checkinlog.StatusFail, "领养: "+shortErr(err), 0, trigger)
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (p *Provider) adoptTriedToday(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (p *Provider) markAdoptTried(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.adoptTried[uid] = travelDay(time.Now())
}
