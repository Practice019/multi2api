// state.go 本地状态文件的**落盘与读取**（U-10 的持久化段）。
//
// # 它存两样东西
//
//	SchemaVersion  这个库**已经被写成**的形态版本（不是二进制支持的版本）
//	Journal        迁移的分步提交日志（U-10 状态机的持久化）
//
// # 为什么"盘上的版本"与"二进制支持的版本"必须分开
//
// `LocalSchemaVersion` 是**能力**（本二进制能读到哪一版）；
// 盘上那个是**事实**（这个库实际已经写到哪一版）。
//
//	v1 库 + v2 二进制 ⇒ need_migration：能读，但新语义还没落地
//	v2 库 + v1 二进制 ⇒ binary_too_old：**拒绝启动**（v1 不认识新字段）
//
// 混成一个数就分不出"升级"与"降级"，而这两种的危险程度完全不同。
//
// # 为什么刷新与日志写在同一个文件
//
// 迁移要求"日志与真实写入在同一个停机窗口落盘"。分成两个文件就有了
// "日志写了、版本没写"（或反过来）的中间态，而那种中间态**无法推断**。
//
// 一个文件 + 一次原子替换 = 要么全旧要么全新。
package channelstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// InitialSchemaVersion 新库 / 引入本机制之前的库，打标时写的版本。
//
// ⚠ 它**不是** LocalSchemaVersion。v2 是"可选的解释"，不是"库已经写成 v2 了"。
// 直接标成 2 会让"迁移跑了一半"这件事永久不可见 —— 下次启动看到版本对得上，
// 就以为干净了。
const InitialSchemaVersion = 1

// PersistedState 状态文件的形状。
type PersistedState struct {
	SchemaVersion int               `json:"schema_version"`
	Journal       *MigrationJournal `json:"journal,omitempty"`
	UpdatedAt     string            `json:"updated_at,omitempty"`
}

// LoadState 读状态文件。
//
// 文件不存在 ⇒ 返回 (零值, false, nil) —— 那是"首次启动"，不是错误。
// 文件损坏 ⇒ 返回错误（**不许**当成"首次启动"静默覆盖：那会把日志一起丢掉）。
func LoadState(path string) (PersistedState, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return PersistedState{}, false, nil
		}
		return PersistedState{}, false, fmt.Errorf("读状态文件 %s: %w", path, err)
	}
	var st PersistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		// ⚠ 损坏不当首次启动：静默覆盖会把迁移日志一起丢掉，
		// 而日志是"哪些步骤已经写过"的唯一记录。
		return PersistedState{}, false, fmt.Errorf("状态文件 %s 损坏（不静默覆盖，请人工检查）: %w", path, err)
	}
	return st, true, nil
}

// SaveState 原子写状态文件（临时文件 + rename）。
//
// 原子性在这里不是洁癖：写一半的文件等于把版本与日志同时破坏，
// 而这两样都没有第二个副本。
func SaveState(path string, st PersistedState) error {
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomicLocal(path, raw)
}

// writeFileAtomicLocal 临时文件 + rename 的原子写。
//
// ⚠ 不用"先删再写"：删了之后写到一半失败，文件就**没了** ——
// 而状态文件没有第二个副本（版本与迁移日志都在里面）。
//
// 同目录的临时文件是必须的：跨目录 rename 不是原子操作。
func writeFileAtomicLocal(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // 失败路径清掉残留

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// 先落盘再 rename：否则崩溃后可能得到一个"长度为 0 或半截"的新文件。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// MarkInitial 给一个没有标记的库打上初始版本。
//
// 保留已有日志（如果有），只补版本。
func MarkInitial(path string) (PersistedState, error) {
	st, found, err := LoadState(path)
	if err != nil {
		return PersistedState{}, err
	}
	if found && st.SchemaVersion != 0 {
		return st, nil // 已经有标记，别覆盖
	}
	st.SchemaVersion = InitialSchemaVersion
	if err := SaveState(path, st); err != nil {
		return PersistedState{}, err
	}
	return st, nil
}

// StartupGuard 启动时的版本门禁：读状态 → 判定 → （必要时）打标。
//
// 返回值：
//
//	compat  判定结论（调用方据此决定是否 `log.Fatal(c.Error())`）
//	state   读到的（或刚打标的）状态
//
// ⚠ 顺序很重要：**先判定再打标**。反过来会把"盘上是 v3"先覆盖成 v1，
// 于是"数据比二进制新"这个事实被自己抹掉了。
func StartupGuard(path string) (DataCompatibility, PersistedState, error) {
	st, found, err := LoadState(path)
	if err != nil {
		return DataCompatibility{}, PersistedState{}, err
	}
	compat := CheckCompatibility(st.SchemaVersion, !found)

	// 只有"没有标记"才打标。其余一律不动 —— 包括 binary_too_old：
	// 那种情况下连写都不该写（写就是拿旧代码改新数据）。
	if compat.Verdict == CompatUnmarked {
		st, err = MarkInitial(path)
		if err != nil {
			return compat, st, err
		}
	}
	return compat, st, nil
}
