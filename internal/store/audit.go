// audit.go audit_log 的写入器与计数（Wave1-T07，承接 PLAN-sql-unify 的 T5）。
//
// # 本文件只做两件事：写一条、数一下
//
// 审计的**判断**（哪些方法算写操作、什么算成功、动作名叫什么）不在这里，而在
// 挂钩点 internal/admin 的 ServeHTTP —— 只有那里看得到 method / route / 状态码 /
// 响应体 / 身份。把判断混进 store，等于让「哪些请求留痕」变成数据层的事，
// 而数据层不该认识 HTTP。
//
// # 与 AppendLog 同一条失败纪律
//
// 审计写失败**绝不允许影响主流程**：调用方拿到 error 只记日志，不改变本次
// 请求的结果。见 internal/store/log.go 的 AppendLog 注释（同一条纪律的第二处）。
//
// # 为什么不存请求体 / 响应体 / query
//
// 同 models.go 的 AuditLog 注释：审计要能长期保留，而请求体里有用户内容与
// 可能的密钥。这里连「参数摘要」都不自动生成 —— 一旦顺手存一点，那份副本就
// 必须按同等强度保护，而它最容易出现在备份与导出里。调用方只能给**已脱敏**的
// 摘要（Params 字段由调用方决定填不填）。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// Category 的两个取值（对齐 models.go 的 AuditLog 注释里的集合：
// login / security / operation / access_token）。
//
// 本任务只产生这两个，其余留给后续（security 的安全事件、access_token 的
// 令牌读取审计）。**不要**在这里加一个「其它」兜底值：兜底会让「这张表里
// 每一行都能被归到一类」这个性质消失，而审计页正是按类别筛的。
const (
	AuditCategoryLogin     = "login"
	AuditCategoryOperation = "operation"
)

// auditEventIDBytes EventID 的随机字节数：hex 后 32 字符（列宽 varchar(64)，
// 见 models.go 的 AuditLog.EventID）。
const auditEventIDBytes = 16

// NewAuditEventID 生成一个全局唯一的审计事件 ID。
//
// # 为什么是随机数，而不是自增或时间戳
//
// 它要承担**幂等键**的职责（AuditLog.EventID 是 uniqueIndex）：同一次操作被
// 重试 / 重放时必须撞上同一个值 ⇒ 值只能由**发起方**在第一次就定下来。
//
//	自增    要求先查库再写，而「先读后写」正是 db.go 里 _txlock=immediate
//	        要治的那个形态（deferred 事务在并发下会拿到 SQLITE_BUSY_SNAPSHOT）；
//	时间戳  同一秒内的两次操作会撞车 ⇒ 第二次被幂等规则**静默丢掉**，
//	        而丢掉的恰恰是该审计的那条。
//
// crypto/rand 而不是 math/rand：审计 ID 可猜，等于把「伪造 / 覆盖自己那条」
// 变成可操作的事。
//
// ⚠ 失败时返回**空串**，不回落成时间戳：回落会把唯一性降级为「同一秒可能撞车」，
// 而撞车的后果是静默丢审计。AppendAudit 见到空串直接报错。
func NewAuditEventID() string {
	var b [auditEventIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// AppendAudit 写一条审计。
//
// # 幂等：EventID 已存在时静默忽略，不产生第二行
//
// 用 ON CONFLICT DO NOTHING，而不是「先查后写」：后者在并发下会两条都写进去
// （两边都查到没有），而这里要的正是「同一次操作只有一行」。用报错代替忽略也
// 不对：重放是**预期内**的（重试、代理重发、手工重跑），把预期内的事件记成
// 错误，会让调用方的错误日志淹掉真问题。
//
// EventID 为空时由本函数生成；At 为 0 时取当前时间。这两条是为了让调用方
// **不可能**造出「没有 ID」或「没有时间」的审计行 —— 前者退化成不可幂等，
// 后者让审计页的排序失去意义。
func AppendAudit(e AuditLog) error {
	if DB == nil {
		return ErrNotOpen
	}
	if e.EventID == "" {
		e.EventID = NewAuditEventID()
		if e.EventID == "" {
			return fmt.Errorf("store: 写 audit_log: 无法生成事件 ID（crypto/rand 失败）")
		}
	}
	if e.At == 0 {
		e.At = time.Now().Unix()
	}
	if err := DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "event_id"}},
		DoNothing: true,
	}).Create(&e).Error; err != nil {
		return fmt.Errorf("store: 写 audit_log: %w", err)
	}
	return nil
}

// CountAudit 审计总行数。
//
// 它是**二元行为判据的观测口**（「改一次渠道 base_url 后行数 +1」，见 PLAN
// Wave 1 的 T07），不是审计页的查询接口：页面要的是按时间倒序 + 类别筛选 +
// 分页，那是审计页自己的事（本任务不建页面，也不预先发明它的查询形态）。
func CountAudit() (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var n int64
	if err := DB.Model(&AuditLog{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("store: 数 audit_log: %w", err)
	}
	return n, nil
}
