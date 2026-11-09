package store

// log_daily.go —— 日级汇总表（T-386）。
//
// # 为什么需要它：让 365 天热力图有数据，同时把存储压下来
//
// 原始日志（logs 表）只为**排障**服务，几天就够；而"某天 24 次请求 / 2.7k token"
// 这种**聚合值**是历史不变量 —— 过去那天的数字不会再变，必须永久保留。
//
//	原始日志：400+ MB / 年（且会无限增长）
//	日聚合  ：365 行 × ~120 字节 ≈ 43 KB / 年
//
// ⇒ 于是分成两层：
//
//	logs      明细，排障用，按天裁剪（保留期见 config.RequestLogKeepDays）
//	log_daily 日聚合，趋势/热力图用，**永不裁剪**
//
// # 与 series.go 的口径必须一致
//
// Day 是**本地自然日**字符串（'2006-01-02'）。series.go 用 bucketStart 按本地自然日对齐，
// 那里的注释记过一个坑：time.Truncate 是 UTC 基准，东八区会整体偏 8 小时 ——
// 所以这里也一律用 t.In(time.Local).Format("2006-01-02")，不用 Truncate。
// SQL 侧对应 date(created_at,'unixepoch','localtime')。

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
)

// LogDaily 一天一行。字段名与 JSON 形状对齐 statsSeriesItem（前端已按它写死）。
type LogDaily struct {
	Day       string  `gorm:"primaryKey;type:varchar(10)" json:"day"` // '2026-11-09'（本地自然日）
	Total     int64   `gorm:"not null;default:0" json:"total"`
	OK        int64   `gorm:"not null;default:0" json:"ok"`
	Fail      int64   `gorm:"not null;default:0" json:"fail"`
	Tokens    int64   `gorm:"not null;default:0" json:"tokens"`
	Cost      float64 `gorm:"not null;default:0" json:"cost"`
	CacheHit  int64   `gorm:"not null;default:0" json:"cache_hit"`
	CacheMiss int64   `gorm:"not null;default:0" json:"cache_miss"`
	// ByModel 按模型的 token 用量（JSON 文本，与 statsSeriesItem.ByModel 同形状）。
	//
	// 为什么存 JSON 而不是另开一张 (day, model) 表：一天一行、查询时整体读出即可；
	// 拆表会让"读 365 天"从 1 次查询变成 365 次 JOIN 或一次大 GROUP BY。
	ByModel string `gorm:"type:text;not null;default:''" json:"by_model"`
	// ByModelOut 本日按模型的**纯输出** token（JSON）。
	//
	// # 为什么必须与 ByModel 分开
	// ByModel 是 total（输入+输出），而用户看"消耗分布"时以为是"模型生成了多少字"。
	// 实测差距极大：某条请求 total=37938 而输出仅 176（其余都是缓存读取的上下文）。
	// 两个语义混在一起等于撒谎。
	// ⚠ 与 statsSeriesItem.ByModelOut 同形状，前端直接消费。
	ByModelOut string `gorm:"type:text;not null;default:''" json:"by_model_out"`
}

// TableName 见 db_test.go 的「有 TableName() 就必进 allModels」守卫。
func (LogDaily) TableName() string { return "log_daily" }

// DayOf 把一个时间点换算成日聚合用的日期键（**本地自然日**）。
func DayOf(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

// AppendLogDaily 把一条日志**增量累加**进当天那一行。
//
// # 为什么增量而不是"零点跑批"
//
// 跑批会丢"当天正在累积"的那半天 —— 而热力图是实时刷新的，用户当天就该看到格子变亮。
//
// # 原子性
//
// 计数列用 INSERT ... ON CONFLICT(day) DO UPDATE SET x = x + excluded.x 一条语句累加；
// 但 by_model 是 JSON，必须读-改-写 ⇒ 整个过程包在一个事务里（否则并发写会丢模型计数）。
//
// # 失败不影响请求路径
//
// 调用方（sqlLogWriter）本来就是异步队列，这里的错误只会上报给队列记日志，不重试、不阻塞。
func AppendLogDaily(day string, ok bool, tokens, outTokens, cacheHit, cacheMiss int64, cost float64, model string) error {
	if DB == nil {
		return ErrNotOpen
	}
	if day == "" {
		day = DayOf(time.Now())
	}
	var okN, failN int64
	if ok {
		okN = 1
	} else {
		failN = 1
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Exec(`INSERT INTO log_daily
			(day, total, ok, fail, tokens, cost, cache_hit, cache_miss, by_model)
			VALUES (?, 1, ?, ?, ?, ?, ?, ?, '')
			ON CONFLICT(day) DO UPDATE SET
				total      = total      + 1,
				ok         = ok         + excluded.ok,
				fail       = fail       + excluded.fail,
				tokens     = tokens     + excluded.tokens,
				cost       = cost       + excluded.cost,
				cache_hit  = cache_hit  + excluded.cache_hit,
				cache_miss = cache_miss + excluded.cache_miss`,
			day, okN, failN, tokens, cost, cacheHit, cacheMiss).Error
		if err != nil {
			return fmt.Errorf("store: 累加 log_daily: %w", err)
		}
		if model == "" {
			return nil
		}
		// 两个 JSON 列都读-改-写（同一事务内 ⇒ 不会与并发累加互相覆盖）
		if err := bumpModelJSON(tx, day, "by_model", model, tokens); err != nil {
			return err
		}
		return bumpModelJSON(tx, day, "by_model_out", model, outTokens)
	})
}

// bumpModelJSON 把 col 这一列（JSON 文本）里 model 的值累加 delta。
//
// 抽出来的原因：by_model 与 by_model_out 是同一套"读-改-写"逻辑，写两遍必然漂移。
// 必须在**同一个事务**里调用（否则并发写会丢计数）。
func bumpModelJSON(tx *gorm.DB, day, col, model string, delta int64) error {
	if model == "" || delta == 0 {
		return nil
	}
	var cur string
	if err := tx.Raw("SELECT "+col+" FROM log_daily WHERE day = ?", day).Scan(&cur).Error; err != nil {
		return fmt.Errorf("store: 读 log_daily.%s: %w", col, err)
	}
	m := map[string]int64{}
	if cur != "" {
		if err := json.Unmarshal([]byte(cur), &m); err != nil {
			// 脏 JSON 不清空也不报错：重建一份，避免一条坏数据把当天所有累加卡住
			log.Printf("store: log_daily.%s 解析失败 day=%s（重建）：%v", col, day, err)
			m = map[string]int64{}
		}
	}
	m[model] += delta
	b, merr := json.Marshal(m)
	if merr != nil {
		return fmt.Errorf("store: 序列化 log_daily.%s: %w", col, merr)
	}
	return tx.Exec("UPDATE log_daily SET "+col+" = ? WHERE day = ?", string(b), day).Error
}

// BackfillLogDaily 从 logs 表回填日聚合。**幂等**：可以每次启动都跑。
//
// # 为什么以 logs 为准（覆盖式，而不是与 log_daily 取大）
//
// 上线前写进 logs 的历史没有日聚合；不回填的话用户的 365 天热力图只有上线后的日期。
// 而 logs 是**明细真相**、log_daily 只是它的聚合缓存 ⇒ 同一天两者不一致时，
// 以 logs 重算出的值为准（增量累加可能只覆盖了当天的一部分，重启后重算正好补齐）。
//
// 返回回填的天数（便于日志与测试断言）。
func BackfillLogDaily() (int, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var cnt int64
	if err := DB.Raw(`SELECT COUNT(*) FROM logs`).Scan(&cnt).Error; err != nil {
		return 0, fmt.Errorf("store: 回填前统计 logs: %w", err)
	}
	if cnt == 0 {
		return 0, nil // 新部署：无事可做
	}
	// ① 计数列
	err := DB.Exec(`INSERT INTO log_daily
			(day, total, ok, fail, tokens, cost, cache_hit, cache_miss, by_model)
		SELECT date(created_at,'unixepoch','localtime') AS day,
		       COUNT(*),
		       SUM(CASE WHEN success THEN 1 ELSE 0 END),
		       SUM(CASE WHEN success THEN 0 ELSE 1 END),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cost_quota), 0),
		       -- T-387：logs 现在有 cache 两列（AutoMigrate 只加不删，旧库零风险）。
		       -- ⚠ T-387 之前写的历史行是 0 ⇒ 回填出来也是 0，语义正确表示"当时没记录"。
		       COALESCE(SUM(cache_hit_tokens), 0),
		       COALESCE(SUM(cache_miss_tokens), 0),
		       ''
		FROM logs
		GROUP BY day
		ON CONFLICT(day) DO UPDATE SET
			total      = excluded.total,
			ok         = excluded.ok,
			fail       = excluded.fail,
			tokens     = excluded.tokens,
			cost       = excluded.cost,
			cache_hit  = excluded.cache_hit,
			cache_miss = excluded.cache_miss`).Error
	if err != nil {
		return 0, fmt.Errorf("store: 回填 log_daily 计数列: %w", err)
	}
	// ② by_model（按天 × 模型再聚合一次，然后按天拼 JSON）
	type row struct {
		Day   string
		Model string
		Tok   int64
		Out   int64
	}
	var rows []row
	if err := DB.Raw(`SELECT date(created_at,'unixepoch','localtime') AS day,
			model_name AS model, COALESCE(SUM(total_tokens),0) AS tok,
			COALESCE(SUM(completion_tokens),0) AS out
		FROM logs WHERE model_name IS NOT NULL AND model_name <> ''
		GROUP BY day, model`).Scan(&rows).Error; err != nil {
		return 0, fmt.Errorf("store: 回填 log_daily 分组: %w", err)
	}
	perDay := map[string]map[string]int64{}
	perDayOut := map[string]map[string]int64{}
	for _, r := range rows {
		if perDay[r.Day] == nil {
			perDay[r.Day] = map[string]int64{}
		}
		perDay[r.Day][r.Model] += r.Tok
		if perDayOut[r.Day] == nil {
			perDayOut[r.Day] = map[string]int64{}
		}
		perDayOut[r.Day][r.Model] += r.Out
	}
	for day, m := range perDay {
		b, merr := json.Marshal(m)
		if merr != nil {
			continue
		}
		if uerr := DB.Exec(`UPDATE log_daily SET by_model = ? WHERE day = ?`, string(b), day).Error; uerr != nil {
			return 0, fmt.Errorf("store: 回填 log_daily.by_model(%s): %w", day, uerr)
		}
		if om := perDayOut[day]; om != nil {
			if ob, oerr := json.Marshal(om); oerr == nil {
				if uerr := DB.Exec(`UPDATE log_daily SET by_model_out = ? WHERE day = ?`, string(ob), day).Error; uerr != nil {
					return 0, fmt.Errorf("store: 回填 log_daily.by_model_out(%s): %w", day, uerr)
				}
			}
		}
	}
	var days int64
	if err := DB.Raw(`SELECT COUNT(*) FROM log_daily`).Scan(&days).Error; err != nil {
		return 0, fmt.Errorf("store: 回填后统计 log_daily: %w", err)
	}
	return int(days), nil
}

// PruneLogs 裁掉保留期之外的 **logs 明细**。
//
// ⚠ 绝不碰 log_daily —— 日聚合是历史不变量，裁掉等于把 365 天热力图擦白。
// ⚠ 保留期下限 7 天：排障至少要一周（配置里小于 7 也按 7 处理）。
func PruneLogs(keepDays int) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	if keepDays < 7 {
		keepDays = 7
	}
	cut := time.Now().AddDate(0, 0, -keepDays).Unix()
	res := DB.Exec(`DELETE FROM logs WHERE created_at < ?`, cut)
	if res.Error != nil {
		return 0, fmt.Errorf("store: 裁剪 logs: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// LoadLogDaily 读取 [fromDay, toDay] 范围内的日聚合（按天升序）。
//
// fromDay/toDay 形如 '2006-01-02'，字符串比较即可（定长 + 字典序 = 时间序）。
func LoadLogDaily(fromDay, toDay string) ([]LogDaily, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var rows []LogDaily
	err := DB.Where("day >= ? AND day <= ?", fromDay, toDay).Order("day ASC").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: 读 log_daily: %w", err)
	}
	return rows, nil
}
