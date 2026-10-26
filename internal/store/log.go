// log.go logs 表的写入器与三个聚合查询（PLAN-sql-unify.md 的 T8 数据层）。
//
// # 它是系统状态页**所有业务指标的唯一来源**（D5）
//
//	rpm            = 窗口内请求数 ÷ 窗口分钟数
//	success_rate   = 成功 / 总数（**无样本保持 0，不误报**）
//	avg_latency    = AVG（**不是 P95**）
//	今日请求/Token/消费 = 按本地日 0 点起算的 COUNT / SUM(tokens) / SUM(cost_quota)
//
// # 为什么"今日"的 0 点由调用方传（本函数**不取 time.Now**）
//
// 因为"今天"依赖时区与部署地的日历，而 store 不该知道这些。调用方（cmd/server）
// 用 time.Now().In(loc) 算出本地日 0 点的 unix 秒再传进来，于是同一份聚合代码在
// "用户在东八区、服务器在 UTC"的部署里也是对的，而且**可测**——测试要造"昨天/
// 今天"的数据，不需要去骗时钟。这与 hyxapi controller/system_status.go:84 同构。
//
// # ⚠ 两处 AVG/P95 并存是刻意的
//
// 本仓的号池（internal/pool）已经有 P95 口径，那是**熔断判据**；这里的
// avg_latency 是**页面展示口径**，照抄 hyxapi。两者用途不同，谁也不该被另一个
// 替换掉——所以统一在这里写死"用 AVG"，并留这段注释免得后人"顺手对齐"。
package store

import (
	"fmt"
)

// AppendLog 写一条请求日志。
//
// # 调用方必须异步（这条是接口语义的一部分）
//
// 日志写失败**绝不允许影响主流程**：调用方把 Log 丢进有界 channel，由后台
// goroutine 调这里；满了就丢弃并计数（计划 §5 的"丢日志不丢请求"）。
// 本函数只保证：① 单条插入就是一次 INSERT，不做查询/重试/回填；
// ② 失败如实返回 error，**由调用方决定丢不丢**。
func AppendLog(e Log) error {
	if DB == nil {
		return ErrNotOpen
	}
	if err := DB.Create(&e).Error; err != nil {
		return fmt.Errorf("store: 写 logs: %w", err)
	}
	return nil
}

// todayRow / windowRow 是聚合结果的扫描目标。
//
// 用**独立的内部结构体**而不是 Log：一条 SQL 出来的列名（reqs/succ/cost…）
// 与 Log 的字段完全不同，硬扫进 Log 会得到一堆零值字段和"看起来成功"的假结果。
type todayRow struct {
	Reqs   int64
	Tokens int64
	Cost   int64
}

type windowRow struct {
	Reqs         int64
	Succ         int64
	AvgLatencyMs float64
}

// TodayAggregate 今日请求数 / Token 总数 / 消费额。
//
// 口径与 hyxapi model/log.go:501、controller/system_status.go:84-86 一致：
// 从 dayStartUnix（**本地日 0 点**）起算，**不设上界**（今天还没过完）。
//
// tokens 是"入+出"两段的合计（hyxapi 同样是 sum(prompt)+sum(completion)）——
// 不直接用 total_tokens 列：那是写入方算出来的派生值，历史行可能缺它，
// 而两段原始计数一定是有的。
func TodayAggregate(dayStartUnix int64) (reqs, tokens int64, cost int64, err error) {
	if DB == nil {
		return 0, 0, 0, ErrNotOpen
	}
	var row todayRow
	err = DB.Model(&Log{}).
		Select("COUNT(*) AS reqs, " +
			"COALESCE(SUM(prompt_tokens), 0) + COALESCE(SUM(completion_tokens), 0) AS tokens, " +
			"COALESCE(SUM(cost_quota), 0) AS cost").
		Where("created_at >= ?", dayStartUnix).
		Scan(&row).Error
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: 今日聚合: %w", err)
	}
	return row.Reqs, row.Tokens, row.Cost, nil
}

// WindowAggregate 一个时间窗内的请求数、成功数与平均延迟（毫秒）。
//
// 边界是**闭区间** [fromUnix, toUnix]，与 hyxapi 的 GetRequestHealthStat
// （model/log.go:526-539）一致：它也是 created_at >= start AND created_at <= end。
//
// 零宽或倒挂的窗口（to <= from）直接返回全 0：那不是一个"查询"，是一个调用方
// 算错了的窗口，返回 0 比返回整表统计安全得多（后者会表现为"页面上的 RPM
// 突然变成历史总量"）。
//
// avg 只统计 latency_ms > 0 的样本 —— 同 hyxapi 的 use_time > 0：失败的请求
// 往往没记延迟，把它们算进分母会把平均延迟**假性拉低**。
func WindowAggregate(fromUnix, toUnix int64) (reqs, succ int64, avgLatencyMs float64, err error) {
	if DB == nil {
		return 0, 0, 0, ErrNotOpen
	}
	if toUnix <= fromUnix {
		return 0, 0, 0, nil
	}
	var row windowRow
	err = DB.Model(&Log{}).
		Select("COUNT(*) AS reqs, " +
			"COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END), 0) AS succ, " +
			"COALESCE(AVG(CASE WHEN latency_ms > 0 THEN latency_ms END), 0) AS avg_latency_ms").
		Where("created_at >= ? AND created_at <= ?", fromUnix, toUnix).
		Scan(&row).Error
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: 窗口聚合: %w", err)
	}
	return row.Reqs, row.Succ, row.AvgLatencyMs, nil
}

// SuccessRate 成功率的**唯一**算法（百分比，0–100）。
//
// 无样本时返回 0 而不是 NaN：hyxapi 的做法是 if total > 0 { … }，即默认值留 0
// （model/log.go:543-545）。若在这里硬做除法，0 请求会得到 NaN ⇒ 序列化成 JSON
// 时是 null（Go 的 encoding/json 拒绝 NaN），前端拿到 null 要么显示 0% 要么
// 直接报错，两种都比"明确说 0"差。
func SuccessRate(reqs, succ int64) float64 {
	if reqs <= 0 {
		return 0
	}
	return float64(succ) / float64(reqs) * 100
}

// RPMOf 窗口 RPM（请求/分钟）。
//
// 照抄 hyxapi model/log.go:546-551：分钟数 = (to-from)/60，且**下限 1 分钟**。
// 下限是必须的：一个 5 秒宽的窗口如果不夹到 1 分钟，"1 个请求"会被算成 12 RPM，
// 页面上的数字会随着刷新时刻剧烈跳动。
func RPMOf(reqs int64, fromUnix, toUnix int64) float64 {
	if toUnix <= fromUnix {
		return 0
	}
	minutes := float64(toUnix-fromUnix) / 60
	if minutes < 1 {
		minutes = 1
	}
	return float64(reqs) / minutes
}
