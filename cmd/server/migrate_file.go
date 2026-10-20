// cmd/server/migrate_file.go —— 一次性文件迁移（2026-10-20）
//
// # 为什么单独一个文件
//
// 这里只有一个函数，但它承载一条**不可逆**的数据操作（把渠道与 API key 从
// `data/channels.json` 搬到 `<凭据根>/channels.json`），所以：
//   - 独立成文件，便于单测直接覆盖
//   - 不塞进 main.go 的启动流程里（那里已经有 1200+ 行）
//
// # 语义（每条都关系到数据安全）
//
//  1. **目标已存在 ⇒ 什么都不做**。绝不能覆盖 —— 那会抹掉用户在新位置之后的修改。
//  2. **源不存在 ⇒ 什么都不做，也不报错**。首次部署没有旧文件是正常状态。
//  3. **父目录不存在 ⇒ 先建**。`auths/` 可能还没生成（全新机器）。
//  4. **先写 .tmp 再 rename**。中途崩溃不能留下半截文件 —— 那比"没迁"更糟，
//     因为下次启动会认为目标已存在而跳过迁移。
//  5. **不删除源文件**。保留回滚路径：旧版 exe 回退时仍能读自己的数据。
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// migrateFileIfAbsent 把 src 复制到 dst，仅当 dst 不存在且 src 存在。
//
// 返回 nil 代表"不需要做"或"做成了"，调用方无需区分 —— 但**真正的 IO 失败**
// 会返回 error，让调用方决定是记日志继续还是中止。
func migrateFileIfAbsent(src, dst string) error {
	if dst == "" {
		return fmt.Errorf("迁移目标路径为空")
	}
	// ① 目标已存在 ⇒ 不覆盖（幂等的前提）
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查目标 %s: %w", dst, err)
	}
	// ② 源不存在 ⇒ 无事可做
	srcInfo, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("检查源 %s: %w", src, err)
	}
	if srcInfo.IsDir() {
		return fmt.Errorf("源 %s 是目录，不是文件", src)
	}
	// ③ 父目录
	if dir := filepath.Dir(dst); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("建目录 %s: %w", dir, err)
		}
	}
	// ④ 读源 → 写 .tmp → rename
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读源 %s: %w", src, err)
	}
	tmp := dst + ".tmp"
	// 0o600：这个文件里有 API key，权限必须与账号凭证同级
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写临时文件 %s: %w", tmp, err)
	}
	// 落盘后再改名：rename 在同一文件系统内是原子的
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp) // 清掉半成品
		return fmt.Errorf("改名 %s → %s: %w", tmp, dst, err)
	}
	return nil
}

// 让 io 的导入不被 goimports 删掉（本文件目前没直接用到 io，
// 但保留是为了将来加"流式拷贝大文件"时不必改 import 块）。
var _ = io.Discard
