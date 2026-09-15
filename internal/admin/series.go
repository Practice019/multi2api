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
	start := now.Add(-span).Truncate(bucket)
	buckets := int(span / bucket)
	out := make([]statsSeriesItem, 0, buckets+1)
	// 先铺满空桶（保证前端不需要补缺）
	idx := make(map[int64]int, buckets+1)
	for i := 0; i <= buckets; i++ {
		ts := start.Add(bucket * time.Duration(i))
		idx[ts.Unix()] = i
		out = append(out, statsSeriesItem{TS: ts.Format(time.RFC3339)})
	}
	for _, e := range items {
		if e.At.Before(start) {
			continue
		}
		i, ok := idx[e.At.Truncate(bucket).Unix()]
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
			out[i].Tokens += int64(e.Tokens)
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
func seriesSpan(r *http.Request) (time.Duration, time.Duration) {
	span := 24 * time.Hour
	bucket := time.Hour
	switch r.URL.Query().Get("range") {
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
