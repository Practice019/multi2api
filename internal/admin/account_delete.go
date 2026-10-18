package admin

// account_delete.go —— 删除账号：出池 + 删凭证文件（用户报的"导入之后删除不掉"）。
//
// # 缺陷现象（用户 2026-10-19 报，附截图）
//
// 账号池里点「移除」，那几行**没消失 / 过一会儿又回来**。
//
// # 实测（合成数据探针 tests/frontend/verify_delete_flow_e2e.js，未用任何真实账号）
//
//	DELETE /admin/accounts/{uid}?purge_file=1
//	→ 200 {"file":"","purged_file":"","removed":true}
//	服务端日志：admin: remove … (purge_file=true deleted="")
//
// 两条独立的事实：
//
//	① loomy 的 `LoadCredentials` 投影**漏了 FilePath**（其余 9 个上游都带了）
//	   ⇒ 池条目上 `FilePath` 恒为空 ⇒ 这里 purge 静默空转：
//	     **账号出池了，文件还在盘上** ⇒ 下次「重载 auths」或重启，
//	     `LoadDir` 又把它装回池子 —— 用户读到的就是"删除不掉 / 删了又复活"。
//	② 回执当时**没有说"文件没删"** ⇒ 前端照旧 toast「已彻底删除」，
//	   界面在说谎。这与本仓反复强调的"宁可失败得刺眼，也不要成功得可疑"相反。
//
// # 本文件的两处修复
//
//	修①：FilePath 为空时，用**上游自己的扫描器**再定位一次
//	     （`locateCredentialFile`）—— 判据与导入/重载共用 credentialDirFor。
//	     兜底的价值不止修 loomy：池子从旧 state.json 恢复、上游将来又漏传，
//	     都会走这条路。
//	修②：没删成功就把原因写进回执（`purge_error`），前端据此如实提示。

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// accountDelete 把账号移出池；`purge_file=1` 时连凭证文件一起删。
//
// 默认只出池、保留文件是**遗留语义**（老客户端可能只发不带参数的 DELETE），
// 但前端早已一律带 `purge_file=1`：凭证文件就是账号在本机的实体，
// "只出池"是个半删除状态 —— 它只制造"复活"，不提供任何能力。
// 想"暂时停用"的正确形态是「禁用」（凭证保留、随时可启用）。
func (h *Handler) accountDelete(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	purge := r.URL.Query().Get("purge_file") == "1"

	provider, inPool := "", false
	if h.cfg.Pool != nil {
		provider, inPool = h.cfg.Pool.ProviderOf(uid)
	}
	if !inPool {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}

	filePath := ""
	if a := h.cfg.Pool.AuthByUID(uid); a != nil {
		filePath = strings.TrimSpace(a.FilePath)
	}
	// ⚠ 兜底：池条目上那份可能是空的（上游投影漏传 / 池子从旧 state.json 恢复）。
	//   删除是**低频人工操作**，多扫一次目录的代价可以忽略；
	//   而"找不到就静默不删"的代价是用户看着删掉的号又活过来。
	if purge && filePath == "" {
		if p := h.locateCredentialFile(provider, uid); p != "" {
			filePath = p
			log.Printf("admin: delete %s 池条目上没有 FilePath，按上游 %s 扫描找回: %s", uid, provider, p)
		}
	}

	h.cfg.Pool.Remove(uid)

	deleted, purgeErr := h.purgeCredentialFile(filePath, purge)
	log.Printf("admin: remove %s provider=%s (purge_file=%v deleted=%q err=%q)", uid, provider, purge, deleted, purgeErr)

	resp := map[string]any{"uid": uid, "removed": true, "purged_file": deleted, "file": filePath}
	if purgeErr != "" {
		// ⚠ 只写日志不算说清楚：前端据这个字段把「已彻底删除」换成如实提示。
		resp["purge_error"] = purgeErr
	}
	writeJSON(w, http.StatusOK, resp)
}

// locateCredentialFile 用**上游自己的扫描器**找出某个 uid 的凭证文件路径。
//
// 为什么不让核心去 Glob：凭证格式与命名都是**上游的事实**
// （`workbuddy*.json` / `loomy*.json` / codearts / …），核心猜不出也不该猜 ——
// 这正是 `gateway.CredentialLoader` 存在的理由。
//
// 目录判据与导入/重载共用 `credentialDirFor`（见 credentialdir.go）。
func (h *Handler) locateCredentialFile(provider, uid string) string {
	if h == nil || provider == "" || uid == "" {
		return ""
	}
	loader, ok := h.credentialsOf(provider)
	if !ok {
		return ""
	}
	dir := h.credentialDirFor(provider)
	creds, err := loader(dir)
	if err != nil {
		// 扫不动不是"删不掉"的理由，但必须留痕：否则又会变成一次静默空转。
		log.Printf("admin: delete %s 重扫上游 %s 的凭证目录失败（dir=%s）: %v", uid, provider, dir, err)
		return ""
	}
	for _, c := range creds {
		if c.UID == uid && strings.TrimSpace(c.FilePath) != "" {
			return strings.TrimSpace(c.FilePath)
		}
	}
	return ""
}

// purgeCredentialFile 删凭证文件，返回 (删掉的绝对路径, 失败原因)。
//
// ⚠ 失败原因**必须回给调用方**并写进回执：静默失败正是本次缺陷的核心 ——
// 池子里删掉了、文件还在，两处状态从此不一致，而界面说"已彻底删除"。
func (h *Handler) purgeCredentialFile(filePath string, purge bool) (string, string) {
	if !purge {
		return "", ""
	}
	if filePath == "" {
		return "", "上游没有报告这个账号的凭证文件路径 —— 文件没删，重载 auths 或重启后它会回到账号池"
	}

	// 只允许删**凭证树**（AuthsBase = 各上游子目录的父目录）内的文件：
	// 防止 FilePath 被构造成任意路径删除。早先用 h.cfg.AuthDir
	// （默认上游子目录 auths/workbuddy）做前缀，于是 loomy/trae
	// 的 auths/<上游>/ 文件永远前缀不匹配 → "拒绝删目录外文件" →
	// 用户删号后重启复活（实测 bug）。父目录才是所有上游的合法根。
	root := h.cfg.AuthsBase
	if root == "" {
		root = h.cfg.AuthDir
	}
	absDir, _ := filepath.Abs(root)
	absFile, _ := filepath.Abs(filePath)
	if !strings.HasPrefix(absFile, absDir+string(os.PathSeparator)) {
		log.Printf("admin: delete 拒绝删目录外文件 %s（凭证根 %s）", absFile, absDir)
		return "", "凭证文件不在凭证树内，已拒绝删除: " + filePath
	}

	if err := os.Remove(absFile); err != nil {
		if os.IsNotExist(err) {
			// 文件本来就不在 ⇒ "彻底删除"这个目标已经达成，不该报错。
			return absFile, ""
		}
		log.Printf("admin: delete 删文件失败 %s: %v", absFile, err)
		return "", "删文件失败: " + err.Error()
	}
	return absFile, ""
}
