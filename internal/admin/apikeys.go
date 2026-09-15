// apikeys.go —— /admin/apikeys 管理端点（对标 new-api 令牌管理，单机简化版）。
//
// # 为什么端点长在核心 admin 而不是上游
//
// API key 是**网关自身的调用方凭据**，与任何上游无关 —— 就像 /admin/settings
// 一样是通用能力。放在核心段（register 在 mountUpstreamRoutes 之前），
// 上游即便声明同名路由也无法覆盖。
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"

	"workbuddy2api/internal/apikey"
)

// apiKeysStore 本包持有的存储（Config.APIKeys 注入，nil = 未启用）。
func (h *Handler) apiKeysStore() *apikey.Store {
	return h.cfg.APIKeys
}

// registerAPIKeys 注册 /admin/apikeys 系列端点（New 里调用）。
//
// # 未启用时**不注册**路由（而不是注册后 501）
//
// 与 Settings/Log 同一条判据：没注入依赖就不该出现会失败的按钮。
// 前端按 manifest 的 admin_routes 渲染操作入口 —— 未注册 = 面板不渲染操作区。
func (h *Handler) registerAPIKeys() {
	h.register("GET /admin/apikeys", h.apiKeysList)
	h.register("POST /admin/apikeys", h.apiKeysCreate)
	h.register("POST /admin/apikeys/toggle", h.apiKeysToggle)
	h.register("POST /admin/apikeys/delete", h.apiKeysDelete)
	h.register("POST /admin/apikeys/reset", h.apiKeysReset)
	h.register("POST /admin/apikeys/update", h.apiKeysUpdate)
	h.register("POST /admin/apikeys/rotate-admin", h.apiKeysRotateAdmin)
}

// apiKeysList GET /admin/apikeys —— 全部 key 的掩码视图 + 管理钥匙掩码。
func (h *Handler) apiKeysList(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []apikey.View{}, "enabled": false})
		return
	}
	adminMask := ""
	if h.cfg.APIKey != "" {
		adminMask = apikey.MaskID(h.cfg.APIKey)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": s.List(), "enabled": true, "admin_key": adminMask})
}

// apiKeysCreate POST /admin/apikeys —— body {"name":"...","limit":0,"rpm":0}。
// 返回完整 key（**仅这一次**返回明文，前端展示后不再可得）。
func (h *Handler) apiKeysCreate(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		Name  string `json:"name"`
		Limit int64  `json:"limit"`
		RPM   int    `json:"rpm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	k, err := s.Create(body.Name, body.Limit, body.RPM)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         k.ID,
		"masked":     apikey.MaskID(k.ID),
		"full":       k.ID,
		"name":       k.Name,
		"enabled":    k.Enabled,
		"limit":      k.Limit,
		"rpm":        k.RPM,
		"created_at": k.CreatedAt,
	})
}

// apiKeysToggle POST /admin/apikeys/toggle —— body {"id":"...","enabled":bool}。
func (h *Handler) apiKeysToggle(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	on, err := s.Toggle(body.ID, body.Enabled)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": apikey.MaskID(body.ID), "enabled": on})
}

// apiKeysDelete POST /admin/apikeys/delete —— body {"id":"..."}。
func (h *Handler) apiKeysDelete(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if err := s.Delete(body.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// apiKeysReset POST /admin/apikeys/reset —— body {"id":"..."}，重置已用额度/失败数。
func (h *Handler) apiKeysReset(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if err := s.Reset(body.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// apiKeysUpdate POST /admin/apikeys/update —— body {"id":"...","limit":0,"rpm":0}。
// 改配额/限速（负值不改，沿用现值语义由 Store 处理）。
func (h *Handler) apiKeysUpdate(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		ID    string `json:"id"`
		Limit int64  `json:"limit"`
		RPM   int    `json:"rpm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if err := s.Update(body.ID, body.Limit, body.RPM); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

// writeStoreError 把 apikey.Store 的哨兵错误映射成 HTTP。
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, apikey.ErrUnknown):
		writeError(w, http.StatusNotFound, "API key 不存在")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// apiKeysRotateAdmin POST /admin/apikeys/rotate-admin —— 轮换管理钥匙。
//
// 生成新 key → 写回 config.json 的 api_key 字段（ConfigPath）→ 通过
// OnAPIKeyRotated 通知装配层更新 handler 内存（立即生效，无需重启）→
// 返回新 key（仅此一次显示完整值）。
func (h *Handler) apiKeysRotateAdmin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.APIKey == "" {
		writeError(w, http.StatusBadRequest, "当前未配置管理钥匙（config 里没有 api_key），无需轮换")
		return
	}
	newKey := apikey.NewAdminKey()
	// 写回 config.json（保留其余字段）。
	if h.cfg.ConfigPath != "" {
		if err := setConfigAPIKey(h.cfg.ConfigPath, newKey); err != nil {
			writeError(w, http.StatusInternalServerError, "写回 config.json 失败: "+err.Error()+"（新 key 未生效）")
			return
		}
	}
	// 内存立即生效（装配层更新 handler 的管理钥匙）。
	if h.cfg.OnAPIKeyRotated != nil {
		h.cfg.OnAPIKeyRotated(newKey)
	}
	log.Printf("admin: 管理钥匙已轮换（写入 %s，立即生效）", h.cfg.ConfigPath)
	writeJSON(w, http.StatusOK, map[string]any{
		"full":   newKey,
		"masked": apikey.MaskID(newKey),
		"path":   h.cfg.ConfigPath,
	})
}

// setConfigAPIKey 读 config.json → 改 api_key 字段 → 原子写回（其余字段原样保留）。
func setConfigAPIKey(path, newKey string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("解析: %w", err)
	}
	cfg["api_key"] = newKey
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("写入临时文件: %w", err)
	}
	return os.Rename(tmp, path)
}
