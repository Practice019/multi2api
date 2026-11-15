// migration_run.go 把「计划 + 提交日志 + 真实写入」连起来（U-09 执行机器）。
//
// # 它做什么，不做什么
//
// 做：把一份 `MigrationPlan` 变成一串**可幂等重跑**的步骤，按日记账推进，
// 写成功才记提交，写失败就停下。
//
// 不做：**不自己启动**。没有"启动时自动跑一遍"的入口 ——
// 执行迁移必须是操作者显式按下的动作（见 cmd 侧）。本文件只提供机器。
//
// # 为什么写入是一个接口
//
// `MigrationWriter` 让整条链路能在**没有真实库**的情况下被完整钉住：
// 假写入器可以注入失败、记录调用、验证幂等。若把写库直接写死在这里，
// 那么"写失败之后日志是什么状态"这条就只能靠人肉演练 —— 那是不可接受的。
//
// # 三条硬规则
//
//	① **计划不 Ready 就一步都不走**（有阻塞项时执行 = 带病迁移）
//	② **写失败立即停**（继续往下会让"逆序回滚"失去意义：后面的步骤
//	   建立在前面已提交的前提上，而那个前提已经不成立了）
//	③ **幂等**：已提交且 checksum 相同的步骤是 no-op（重跑不会重复写）
package channelstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// TokenPolicyWrite 一次"写某个令牌的 v2 字段"的内容。
//
// ⚠ 刻意只有这两个字段：迁移只改**权限表达**，不改归属、不改额度、
// 不改密钥。字段越少，一次误写能造成的破坏越小。
type TokenPolicyWrite struct {
	// ModelLimitsEnabled 是否启用白名单。
	ModelLimitsEnabled bool
	// ModelLimits 白名单集合（启用时有效）。
	ModelLimits []string
}

// MigrationWriter 迁移的真实写入点。实现方负责落库。
//
// ⚠ **必须幂等**：同一个 `(name, write)` 写两次的结果必须与一次相同。
// 幂等的责任在实现方 —— 因为提交日志能保证"多数情况只写一次"，
// 但中断恢复的边界上（写完、还没记提交就崩了）一定会出现"写了两次"。
type MigrationWriter interface {
	WriteTokenPolicy(name string, write TokenPolicyWrite) error
}

// MigrationRunReport 一次执行的结果。
type MigrationRunReport struct {
	// Executed 本次真的执行并提交的步骤。
	Executed []string `json:"executed"`
	// Skipped 本次跳过的步骤（已提交 / 该项被阻塞）。
	Skipped []string `json:"skipped"`
	// Failed 失败的那一步（空 = 没有失败）。
	Failed string `json:"failed,omitempty"`
	// FailedReason 失败原因。
	FailedReason string `json:"failed_reason,omitempty"`
	// Blocked 整份计划不可执行的原因（非空 ⇒ 一步都没走）。
	Blocked string `json:"blocked,omitempty"`
	// Stopped 是否因失败而提前停止。
	Stopped bool `json:"stopped,omitempty"`
}

// MigrationRunner 把一份计划和它的日志绑在一个写入器上。
type MigrationRunner struct {
	plan    MigrationPlan
	journal *MigrationJournal
	writer  MigrationWriter
}

// PlanChecksum 整份计划的内容指纹。
//
// 用途：计划一改，旧日志就不能续跑（否则"那次写的是旧计划"会被当成
// "这次要写的新计划"已完成）。它**不**覆盖 Blocked/Note 这类说明字段 ——
// 那些改了不影响要写什么。
func PlanChecksum(plan MigrationPlan) string {
	type item struct {
		Name              string   `json:"name"`
		Pinned            []string `json:"pinned,omitempty"`
		WhitelistDisabled bool     `json:"whitelist_disabled,omitempty"`
	}
	payload := make([]item, 0, len(plan.Items))
	for _, it := range plan.Items {
		if it.Blocked != "" {
			continue // 阻塞项不参与执行，也不该让整份计划失效
		}
		payload = append(payload, item{
			Name:              it.Name,
			Pinned:            it.PinnedWhitelist,
			WhitelistDisabled: it.WhitelistDisabled,
		})
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// StepID 一个计划项对应的步骤标识。
//
// ⚠ 用**名字**而不是下标：下标会随无关令牌的增删而漂移，
// 而"第 3 步已提交"这种记录一旦指错对象就是灾难。
func (i MigrationPlanItem) StepID() string { return "token:" + i.Name }

// StepChecksum 一个计划项"要写什么"的指纹。
func (i MigrationPlanItem) StepChecksum() string {
	w := i.Write()
	raw, _ := json.Marshal(w)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Write 把计划项翻译成一次写入。
func (i MigrationPlanItem) Write() TokenPolicyWrite {
	if i.WhitelistDisabled {
		// 旧解释是"不限" ⇒ v2 表达不了 ⇒ 关闭白名单（由人确认过）。
		return TokenPolicyWrite{ModelLimitsEnabled: false, ModelLimits: nil}
	}
	return TokenPolicyWrite{
		ModelLimitsEnabled: true,
		ModelLimits:        append([]string{}, i.PinnedWhitelist...),
	}
}

// NewMigrationRunner 建一个执行器。
//
// 若 `journal` 为 nil，则按计划新建一份（全部 pending）。
func NewMigrationRunner(plan MigrationPlan, journal *MigrationJournal, w MigrationWriter) (*MigrationRunner, error) {
	if w == nil {
		return nil, fmt.Errorf("迁移执行器缺少写入器")
	}
	// 步骤 ID 必须唯一 —— 重名会让"某个令牌已提交"指错对象。
	seen := make(map[string]bool, len(plan.Items))
	for _, it := range plan.Items {
		if it.Blocked != "" {
			continue
		}
		id := it.StepID()
		if seen[id] {
			return nil, fmt.Errorf("迁移计划里有重名的步骤 %q（同名令牌无法区分）", id)
		}
		seen[id] = true
	}
	if journal == nil {
		steps := make([][2]string, 0, len(plan.Items))
		for _, it := range plan.Items {
			if it.Blocked != "" {
				continue
			}
			steps = append(steps, [2]string{it.StepID(), it.StepChecksum()})
		}
		journal = NewJournal(PlanChecksum(plan), steps)
	}
	return &MigrationRunner{plan: plan, journal: journal, writer: w}, nil
}

// Journal 暴露日志（调用方要负责持久化它）。
func (r *MigrationRunner) Journal() *MigrationJournal { return r.journal }

// RunAll 执行整份计划。
//
// 规则见文件头：不 Ready 不走、写失败立停、已提交的 no-op。
func (r *MigrationRunner) RunAll() MigrationRunReport {
	out := MigrationRunReport{Executed: []string{}, Skipped: []string{}}

	// ① 计划不 Ready ⇒ 一步都不走。
	if !r.plan.Ready {
		out.Blocked = "计划未就绪（Blocked=" + fmt.Sprint(r.plan.Blocked) +
			"）：有阻塞项时执行等于带病迁移"
		return out
	}

	planSum := PlanChecksum(r.plan)
	for _, it := range r.plan.Items {
		id := it.StepID()
		if it.Blocked != "" {
			// 逐项阻塞：跳过，不失败。整份 Ready 已经保证不会走到这里，
			// 但保留这条是为了让"将来放宽 Ready 判据"时仍然安全。
			out.Skipped = append(out.Skipped, id)
			continue
		}

		act, err := r.journal.Apply(planSum, id, it.StepChecksum())
		if err != nil {
			// 计划/校验和漂移 —— 这不是"某一步失败"，是"日志与计划不一致"，
			// 必须整体停下等人工介入（不能当成普通失败继续）。
			out.Failed = id
			out.FailedReason = err.Error()
			out.Stopped = true
			return out
		}
		if !act.Execute {
			out.Skipped = append(out.Skipped, id)
			continue
		}

		if werr := r.writer.WriteTokenPolicy(it.Name, it.Write()); werr != nil {
			// ② 写失败立停。
			_ = r.journal.Fail(id, werr.Error())
			out.Failed = id
			out.FailedReason = werr.Error()
			out.Stopped = true
			return out
		}
		if cerr := r.journal.Commit(id); cerr != nil {
			out.Failed = id
			out.FailedReason = cerr.Error()
			out.Stopped = true
			return out
		}
		out.Executed = append(out.Executed, id)
	}
	return out
}
