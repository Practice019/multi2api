// apikeys.go —— /admin/apikeys 管理端点（对标 new-api 令牌管理，单机简化版）。
//
// # 为什么端点长在核心 admin 而不是上游
//
// API key 是**网关自身的调用方凭据**，与任何上游无关 —— 就像 /admin/settings
// 一样是通用能力。放在核心段（register 在 mountUpstreamRoutes 之前），
// 上游即便声明同名路由也无法覆盖。
package admin

import (
	"bytes"
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

// apiKeysCreate POST /admin/apikeys —— body {"name":"...","limit":0,"rpm":0,"models":["..."]}。
// 返回完整 key（**仅这一次**返回明文，前端展示后不再可得）。
//
// models 省略 / null / [] 都等于**不限制模型**（见 apikey.Key.Models 的三态说明）。
func (h *Handler) apiKeysCreate(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Limit  int64    `json:"limit"`
		RPM    int      `json:"rpm"`
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	k, err := s.Create(body.Name, body.Limit, body.RPM, body.Models)
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
		"models":     k.Models,
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

// apiKeysUpdate POST /admin/apikeys/update —— body {"id":"...","limit":0,"rpm":0,"models":[...]}。
// 改配额/限速/模型白名单。
//
// # 每个字段都是三态（**字段缺失 = 不改这一项**）
//
//	limit / rpm 用**指针**：缺失 / null → 不改；给 0 → 设为不限；给 N → 设为 N
//	models 用切片：缺失 / null → 不改；给 `[]` → 清空（恢复不限制）；给非空 → 设为白名单
//
// ⚠ 前端有「改配额」与「改模型」两个独立动作，各自只带自己那几个字段。
// 把"缺失"当成"设为 0 / 清空"会让**用户没动过的字段被悄悄改掉** ——
// 改一次模型把额度清零，或改一次配额把模型限制解开。两者界面上都看不出来。
func (h *Handler) apiKeysUpdate(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		ID     string   `json:"id"`
		Limit  *int64   `json:"limit"`
		RPM    *int     `json:"rpm"`
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if err := s.Update(body.ID, body.Limit, body.RPM, body.Models); err != nil {
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

// setConfigAPIKey 读 config.json → 只改 api_key 的值 → 原子写回。
//
// # ⚠ 改造前它把整个 config 重新序列化，这是用户那份配置"乱"的根源
//
// 旧实现：
//
//	var cfg map[string]any
//	json.Unmarshal(raw, &cfg)
//	cfg["api_key"] = newKey
//	out, _ := json.MarshalIndent(cfg, "", "  ")   // ← 整个文件重写
//
// 后果三条（用户实测看到的"太乱了"）：
//
//	① **注释全丢** —— 用户手写的说明一个都不剩
//	② **顺序全变** —— map 序列化按 key 排序，且嵌套层级与手写习惯无关，
//	   于是"轮换一次钥匙，配置文件的段落顺序就变了"
//	③ **空行/缩进风格全变** —— 包括那个夹在段中间的空行
//
// 而轮换管理钥匙**只需要改一个字符串**。没有任何理由重写整个文件。
//
// # 现在的做法：字节级替换那一处的值
//
// 定位 `"api_key"` 这个键、找到它后面的字符串字面量、只替换那一段。
// 其余**逐字节原样保留** —— 注释、顺序、空行、缩进全部不动。
//
// 找不到该键时（比如配置里根本没写 api_key）才回落到"解析 + 重写"，
// 因为那种情况下没有可替换的原文 —— 而这种回落是**有日志的**，
// 用户能知道自己的注释这次被规整了。
func setConfigAPIKey(path, newKey string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取: %w", err)
	}

	if out, ok := replaceJSONStringValue(raw, "api_key", newKey); ok {
		return writeFileAtomic(path, out)
	}

	// 回落：文件里没有 api_key 键（或形态异常到定位不了）。
	log.Printf("admin: config.json 里没有可替换的 api_key（首次写入）—— "+
		"本次会重新序列化该文件，注释与原有顺序不再保留（写入 %s）", path)
	var cfg map[string]any
	if err := json.Unmarshal(stripCommentsForRewrite(raw), &cfg); err != nil {
		return fmt.Errorf("解析: %w", err)
	}
	cfg["api_key"] = newKey
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化: %w", err)
	}
	return writeFileAtomic(path, out)
}

// replaceJSONStringValue 把 `"key": "..."` 里的值替换成 newValue，
// 其余字节**原样保留**。成功返回 (新内容, true)。
//
// # 为什么手写扫描而不是用正则
//
// 要跳过的情形太多，正则写不对：字符串内部的 `"api_key"`（例如某个说明
// 字段刚好含这几个字）、转义引号、注释里的同名键。这些的失败形态都是
// **改错地方** —— 而改错一个配置文件的后果比不轮换钥匙重得多。
//
// 扫描规则（与 jsonc.go 的字符串状态机同源）：
//
//	跳过字符串、行注释、块注释
//	只在**顶层**找 `"key"`（深度 1 的键）—— 嵌套对象里的同名键不动
//	键后面必须是可选的空白 + 冒号 + 可选的空白 + 双引号字符串
func replaceJSONStringValue(raw []byte, key, newValue string) ([]byte, bool) {
	keyLit := []byte(`"` + key + `"`)
	depth := 0
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			// 字符串：跳到结束（含转义处理）。
			j := i + 1
			for j < len(raw) {
				if raw[j] == '\\' {
					j += 2
					continue
				}
				if raw[j] == '"' {
					break
				}
				j++
			}
			if j >= len(raw) {
				return nil, false // 未闭合
			}
			// 是顶层键名？且紧接着是冒号？
			if depth == 1 && bytes.Equal(raw[i:j+1], keyLit) {
				k := j + 1
				for k < len(raw) && (raw[k] == ' ' || raw[k] == '\t' || raw[k] == '\n' || raw[k] == '\r') {
					k++
				}
				if k < len(raw) && raw[k] == ':' {
					k++
					for k < len(raw) && (raw[k] == ' ' || raw[k] == '\t' || raw[k] == '\n' || raw[k] == '\r') {
						k++
					}
					if k < len(raw) && raw[k] == '"' {
						// 找到值的起止。
						v := k + 1
						for v < len(raw) {
							if raw[v] == '\\' {
								v += 2
								continue
							}
							if raw[v] == '"' {
								break
							}
							v++
						}
						if v >= len(raw) {
							return nil, false
						}
						enc, err := json.Marshal(newValue)
						if err != nil {
							return nil, false
						}
						out := make([]byte, 0, len(raw)-(v-k-1)+len(enc))
						out = append(out, raw[:k]...)
						out = append(out, enc...)
						out = append(out, raw[v+1:]...)
						return out, true
					}
				}
			}
			i = j
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '/':
			// 注释：跳过（保持与 jsonc.go 一致 —— 注释里的 "api_key" 不是键）。
			if i+1 < len(raw) && raw[i+1] == '/' {
				for i < len(raw) && raw[i] != '\n' {
					i++
				}
			} else if i+1 < len(raw) && raw[i+1] == '*' {
				i += 2
				for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
					i++
				}
				i++
			}
		}
	}
	return nil, false
}

// stripCommentsForRewrite 只给"没有 api_key 可替换、必须整体重写"的回落路径用。
//
// 与 jsonc.go 的 stripJSONComments 是**同一件事**，但那个在 main 包、
// 这个在 admin 包（admin 不能 import cmd/server —— 方向反了）。
//
// ⚠ 两处实现同一套状态机是**重复**，这里如实记下取舍：抽公共包要新建
// 一个只放 40 行状态机的包，而它的唯一调用方就是这两处；相比之下
// "两处各有一份"的风险是它们会分叉。分叉的症状是"配置文件写法在
// 启动解析和轮换写回两条路径上被理解得不一样"—— 概率低但难查。
// 若将来出现第三处调用，就应该抽出去（那时收益才盖过成本）。
func stripCommentsForRewrite(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	inStr, inLine, inBlock := false, false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case inLine:
			if c == '\n' || c == '\r' {
				inLine = false
				continue
			}
			out[i] = ' '
		case inBlock:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				inBlock = false
				continue
			}
			if c != '\n' && c != '\r' {
				out[i] = ' '
			}
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		default:
			switch {
			case c == '"':
				inStr = true
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				out[i], out[i+1] = ' ', ' '
				i++
				inLine = true
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				out[i], out[i+1] = ' ', ' '
				i++
				inBlock = true
			}
		}
	}
	return out
}

// writeFileAtomic 临时文件 + rename，避免写到一半被中断留下半个配置。
//
// 权限保持 0600（配置里有管理钥匙）—— 与旧实现一致。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换: %w", err)
	}
	return nil
}
