// admin.go MiMo 的管理端点（gateway.AdminExt）。
//
// 全部 Hidden（不生成面板入口，由账号池行内动作/导入框/登录弹窗调用）。
// 其中 `POST /admin/mimo/import` 的路径以 `/import` 结尾 —— 前端据此
// **自动渲染批量导入框**（webui.html 的 manifest 驱动规则），这就是
// "新上游前端零改动"的具体形态（报告 §4.2/§0-5）。
package mimo

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

const (
	modelsPath   = "/admin/mimo/models"
	importPath   = "/admin/mimo/import"
	verifyPath   = "/admin/mimo/keys/verify"
	storePath    = "/admin/mimo/clientstore"
	completePath = "/admin/mimo/login/complete"
)

// AdminRoutes 返回本上游的管理端点。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{Method: http.MethodGet, Path: modelsPath, Handler: p.handleModelsPreview, Capability: gateway.CapModels, Title: "模型目录（实时）", Hidden: true},
		{Method: http.MethodPost, Path: importPath, Handler: p.handleImport, Capability: gateway.CapChat, Title: "批量导入 key", Hidden: true},
		{Method: http.MethodPost, Path: verifyPath, Handler: p.handleVerify, Capability: gateway.CapChat, Title: "逐 key 验活", Hidden: true},
		{Method: http.MethodGet, Path: storePath, Handler: p.handleClientStore, Capability: gateway.CapChat, Title: "本机客户端凭证探测", Hidden: true},
		{Method: http.MethodPost, Path: completePath, Handler: p.handleCompleteLogin, Capability: gateway.CapChat, Title: "手动完成登录", Hidden: true},
	}
}

func writeMimoJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"ok":false,"error":"序列化失败"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func mimoCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	ctx := r.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, d)
}

// handleModelsPreview GET /admin/mimo/models —— 用第一个 paid 号实时拉目录。
func (p *Provider) handleModelsPreview(w http.ResponseWriter, r *http.Request) {
	list, _ := p.localAccounts()
	var first *Auth
	for _, a := range list {
		if a != nil && a.Channel == ChannelPaid {
			first = a
			break
		}
	}
	if first == nil {
		writeMimoJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "没有可用的 mimo paid 账号"})
		return
	}
	ctx, cancel := mimoCtx(r, 30*time.Second)
	defer cancel()
	models, err := p.client.FetchModels(ctx, first)
	if err != nil {
		writeMimoJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeMimoJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(models), "models": models})
}

// handleImport POST /admin/mimo/import —— 批量导入（loomy 同族形态）。
//
// 三种入参（可并存，按序处理）：
//
//	{"keys_text":"tp-xxx 主号\n# 注释\nsk-yyy"}   多行 key（可带名字，空格分词）
//	{"auth_json":"<整包官方 auth.json>"}           官方文件内容直贴
//	{"pick_local":true}                            探测本机官方客户端目录
//
// 逐条回执 {index, ok, uid|error}；坏行不拖好行。落盘后立即点「重载 auths」
// （前端导入成功会自动做，loomy 同链）。
func (p *Provider) handleImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KeysText  string `json:"keys_text"`
		AuthJSON  string `json:"auth_json"`
		PickLocal bool   `json:"pick_local"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMimoJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	results := make([]map[string]any, 0, 16)
	idx := 0
	add := func(m map[string]any) { m["index"] = idx; idx++; results = append(results, m) }

	seen := map[string]bool{}
	for _, line := range strings.Split(req.KeysText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, key := splitNamed(line)
		if key == "" {
			add(map[string]any{"ok": false, "error": "行里没有找到 sk-/tp- key: " + truncate(line, 40)})
			continue
		}
		if seen[key] {
			continue // 重复行静默去重（回执里不占条，避免误导"导入了很多"）
		}
		seen[key] = true
		uid := DeriveUID(key)
		a := &Auth{Channel: ChannelPaid, Type: TypeAPI, Key: key, UID: uid,
			Nickname: firstNonEmpty(name, MaskKey(key)), Source: "import",
			LoggedInAt: time.Now().UTC().Format(time.RFC3339)}
		if err := p.saveAuth(a); err != nil {
			add(map[string]any{"ok": false, "error": err.Error()})
			continue
		}
		add(map[string]any{"ok": true, "uid": uid, "nickname": a.Nickname, "key_type": a.KeyType()})
	}

	if strings.TrimSpace(req.AuthJSON) != "" {
		entries, err := ParseClientAuthJSON([]byte(req.AuthJSON))
		if err != nil {
			add(map[string]any{"ok": false, "error": err.Error()})
		}
		for _, a := range entries {
			if seen[a.Key] && a.Key != "" {
				continue
			}
			if err := p.saveAuth(a); err != nil {
				add(map[string]any{"ok": false, "error": err.Error()})
				continue
			}
			add(map[string]any{"ok": true, "uid": a.UID, "nickname": a.Nickname, "key_type": a.KeyType()})
		}
	}

	if req.PickLocal {
		if !p.importClientAuth {
			add(map[string]any{"ok": false, "error": "本机客户端拾取未启用（需 mimo.import_client_auth=true）"})
		} else {
			found := 0
			for _, path := range clientAuthPathCandidates(p.clientAuthDir) {
				raw, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				entries, perr := ParseClientAuthJSON(raw)
				if perr != nil || len(entries) == 0 {
					continue
				}
				for _, a := range entries {
					if seen[a.Key] && a.Key != "" {
						continue
					}
					if err := p.saveAuth(a); err != nil {
						add(map[string]any{"ok": false, "error": err.Error()})
						continue
					}
					add(map[string]any{"ok": true, "uid": a.UID, "nickname": a.Nickname, "from": path})
					found++
				}
				break // 命中一个候选即止（同一台机器多份时取优先级第一个）
			}
			if found == 0 {
				add(map[string]any{"ok": false, "error": "本机未找到可导入的官方客户端凭证（auth.json）"})
			}
		}
	}

	anyOK := false
	for _, m := range results {
		if ok, _ := m["ok"].(bool); ok {
			anyOK = true
		}
	}
	writeMimoJSON(w, http.StatusOK, map[string]any{"ok": anyOK || len(results) == 0, "imported": idx, "results": results})
}

// saveAuth 落盘一份导入凭证（文件名铁律 mimo-<uid>.json）。
func (p *Provider) saveAuth(a *Auth) error {
	if p.authDir == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(p.authDir, 0o755); err != nil {
		return err
	}
	path := strings.TrimSpace(a.FilePath)
	if path == "" {
		path = p.authDir + string(os.PathSeparator) + FileName(a)
		a.FilePath = path
	}
	return a.SaveAtomic()
}

// splitNamed "名字 sk-xxx" / "sk-xxx" → (name, key)。
func splitNamed(line string) (string, string) {
	fields := strings.Fields(line)
	for i, f := range fields {
		if strings.HasPrefix(f, "sk-") || strings.HasPrefix(f, "tp-") {
			return strings.TrimSpace(strings.Join(fields[:i], " ")), f
		}
	}
	return "", ""
}

// handleVerify POST /admin/mimo/keys/verify —— 逐号 GET /models 验活。
// （OmniProxy validator 同法：最便宜、真实带鉴权。）
func (p *Provider) handleVerify(w http.ResponseWriter, r *http.Request) {
	list, _ := p.localAccounts()
	out := make([]map[string]any, 0, len(list))
	ctx, cancel := mimoCtx(r, 3*time.Minute)
	defer cancel()
	for _, a := range list {
		res := map[string]any{"uid": a.UID, "nickname": a.Nickname, "channel": a.Channel}
		if a.Channel == ChannelFree {
			res["ok"] = false
			res["error"] = "free 通道无验活端点（探活=bootstrap，由健康检查负责）"
		} else if err := p.client.fetchModelsErr(ctx, a); err != nil {
			res["ok"] = false
			res["error"] = truncate(err.Error(), 160)
		} else {
			res["ok"] = true
		}
		out = append(out, res)
	}
	writeMimoJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

// handleClientStore GET /admin/mimo/clientstore —— 本机官方目录探测诊断。
// ⚠ 脱敏纪律（loomy 同款）：只回路径存在性/条目数量/掩码，key 本体绝不回。
func (p *Provider) handleClientStore(w http.ResponseWriter, r *http.Request) {
	type cand struct {
		Path    string `json:"path"`
		Exists  bool   `json:"exists"`
		Entries int    `json:"entries,omitempty"`
		Note    string `json:"note,omitempty"`
	}
	out := []cand{}
	for _, path := range clientAuthPathCandidates(p.clientAuthDir) {
		c := cand{Path: path}
		raw, err := os.ReadFile(path)
		if err != nil {
			c.Note = "不存在/不可读"
			out = append(out, c)
			continue
		}
		c.Exists = true
		entries, perr := ParseClientAuthJSON(raw)
		if perr != nil {
			c.Note = "结构不识别"
		} else {
			c.Entries = len(entries)
		}
		out = append(out, c)
	}
	writeMimoJSON(w, http.StatusOK, map[string]any{
		"ok": true, "enabled": p.importClientAuth, "candidates": out,
		"note": "导入需 mimo.import_client_auth=true；本接口永不回传 key 本体",
	})
}

// handleCompleteLogin POST /admin/mimo/login/complete —— 手动粘贴回跳 URL/u。
func (p *Provider) handleCompleteLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
		Code  string `json:"code"` // 整条回跳 URL 或裸 u 值
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMimoJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	f, _ := p.LoginFlow()
	if err := f.(*loginFlow).completeWithU(req.State, req.Code); err != nil {
		writeMimoJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeMimoJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "已就绪，回到控制台等待轮询收号即可"})
}
