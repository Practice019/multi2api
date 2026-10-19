// series.go —— GET /admin/stats/series 时间序列聚合（仪表盘趋势图的数据源）。
//
// # 为什么单独一个端点而不是塞进 /admin/stats
//
// /admin/stats 回答"累计到今天一共多少"，趋势图回答"过去 24h 每小时多少"。
// 两者是不同形状的响应（标量集合 vs 桶序列），拆开各自清晰；
// 数据源复用 statsItems 的缓存（同一个 request-log / 环形缓冲），
// 不重复扫描。
package admin

import (
	"net/http"
	"time"
)

// statsSeriesItem 一个时间桶的聚合。
type statsSeriesItem struct {
	TS     string `json:"ts"`     // RFC3339 桶起点（小时/天）
	Total  int64  `json:"total"`  // 请求数
	OK     int64  `json:"ok"`     // 成功数
	Fail   int64  `json:"fail"`   // 失败数
	Tokens int64  `json:"tokens"` // 消耗 token（usage 缺失不计）
	// ⚠ 缓存命中率趋势要的两个量（2026-10-18 加，照 agent2api 的 `cache_trend_24h`）。
	//
	// ⚠ **一处刻意偏离它的口径**：它的 `rate = hit / input`，其中 `input` 取
	// `prompt_tokens`。⚠ 我们的日志**没有 prompt_tokens 这个字段** ⇒ 这里只给
	// 两个**原始量**，`rate` 由前端按 `hit / (hit + miss)` 算 —— 与顶层
	// `cache_hit_rate` 和 `cache_rates` 四档**同一口径**，不会出现"同一个指标两个算法"。
	//
	// ⚠ 不给 rate 还有一个理由：它的前端明确要求"命中率纵轴上限按真实峰值抬、
	// **不夹到 100%**"（上游分开上报时合法可超 100%）；把 rate 交给前端算，
	// 前端才能在**同一个地方**决定轴上限与"整段无数据就不画这条线"。
	CacheHit  int64 `json:"cache_hit"`
	CacheMiss int64 `json:"cache_miss"`
}

// registerStatsSeries 注册趋势图端点。
func (h *Handler) registerStatsSeries() {
	h.register("GET /admin/stats/series", h.statsSeries)
}

// statsSeries GET /admin/stats/series?range=24h|7d&bucket=hour|day
//
// 桶按本地时区对齐：24h 用小时桶（24 点），7d 用天桶（7 点）。
// 空桶补 0（前端补缺与 one-api 的 processTimeSeriesData 同一手法，
// 但后端直接补齐，前端只画图）。
func (h *Handler) statsSeries(w http.ResponseWriter, r *http.Request) {
	span, bucket := seriesSpan(r)
	items, _, err := h.statsItems()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取日志失败: "+err.Error())
		return
	}

	now := time.Now()
	// ⚠ 用 bucketStart 而不是 Truncate —— 天桶要按**本地自然日**对齐（见 bucketStart 的注释：F2）。
	start := bucketStart(now.Add(-span), bucket)
	buckets := int(span / bucket)
	out := make([]statsSeriesItem, 0, buckets+1)
	// 先铺满空桶（保证前端不需要补缺）
	idx := make(map[int64]int, buckets+1)
	for i := 0; i <= buckets; i++ {
		ts := bucketStart(start.Add(bucket*time.Duration(i)), bucket)
		idx[ts.Unix()] = i
		out = append(out, statsSeriesItem{TS: ts.Format(time.RFC3339)})
	}
	for _, e := range items {
		if e.At.Before(start) {
			continue
		}
		i, ok := idx[bucketStart(e.At, bucket).Unix()]
		if !ok {
			continue
		}
		out[i].Total++
		if e.Status >= 200 && e.Status < 300 {
			out[i].OK++
		} else {
			out[i].Fail++
		}
		if e.Tokens >= 0 {
			// 同 admin.go 的那条守卫：Tokens 的 -1 是"usage 缺失"哨兵，不能累加。
			if e.Tokens > 0 {
				out[i].Tokens += int64(e.Tokens)
			}
		}
		// ⚠ 缓存两量与别处同一取法：>0 才累加（负数/负零属上游异常，丢了比算进去好解释）。
		if e.CacheHitTokens > 0 {
			out[i].CacheHit += int64(e.CacheHitTokens)
		}
		if e.CacheMissTokens > 0 {
			out[i].CacheMiss += int64(e.CacheMissTokens)
		}
	}
	// 去掉尾部全空桶（活跃窗口之外），保留从 start 起的连续段
	for len(out) > 0 && out[len(out)-1].Total == 0 {
		out = out[:len(out)-1]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"range":  span.String(),
		"bucket": bucket.String(),
		"source": "log",
		"series": out,
	})
}

// seriesSpan 解析 range/bucket 参数；非法回落 24h/hour。
//
// ⚠ 支持的范围：`24h`（小时桶）· `7d` / `30d` / `365d`（天桶）。
// `365d` 是给**活跃热力图**（仪表盘那个 365 天日历阵）用的。
func seriesSpan(r *http.Request) (time.Duration, time.Duration) {
	span := 24 * time.Hour
	bucket := time.Hour
	switch r.URL.Query().Get("range") {
	case "365d":
		span = 365 * 24 * time.Hour
		bucket = 24 * time.Hour
	case "30d":
		span = 30 * 24 * time.Hour
		bucket = 24 * time.Hour
	case "7d":
		span = 7 * 24 * time.Hour
		bucket = 24 * time.Hour
	case "24h":
		span = 24 * time.Hour
		bucket = time.Hour
	}
	// bucket 显式覆盖（默认随 range）
	if b := r.URL.Query().Get("bucket"); b == "day" {
		bucket = 24 * time.Hour
	}
	return span, bucket
}

// bucketStart **桶起点**：天桶按**本地自然日**对齐，小时桶按本地整点对齐。
//
// ⚠⚠ 这里为什么不能用 `t.Truncate(24*time.Hour)`（原来就是这么写的，已修）：
// `time.Truncate` 以 **UTC 零时**为基准取整 ⇒ 天桶的起点会落在**本地 08:00**
// （东八区），于是「7 天」会变成 **8 个桶 / 覆盖 189 小时**，
// 而热力图上"10-17 那一格"实际代表的是 10-17 08:00 ～ 10-18 08:00 —— **格子与日期错位**。
// ⚠ 这是 R1 复核抓到的 F2，在加 365 天热力图时顺手修掉（热力图正是它的放大版）。
//
// 小时桶保持"本地整点"：原来 `Truncate(time.Hour)` 在整小时区（东八区）本来就等于本地整点，
// 这里用同一套写法统一掉，语义上更明确。
func bucketStart(t time.Time, bucket time.Duration) time.Time {
	local := t.Local()
	if bucket >= 24*time.Hour {
		y, m, d := local.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, local.Location())
	}
	y, m, d := local.Date()
	return time.Date(y, m, d, local.Hour(), 0, 0, 0, local.Location())
}
