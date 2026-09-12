// upstream_business.go 把 workbuddy 的业务能力适配成 admin 的消费方接口。
//
// # 为什么需要这一层薄适配
//
// 两边的类型结构完全相同，但**不能共用同一个定义**：
//   - workbuddy 不能 import admin（架构约束：上游不得依赖核心包）
//   - admin 不能 import workbuddy（否则加新上游时 admin 要改）
//
// 所以各自声明，转换只在这一个地方做。这与 main.go 里 modelCatalogState
// 的做法完全一致（server 与 admin 各有一份结构相同的目录状态类型）。
//
// 适配器很薄：每个方法只做一次逐字段转换，没有逻辑。
// 它存在的意义是让"两侧的类型各自独立演化"，代价是一处编译期检查的转换。
package main

import (
	"time"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/workbuddy"
)

// ---------------------------------------------------------------------------
// 账号池适配器
// ---------------------------------------------------------------------------

// poolAdapter 把 *pool.Pool 适配成 workbuddy.AccountPool。
//
// 只有 List 需要转换（pool.Status → workbuddy.Account）；其余方法签名完全一致，
// 直接转发。放在 cmd/server 是因为这里是唯一同时认识两侧的地方。
type poolAdapter struct{ p *pool.Pool }

func (a poolAdapter) List() []workbuddy.Account {
	src := a.p.List()
	out := make([]workbuddy.Account, 0, len(src))
	for _, st := range src {
		out = append(out, workbuddy.Account{
			UID:      st.UID,
			Nickname: st.Nickname,
			Disabled: st.Disabled,
			Cooling:  st.Cooling,
		})
	}
	return out
}

func (a poolAdapter) AuthByUID(uid string) *auth.Auth { return a.p.AuthByUID(uid) }

func (a poolAdapter) Has(uid string) bool {
	_, ok := a.p.Status(uid)
	return ok
}

func (a poolAdapter) SetCredits(uid string, credits int64) { a.p.SetCredits(uid, credits) }

func (a poolAdapter) ReenableIfCredits(uid string, remain int64) {
	a.p.ReenableIfCredits(uid, remain)
}

func (a poolAdapter) Disable(uid, reason string) { a.p.Disable(uid, reason) }

var _ workbuddy.AccountPool = poolAdapter{}

// businessAdapter 实现 admin.UpstreamBusiness。
type businessAdapter struct {
	p *workbuddy.Provider
}

// newBusinessAdapter 包装一个上游 Provider。
func newBusinessAdapter(p *workbuddy.Provider) admin.UpstreamBusiness {
	return &businessAdapter{p: p}
}

// ---- 成长中心 ----

func (a *businessAdapter) RefreshGrowth(force, autoActions bool) []admin.GrowthSnapshot {
	src := a.p.RefreshGrowth(force, autoActions)
	out := make([]admin.GrowthSnapshot, 0, len(src))
	for _, s := range src {
		out = append(out, toAdminGrowthSnapshot(s))
	}
	return out
}

func (a *businessAdapter) GrowthSnapshots() []admin.GrowthSnapshot {
	src := a.p.GrowthSnapshots()
	out := make([]admin.GrowthSnapshot, 0, len(src))
	for _, s := range src {
		out = append(out, toAdminGrowthSnapshot(s))
	}
	return out
}

func (a *businessAdapter) GrowthToggles() (accept, makeup, redeem, open, draw, claim bool) {
	return a.p.GrowthToggles()
}

func (a *businessAdapter) GrowthWatchInterval() time.Duration {
	return a.p.GrowthWatchInterval()
}

func (a *businessAdapter) SetGrowthToggles(accept, makeup, redeem, open, draw, claim bool) {
	a.p.SetGrowthToggles(accept, makeup, redeem, open, draw, claim)
}

func (a *businessAdapter) GrowthClaimFor(uid, taskCode, trigger string) admin.GrowthActionResult {
	return toAdminGrowthResult(a.p.GrowthClaimFor(uid, taskCode, trigger))
}

func (a *businessAdapter) GrowthAcceptFor(uid, taskCode, trigger string) admin.GrowthActionResult {
	return toAdminGrowthResult(a.p.GrowthAcceptFor(uid, taskCode, trigger))
}

func (a *businessAdapter) GrowthRedeemFor(uid, tier, trigger string) admin.GrowthActionResult {
	return toAdminGrowthResult(a.p.GrowthRedeemFor(uid, tier, trigger))
}

func (a *businessAdapter) GrowthMakeupFor(uid, date, trigger string) admin.GrowthActionResult {
	return toAdminGrowthResult(a.p.GrowthMakeupFor(uid, date, trigger))
}

func (a *businessAdapter) GrowthOpenFor(uid string, count int, trigger string) admin.GrowthActionResult {
	return toAdminGrowthResult(a.p.GrowthOpenFor(uid, count, trigger))
}

func (a *businessAdapter) GrowthDrawFor(uid, trigger string) admin.GrowthActionResult {
	return toAdminGrowthResult(a.p.GrowthDrawFor(uid, trigger))
}

// ---- 猫猫旅行 ----

func (a *businessAdapter) RefreshTravel(force, autoClaim bool) []admin.TravelSnapshot {
	src := a.p.RefreshTravel(force, autoClaim)
	out := make([]admin.TravelSnapshot, 0, len(src))
	for _, s := range src {
		out = append(out, toAdminTravelSnapshot(s))
	}
	return out
}

func (a *businessAdapter) TravelSnapshots() []admin.TravelSnapshot {
	src := a.p.TravelSnapshots()
	out := make([]admin.TravelSnapshot, 0, len(src))
	for _, s := range src {
		out = append(out, toAdminTravelSnapshot(s))
	}
	return out
}

func (a *businessAdapter) TravelAutoClaimEnabled() bool { return a.p.TravelAutoClaimEnabled() }
func (a *businessAdapter) SetTravelAutoClaim(on bool)   { a.p.SetTravelAutoClaim(on) }
func (a *businessAdapter) WatchInterval() time.Duration { return a.p.WatchInterval() }
func (a *businessAdapter) RunTravelManual()             { a.p.RunTravelManual() }

func (a *businessAdapter) TravelDepartFor(uid, trigger string) admin.TravelActionResult {
	return toAdminTravelResult(a.p.TravelDepartFor(uid, trigger))
}

func (a *businessAdapter) TravelClaimFor(uid, trigger string) admin.TravelActionResult {
	return toAdminTravelResult(a.p.TravelClaimFor(uid, trigger))
}

// ---- 额度 ----

func (a *businessAdapter) RefreshCredits(uid, trigger string) (admin.CheckinView, bool) {
	res, ok := a.p.RefreshCredits(uid, trigger)
	return admin.CheckinView{
		UID: res.UID, Status: res.Status, Detail: res.Detail,
		Credits: res.Credits, HasQuota: res.HasQuota,
	}, ok
}

// ---- 逐字段转换 ----

func toAdminGrowthSnapshot(s workbuddy.GrowthSnapshot) admin.GrowthSnapshot {
	out := admin.GrowthSnapshot{
		UID: s.UID, Nickname: s.Nickname,
		TasksTotal: s.TasksTotal, TasksCompleted: s.TasksCompleted, TasksAccepted: s.TasksAccepted,
		AcceptableCount: s.AcceptableCount, AcceptableCredit: s.AcceptableCredit,
		AcceptableEnergy: s.AcceptableEnergy,
		PendingCount:     s.PendingCount, PendingCredit: s.PendingCredit, PendingEnergy: s.PendingEnergy,
		ClaimableCount: s.ClaimableCount, ClaimableCredit: s.ClaimableCredit,
		ClaimableEnergy: s.ClaimableEnergy,
		StreakDays:      s.StreakDays, NextTier: s.NextTier, NextTierRemaining: s.NextTierRemaining,
		MakeupCards: s.MakeupCards, MakeupDates: s.MakeupDates, RemainingDays: s.RemainingDays,
		Energy: s.Energy, BlindBoxAffordable: s.BlindBoxAffordable, BlindBoxCost: s.BlindBoxCost,
		LotteryChances: s.LotteryChances,
		ObservedAt:     s.ObservedAt, Error: s.Error, Stale: s.Stale,
	}
	if len(s.Tasks) > 0 {
		out.Tasks = make([]admin.GrowthTaskView, 0, len(s.Tasks))
		for _, t := range s.Tasks {
			out.Tasks = append(out.Tasks, admin.GrowthTaskView{
				TaskCode: t.TaskCode, Title: t.Title,
				Description: t.Description, HowTo: t.HowTo, Status: t.Status,
				Current: t.Current, Target: t.Target,
				RewardCredit: t.RewardCredit, RewardEnergy: t.RewardEnergy,
				Tag: t.Tag, Locked: t.Locked, Claimable: t.Claimable,
				ExpiresAt: t.ExpiresAt, ExpiresInDays: t.ExpiresInDays,
				Expired: t.Expired, ExpiringSoon: t.ExpiringSoon,
			})
		}
	}
	return out
}

func toAdminGrowthResult(r workbuddy.GrowthActionResult) admin.GrowthActionResult {
	return admin.GrowthActionResult{
		UID: r.UID, Action: r.Action, Status: r.Status, Detail: r.Detail,
		Credits: r.Credits, Energy: r.Energy, Count: r.Count,
		AlreadyClaimed: r.AlreadyClaimed,
	}
}

func toAdminTravelSnapshot(s workbuddy.TravelSnapshot) admin.TravelSnapshot {
	return admin.TravelSnapshot{
		UID: s.UID, Nickname: s.Nickname,
		HasBuddy: s.HasBuddy, BuddyName: s.BuddyName, BuddyRarity: s.BuddyRarity,
		State: s.State, LocationName: s.LocationName,
		RecordID: s.RecordID, DepartAt: s.DepartAt, ArriveAt: s.ArriveAt,
		RemainingSec: s.RemainingSec, DurationHours: s.DurationHours,
		RewardCredit: s.RewardCredit, DailyLimitReached: s.DailyLimitReached,
		ObservedAt: s.ObservedAt, Error: s.Error,
	}
}

func toAdminTravelResult(r workbuddy.TravelActionResult) admin.TravelActionResult {
	return admin.TravelActionResult{
		UID: r.UID, Action: r.Action, Status: r.Status, Detail: r.Detail,
		Credits: r.Credits, State: r.State, ArriveAt: r.ArriveAt,
	}
}
