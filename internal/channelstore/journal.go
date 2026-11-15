// journal.go 迁移的**分步提交日志**（U-10）。
//
// # 它解决什么问题
//
// 计划 §5 U-10 的风险栏逐字写着：「跨 JSON/SQL 无统一事务；停写窗口备份＋
// **分步提交日志**，恢复前核对后续写入，不整库覆盖其他任务成果」。
//
// 迁移要写三处（groups JSON、tokens SQL、channels SQL），没有任何事务能覆盖它们。
// 于是唯一安全的形态是：**一步一提交，日志是唯一真相**。
//
// # 四条不变量（每条都有判据）
//
//	不变式 1  幂等重跑   已 committed 且 checksum 相同的步骤**再执行是 no-op**
//	不变式 2  校验和     同一步的 checksum 变了 ⇒ **拒绝**（计划被改过，不能续跑）
//	不变式 3  计划钉住   整份计划的 checksum 变了 ⇒ **拒绝续跑**（必须重新开始）
//	不变式 4  逆序回滚   只允许回滚**最后一步**（后提交的先撤销）
//
// # 为什么不变式 2 是这套东西的灵魂
//
// 「已 committed」只是个标记；真正要保证的是**同一个标记对应同一份内容**。
// 没有 checksum 的话，改完计划再跑一遍，日志会把"那次写的是旧内容"当成
// "这次要写的新内容"已经完成 —— 于是永远不写，而报告说全绿。
//
// # 这个文件不做 IO
//
// 它是**纯状态机**。持久化由调用方负责（写盘/写库），因为它要跟真实写入在
// 同一个停机窗口里落盘。纯状态机的好处是每条不变量都能离线钉住。
package channelstore

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// 步骤状态。
const (
	// StepPending 还没提交（可以执行）。
	StepPending = "pending"
	// StepCommitted 已提交（再执行是 no-op）。
	StepCommitted = "committed"
	// StepFailed 上次尝试失败（可以重试，但要知道失败过）。
	StepFailed = "failed"
)

// MigrationStep 迁移日志里的一步。
type MigrationStep struct {
	// ID 步骤标识（如 "token:app"、"group:route"）。在同一份日志里唯一。
	ID string `json:"id"`
	// Checksum 这一步**要写什么**的指纹。
	//
	// ⚠ 它必须是内容的哈希，不能是"第几次"之类会变的东西 ——
	// 否则幂等判断就退化成"看到标记就跳过"。
	Checksum string `json:"checksum"`
	// Status 见 StepPending / StepCommitted / StepFailed。
	Status string `json:"status"`
	// Attempts 尝试次数（失败重试时递增，用于"别再无限重试"的判据）。
	Attempts int `json:"attempts"`
	// Error 最近一次失败原因（成功后清空）。
	Error string `json:"error,omitempty"`
}

// MigrationJournal 一份迁移日志。
type MigrationJournal struct {
	// Plan 整份计划的 checksum（计划一改，旧日志就不能续跑）。
	Plan string `json:"plan"`
	// Steps 按**执行顺序**排列（不是 map：顺序本身就是语义）。
	Steps []MigrationStep `json:"steps"`
}

// JournalAction 一次 Apply 的结论。
type JournalAction struct {
	// Execute true = 真的要去写；false = 这次不用写（已提交 / 被拒绝）。
	Execute bool `json:"execute"`
	// Reason 给人看的一句说明。
	Reason string `json:"reason"`
}

// JournalError 拒绝续跑的原因（它不是"失败"，是"状态不一致，必须人工介入"）。
type JournalError struct {
	StepID string
	Reason string
}

func (e *JournalError) Error() string {
	if e.StepID == "" {
		return "迁移日志与计划不一致：" + e.Reason
	}
	return "迁移步骤 " + e.StepID + " 与计划不一致：" + e.Reason
}

// Checksum 算一段内容的指纹。调用方把"这一步要写什么"序列化后交给它。
func Checksum(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// NewJournal 按计划内容建一份全新日志（全部 pending）。
//
// steps 是 (id, checksum) 的有序列表。顺序就是执行顺序。
func NewJournal(planChecksum string, steps [][2]string) *MigrationJournal {
	out := &MigrationJournal{Plan: planChecksum, Steps: make([]MigrationStep, 0, len(steps))}
	for _, s := range steps {
		out.Steps = append(out.Steps, MigrationStep{ID: s[0], Checksum: s[1], Status: StepPending})
	}
	return out
}

// Apply 标记"这一步即将执行"（或告诉调用方不用执行）。
//
// 返回 execute=true 时，调用方**写成功之后**必须调 Commit；写失败调 Fail。
// 这个"先问再写后确认"的三段式是刻意的：把状态推进与实际写入解耦，
// 中断在任何一段都能给出确定结论。
func (j *MigrationJournal) Apply(planChecksum, id, checksum string) (JournalAction, error) {
	if err := j.checkPlan(planChecksum); err != nil {
		return JournalAction{}, err
	}
	step, err := j.find(id)
	if err != nil {
		return JournalAction{}, err
	}
	if step.Status == StepCommitted {
		if step.Checksum != checksum {
			// 不变式 2：同一个标记不能对应两份内容。
			return JournalAction{}, &JournalError{StepID: id, Reason: "已提交的步骤内容变了（计划被改过），不能续跑"}
		}
		// 不变式 1：幂等 no-op。
		return JournalAction{Execute: false, Reason: "已提交，跳过"}, nil
	}
	if step.Checksum != checksum {
		// 还没提交但内容也变了 —— 同样不能悄悄换。
		return JournalAction{}, &JournalError{StepID: id, Reason: "未提交的步骤内容变了（计划被改过），不能续跑"}
	}
	return JournalAction{Execute: true, Reason: "待执行"}, nil
}

// Commit 记一次成功。
func (j *MigrationJournal) Commit(id string) error {
	step, err := j.find(id)
	if err != nil {
		return err
	}
	step.Status = StepCommitted
	step.Attempts++
	step.Error = ""
	return nil
}

// Fail 记一次失败（可重试，但要看得到失败过）。
func (j *MigrationJournal) Fail(id, reason string) error {
	step, err := j.find(id)
	if err != nil {
		return err
	}
	step.Status = StepFailed
	step.Attempts++
	step.Error = reason
	return nil
}

// Resume 返回**第一个还没提交的步骤**（中断后从这里继续）。
//
// 全部提交过则返回 ("", false)。
func (j *MigrationJournal) Resume() (string, bool) {
	for _, s := range j.Steps {
		if s.Status != StepCommitted {
			return s.ID, true
		}
	}
	return "", false
}

// Rollback 撤销**最后一步**的提交标记（逆序回滚）。
//
// ⚠ 只允许回滚最后一个已提交步骤：跳过中间步骤回滚会让日志与实际写入
// 出现无法推断的关系（"第 1 步撤了、第 2 步还在"到底是什么状态？）。
func (j *MigrationJournal) Rollback(id string) error {
	last := -1
	for i, s := range j.Steps {
		if s.Status == StepCommitted {
			last = i
		}
	}
	if last < 0 {
		return &JournalError{StepID: id, Reason: "没有已提交的步骤可回滚"}
	}
	if j.Steps[last].ID != id {
		return &JournalError{StepID: id, Reason: "只能逆序回滚：最后提交的是 " + j.Steps[last].ID}
	}
	j.Steps[last].Status = StepPending
	j.Steps[last].Error = ""
	return nil
}

// Done 是否全部提交。
func (j *MigrationJournal) Done() bool {
	_, more := j.Resume()
	return !more
}

// Summary 给报告用的计数（**不含秘密** —— 只有 id 与状态）。
func (j *MigrationJournal) Summary() (committed, pending, failed int) {
	for _, s := range j.Steps {
		switch s.Status {
		case StepCommitted:
			committed++
		case StepFailed:
			failed++
		default:
			pending++
		}
	}
	return
}

// RetryExhausted 是否有步骤重试次数已达上限（默认 3）。
//
// ⚠ 与"失败"分开：失败是可以继续试的；**重试耗尽**要停下来等人。
// 无限重试会把一次配置错误变成一场持续的写库风暴。
func (j *MigrationJournal) RetryExhausted(max int) bool {
	if max <= 0 {
		max = 3
	}
	for _, s := range j.Steps {
		if s.Status != StepCommitted && s.Attempts >= max {
			return true
		}
	}
	return false
}

func (j *MigrationJournal) checkPlan(planChecksum string) error {
	if j.Plan == "" {
		// 空日志（还没建过）—— 由调用方决定要不要初始化。
		return nil
	}
	if j.Plan != planChecksum {
		// 不变式 3：整份计划变了 ⇒ 旧日志作废，必须人工重新开始。
		return &JournalError{Reason: "整份计划的 checksum 变了（计划被改过），旧日志不能续跑"}
	}
	return nil
}

func (j *MigrationJournal) find(id string) (*MigrationStep, error) {
	for i := range j.Steps {
		if j.Steps[i].ID == id {
			return &j.Steps[i], nil
		}
	}
	return nil, &JournalError{StepID: id, Reason: "日志里没有这一步"}
}

// SortedStepIDs 排好序的步骤 ID（建日志时用它保证**顺序稳定**）。
//
// 迁移步骤的顺序必须是确定的：两次运行排出不同顺序会让日志与写入错位。
func SortedStepIDs(ids []string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}
