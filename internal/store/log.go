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
		Select("COUNT(*) AS reqs, "+
			"COALESCE(SUM(prompt_tokens), 0) + COALESCE(SUM(completion_tokens), 0) AS tokens, "+
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
		Select("COUNT(*) AS reqs, "+
			"COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END), 0) AS succ, "+
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

// LoadLogsSince 读取 created_at >= fromUnix 的 logs 明细（按时间升序）。
//
// # 为什么需要它（统计统一读库，2026-11-10）
//
// `/admin/stats` 此前只读 JSONL 文件，而那个文件按体积/天数裁剪 ⇒ 实测只剩 2.3 天，
// 页面"总 Token"显示 107.8 亿，而本表同期真相是 274.2 亿（**少报 61%**）。
// 有了这个读取入口，聚合就能改走本表 —— 保留期由 PruneLogs 保证（下限 7 天，
// 远长于文件的 2.3 天），且与 `log_daily` 的日聚合同源。
//
// # 为什么返回 []Log 而不是 []logbuf.Entry
//
// store **不得** import logbuf：logbuf 在请求路径上，而本包是数据层，
// 让数据层依赖"请求日志的内存/落盘形态"会把两个层次绑死
// （反向依赖同样成立：logbuf 也不 import store，见 internal/logbuf/sqlsink.go 的文件头）。
// 转换放在装配层/管理端 —— 那里同时知道两边的类型。
//
// # 为什么要给上限而不是一次全取
//
// 本表是增长最快的表（每请求一行），不设上限时"读全量"会在部署数周后变成
// 一次几百 MB 的内存峰值。limit<=0 取 DefaultLogLoadLimit。
// 调用方应在**命中不了上限**时把 `truncated` 如实报出去（见返回值的第二个参数）——
// 悄悄截断会让"总 Token"再次变成一个看起来像全量的偏小值。
func LoadLogsSince(fromUnix int64, limit int) (rows []Log, truncated bool, err error) {
	if DB == nil {
		return nil, false, ErrNotOpen
	}
	if limit <= 0 {
		limit = DefaultLogLoadLimit
	}
	// ⚠⚠ **用原生行扫描，不用 `Find(&rows)`**（2026-11-10）。
	//
	// 实测（13.6 万行）：SQLite 侧 `SELECT *` 只要 **376 ms**，而整条
	// `/admin/stats` 冷查询要 **2.2 s** —— 差值几乎全在 GORM 的**逐行反射映射**上。
	// 面板是轮询调用的，2.2 s 的冷查询是这条改造引入的退化，必须自己吃掉。
	//
	// 手写 Scan 把映射变成直接的字段赋值 ⇒ 同一份数据只需几十毫秒。
	// 代价是列名与顺序要与下面的 Scan 严格对应 —— 所以两者紧挨着写，
	// 并且**显式列出列名**（不用 `SELECT *`）：列序变化不该静默错位。
	// ⚠⚠ **走 database/sql 直连扫描**（2026-11-10，第二次优化）。
	//
	// 实测三个版本（13 万行，同一台机）：
	//	GORM `Find(&rows)`            → 整个 /admin/stats 冷查询 **2.2 s**
	//	GORM `Raw(...).Rows()` + Scan → `LoadLogsSince` 本身 **1.29 s**
	//	`sql.DB.Query` + `rows.Scan`  → 见下方实测值
	//
	// 差值全在**逐行反射映射**上（SQLite 侧 `SELECT *` 只要 376 ms）。
	// 面板是轮询调用的，这条改造不该让冷查询变慢 —— 所以这里绕开 GORM 的模型层。
	//
	// 代价：列名与 Scan 参数必须严格对应。两者紧挨着写、且**显式列出列名**
	//（不用 `SELECT *`），列序变化不该静默错位。
	const q = "SELECT created_at, model_name, channel_name, account_uid, prompt_tokens, " +
		"completion_tokens, total_tokens, cache_hit_tokens, cache_miss_tokens, cost_quota, " +
		"latency_ms, success, type, status, ttfb_ms, think_tokens, provider " +
		"FROM logs WHERE created_at >= ? ORDER BY created_at ASC LIMIT ?"
	sdb, err := DB.DB()
	if err != nil {
		return nil, false, fmt.Errorf("store: 取底层连接: %w", err)
	}
	// 多取一条用于判断"是否被截断"：取满 limit+1 条说明还有更多。
	rs, err := sdb.Query(q, fromUnix, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: 读 logs: %w", err)
	}
	defer func() { _ = rs.Close() }()
	for rs.Next() {
		var x Log
		var succ int64
		if serr := rs.Scan(
			&x.CreatedAt, &x.ModelName, &x.ChannelName, &x.AccountUID,
			&x.PromptTokens, &x.CompletionTokens, &x.TotalTokens,
			&x.CacheHitTokens, &x.CacheMissTokens, &x.CostQuota,
			&x.LatencyMs, &succ, &x.Type,
			&x.Status, &x.TTFBMs, &x.ThinkTokens, &x.Provider,
		); serr != nil {
			return nil, false, fmt.Errorf("store: 扫描 logs 行: %w", serr)
		}
		x.Success = succ != 0
		rows = append(rows, x)
	}
	if rerr := rs.Err(); rerr != nil {
		return nil, false, fmt.Errorf("store: 遍历 logs: %w", rerr)
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// DefaultLogLoadLimit 一次读取 logs 的默认上限。
//
// 20 万行 ≈ 当前 8 天量级（实测 13.6 万行 / 8 天）留一倍余量；
// 单行约 200 字节 ⇒ 峰值约 40 MB，对统计这种低频（带 TTL 缓存）调用可接受。
const DefaultLogLoadLimit = 200000

// LoadLogRange 读取 [fromUnix, toUnix] 的明细条数与 token 合计（**不加载明细**）。
//
// 给"只需要数字、不需要分维度"的场景用：一条 SQL 出结果，
// 不必把几十万行搬进内存再求和。
func LoadLogRange(fromUnix, toUnix int64) (reqs int64, tokens int64, err error) {
	if DB == nil {
		return 0, 0, ErrNotOpen
	}
	if toUnix <= fromUnix {
		return 0, 0, nil
	}
	var row struct {
		Reqs   int64
		Tokens int64
	}
	err = DB.Model(&Log{}).
		Select("COUNT(*) AS reqs, COALESCE(SUM(total_tokens), 0) AS tokens").
		Where("created_at >= ? AND created_at <= ?", fromUnix, toUnix).
		Scan(&row).Error
	if err != nil {
		return 0, 0, fmt.Errorf("store: 按范围聚合 logs: %w", err)
	}
	return row.Reqs, row.Tokens, nil
}

// LogCoverage 报告 logs 明细的覆盖范围（最早/最新时刻与总行数）。
//
// 用途：界面上"总 Token"必须能说出**它覆盖了多久** ——
// 此前那个数字看起来是全量、实际只有 2.3 天，正是缺这一句造成的误读。
func LogCoverage() (count int64, minUnix, maxUnix int64, err error) {
	if DB == nil {
		return 0, 0, 0, ErrNotOpen
	}
	var row struct {
		N   int64
		Min int64
		Max int64
	}
	err = DB.Model(&Log{}).
		Select("COUNT(*) AS n, COALESCE(MIN(created_at),0) AS min, COALESCE(MAX(created_at),0) AS max").
		Scan(&row).Error
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: 读 logs 覆盖范围: %w", err)
	}
	return row.N, row.Min, row.Max, nil
}
