package scheduler

// triggerManual 触发来源标识（写入历史，用于区分定时任务与人工点击）。
//
// 改造前这些常量与旅行/成长业务同在一处。业务搬走后，核心自己仍需要它们 ——
// 签到/保活的历史里也要写 trigger（"schedule" 与 "manual"）。
const (
	triggerSchedule = "schedule"
	triggerManual   = "manual"
)
