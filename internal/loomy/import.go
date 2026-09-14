// import.go loomy 的**批量粘贴导入**端点。
//
// # 它解决什么问题（本轮用户的要求）
//
// 之前的添加账号路径是"本机拾取"或"手机号验证码"两条 —— 都要求**在网关这台
// 机器上**完成。而用户手上可能已经有一批**别处跑出来的账号 JSON**
// （截图里的形态：`[{phone, userid, session, nickname, ...}]`，可能几十条），
// 需要**直接粘贴进网页**一次导入。
//
// # 为什么它是 loomy 自己的端点，而不是改核心
//
// 凭证格式是**上游的事实**：只有 loomy 知道 `session` 是什么、`loomy-<uid>.json`
// 该怎么写。核心的落盘接口（`pollViaFlow`）要求凭证实现 `MarshalAuthFile`，
// 这里**复用同一套**（MarshalAuthFile + FileName），所以落盘出来的文件与
// 页内添加账号产出的**逐字节同形** —— 下游（重载 auths / 账号池）不用区分
// 它俩。
//
// # 导入后为什么还要"重载 auths"
//
// 本包**不能**碰账号池（架构约束：upstream 不得依赖 internal/pool）。
// 写入凭证文件后，池子的对齐动作交给核心的 `POST /admin/accounts/reload
// {provider:"loomy"}` —— 前端在导入成功后自动调一次，用户无需再手动点。
package loomy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// importPath 批量导入端点的路径。
const importPath = "/admin/loomy/import"

// importItem 一条输入记录的形态。
//
// 字段名与用户给出的 JSON **逐字对应**（`phone`/`userid`/`session`/`nickname`）。
// 额外的字段（isnew/haspwd/boundInvite/inviteCode/tasksDone…）**原样忽略**：
// 导入只负责"把 session 变成一份可用的凭证"，状态类信息以网关自己查询为准
// （免得把一份可能已经过期的快照当真）。
type importItem struct {
	Phone    string `json:"phone"`
	UserID   string `json:"userid"`
	UID      string `json:"uid"` // 容错：也认 uid 写法
	Session  string `json:"session"`
	Nickname string `json:"nickname"`
}

// handleImport POST /admin/loomy/import —— 批量粘贴导入。
//
// 请求体两种形状都认：
//
//	[{...}, {...}]        数组（多条）
//	{...}                 单个对象
//
// 逐条处理、逐条回报 —— 一条坏数据不拖累整批（与 LoadDir 的"单文件失败跳过"
// 同一条判据）。回执：
//
//	{ok, imported, failed, results:[{index, ok, uid?, error?}]}
func (p *Provider) handleImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取请求体失败"})
		return
	}
	trim := strings.TrimSpace(string(raw))
	if trim == "" {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体为空 —— 请粘贴账号 JSON"})
		return
	}

	var list []importItem
	switch trim[0] {
	case '[':
		if err := json.Unmarshal(raw, &list); err != nil {
			writeLoomyJSON(w, http.StatusBadRequest,
				map[string]any{"ok": false, "error": "不是合法的 JSON 数组: " + err.Error()})
			return
		}
	default:
		var one importItem
		if err := json.Unmarshal(raw, &one); err != nil {
			writeLoomyJSON(w, http.StatusBadRequest,
				map[string]any{"ok": false, "error": "不是合法的 JSON 对象: " + err.Error()})
			return
		}
		list = []importItem{one}
	}
	if len(list) == 0 {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "数组里没有任何账号"})
		return
	}

	// 目录先确保存在（与 writeAuthFile 同款 0700），避免逐条写时重复建。
	dir := p.authDir
	if strings.TrimSpace(dir) == "" {
		dir = "auths/loomy" // 装配层总会注入，这里只是兜底
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeLoomyJSON(w, http.StatusInternalServerError,
			map[string]any{"ok": false, "error": "创建凭证目录失败: " + err.Error()})
		return
	}

	results := make([]map[string]any, 0, len(list))
	okN := 0
	for i, it := range list {
		res := map[string]any{"index": i + 1}
		uid, ierr := importOne(dir, it)
		if ierr != nil {
			res["ok"] = false
			res["error"] = ierr.Error()
		} else {
			okN++
			res["ok"] = true
			res["uid"] = uid
		}
		results = append(results, res)
	}
	writeLoomyJSON(w, http.StatusOK, map[string]any{
		"ok":       okN == len(list),
		"imported": okN,
		"failed":   len(list) - okN,
		"results":  results,
	})
}

// importOne 把一条输入记录落盘成一份 loomy 凭证。
func importOne(dir string, it importItem) (string, error) {
	session := strings.TrimSpace(it.Session)
	if session == "" {
		return "", errors.New("缺少 session 字段（它是唯一鉴权材料）")
	}
	a := &Auth{
		Session:    session,
		Phone:      strings.TrimSpace(it.Phone),
		UserID:     strings.TrimSpace(firstNonEmpty(it.UserID, it.UID)),
		LoggedInAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	// UID 与 ParseCredential 同一条顺序：userid > 派生。
	a.UID = firstNonEmpty(a.UserID, deriveUID(session))
	if a.UID == "" {
		return "", errors.New("无法确定账号标识（userid 为空且 session 无法派生）")
	}
	a.Nickname = firstNonEmpty(strings.TrimSpace(it.Nickname), a.Phone, shortUID(a.UID))

	if !looksLikeSession(session) {
		// 与 ParseCredential 同款：只告警、不拒绝。
		log.Printf("loomy: 导入的凭证 uid=%s 的 session 不是 32 位小写 hex，仍按原样使用", shortUID(a.UID))
	}

	raw, err := MarshalAuthFile(a)
	if err != nil {
		return "", fmt.Errorf("序列化凭证失败: %w", err)
	}
	path := filepath.Join(dir, FileName(a))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", fmt.Errorf("写入凭证失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("落盘凭证失败: %w", err)
	}
	return a.UID, nil
}
