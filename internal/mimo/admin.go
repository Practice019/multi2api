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
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

const (
	modelsPath   = "/admin/mimo/models"
	syncPath     = "/admin/mimo/sync"
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
		// sync：给桌面侧脚本用的幂等令牌上送口（serviceToken 刷新即同步）。
		{Method: http.MethodPost, Path: syncPath, Handler: p.handleSync, Capability: gateway.CapChat, Title: "桌面令牌同步", Hidden: true},
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
	ctx, cancel := mimoCtx(r, 3*time.Minute)
	defer cancel()
	results := make([]map[string]any, 0, 16)
	idx := 0
	add := func(m map[string]any) { m["index"] = idx; idx++; results = append(results, m) }

	seen := map[string]bool{}
	for _, line := range strings.Split(req.KeysText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 行形态分派：含 serviceToken= 的行按 **route 通道 Cookie 串**导入
		// （桌面端网关探测报告 §6.3 的四件套），否则按 sk/tp key 导入。
		if strings.Contains(line, "serviceToken=") {
			if res := p.importRouteCookie(ctx, line); res != nil {
				add(res)
			}
			continue
		}
		name, key := splitNamed(line)
		if key == "" {
			add(map[string]any{"ok": false, "error": "行里既没有 sk-/tp- key 也没有 serviceToken Cookie: " + truncate(line, 40)})
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

// parseRouteCookie 解析 Cookie 串（serviceToken=…; userId=…; mimopc_slh=…; mimopc_ph=…）
// 成未验活的 route 凭证 —— import 与 sync 共用这一份解析规则（解析逻辑只此一处）。
// 返回 (凭证, 错误文案)；错误为空串表示解析成功。
func parseRouteCookie(line string) (*Auth, string) {
	cookies := map[string]string{}
	for _, kv := range strings.Split(line, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && v != "" {
			cookies[strings.ToLower(k)] = strings.TrimSpace(v)
		}
	}
	st, uid := cookies["servicetoken"], cookies["userid"]
	pass, cuid := cookies["passtoken"], cookies["cuserid"]
	dev := firstNonEmpty(cookies["deviceid"], cookies["d"])
	switch {
	case st == "" && (pass == "" || cuid == ""):
		return nil, "Cookie 里至少要有一套：serviceToken（即时可用）或 passToken+cUserId+userId（SSO 自动换票续命）"
	case st == "" && uid == "":
		// 实测（探测报告 §7）：serviceLogin 缺 userId cookie 会 302 进 SPA 死路，
		// SSO 链要求账号 cookie **全套** —— passToken 套必须连 userId 一起带。
		return nil, "passToken 套还缺 userId（SSO 链要求全套账号 cookie，缺一即死路；Cookie 库/抓包里都有它，不是密钥）"
	}
	nick := ""
	if uid != "" {
		nick = "route:" + uid
	}
	return &Auth{Channel: ChannelRoute, Type: TypeAPI, ServiceToken: st, PassToken: pass,
		CUserID: cuid, UID: uid, Slh: cookies["mimopc_slh"], Ph: cookies["mimopc_ph"],
		DeviceD: dev, Nickname: nick,
		Source: "cookie-import", LoggedInAt: time.Now().UTC().Format(time.RFC3339)}, ""
}

// importRouteCookie 一行 Cookie 进池：解析 → 验活 → 落盘。导错当场报错，
// 比进池后静默 401 友好。
func (p *Provider) importRouteCookie(ctx context.Context, line string) map[string]any {
	a, fail := parseRouteCookie(line)
	if fail != "" {
		return map[string]any{"ok": false, "error": fail}
	}
	st, uid := a.ServiceToken, a.UID
	if cookieSeen["route:"+st] && st != "" {
		return nil // 去重：同一 serviceToken 只导一次（不占回执条）
	}
	cctx, cc := context.WithTimeout(ctx, 40*time.Second)
	defer cc()
	// pass-only 凭证（没有 serviceToken）：先跑一次 SSO 链现换 —— 换票失败
	// 就是 passToken 死了，当场报错（这正是"以后能不能自动续"的第一次考验）。
	if a.ServiceToken == "" {
		if err := p.client.SSOFresh(cctx, a); err != nil {
			return map[string]any{"ok": false, "error": "SSO 换票失败: " + truncate(err.Error(), 160)}
		}
		st, uid = a.ServiceToken, a.UID
		if uid == "" {
			return map[string]any{"ok": false, "error": "SSO 链没带回 userId（异常回执，请联系网关维护者）"}
		}
	}
	// 验活 + 真昵称（失败不拦导入：令牌可能刚从别的机器带来，池子的健康检查
	// 会负责后续；但把探到的昵称/错误带进回执，让用户立刻知道状态）。
	if nick, err := p.client.RouteMe(cctx, a); err != nil {
		return map[string]any{"ok": false, "error": "令牌验活失败（大概率已过期，重新抓包）: " + truncate(err.Error(), 140), "uid": uid}
	} else if nick != "" {
		a.Nickname = nick
	}
	if err := p.saveAuth(a); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	cookieSeen["route:"+st] = true
	return map[string]any{"ok": true, "uid": uid, "nickname": a.Nickname, "channel": "route"}
}

var cookieSeen = map[string]bool{} // 导入会话内去重（进程生命周期，够用）

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
		var err error
		switch a.Channel {
		case ChannelFree:
			err = fmt.Errorf("free 通道无验活端点（探活=bootstrap，由健康检查负责）")
		case ChannelRoute:
			// route 验活 = /api/user/xiaomi/me（302/401 都会折算成错误返回）。
			var nick string
			nick, err = p.client.RouteMe(ctx, a)
			if err == nil && nick != "" {
				res["nickname"] = nick // 顺手刷新真昵称进回执
			}
		default:
			err = p.client.fetchModelsErr(ctx, a)
		}
		if err != nil {
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

// handleSync POST /admin/mimo/sync —— 桌面 serviceToken 幂等上送（同步器专用）。
//
// 与 import 的区别：**按 uid upsert**。桌面端每次启动都会换发新 serviceToken，
// 同步脚本无脑把最新四件套贴过来即可 —— 首次 created、之后 updated，
// 永远只有一份凭证（不会每次同步堆一个重复账号）。
func (p *Provider) handleSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMimoJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	a, fail := parseRouteCookie(req.Cookie)
	if fail != "" {
		writeMimoJSON(w, http.StatusOK, map[string]any{"ok": false, "error": fail})
		return
	}
	ctx, cancel := mimoCtx(r, 60*time.Second)
	defer cancel()
	// 同步器可以只推 passToken 套（桌面 cookie 库那份 30 天的）：先现换再验。
	if a.ServiceToken == "" {
		if err := p.client.SSOFresh(ctx, a); err != nil {
			writeMimoJSON(w, http.StatusOK, map[string]any{"ok": false, "uid": a.UID, "error": "SSO 换票失败: " + truncate(err.Error(), 160)})
			return
		}
	}
	// 验活 + 取昵称。注意**失败也回 200**（带 ok:false）：同步脚本/插件里
	// 非 2xx 常被当传输故障重试刷日志，"令牌这次不新鲜"是业务事实。
	nick, verr := p.client.RouteMe(ctx, a)
	if verr != nil {
		writeMimoJSON(w, http.StatusOK, map[string]any{"ok": false, "uid": a.UID, "error": truncate(verr.Error(), 160)})
		return
	}
	if nick != "" {
		a.Nickname = nick
	}
	action := "created"
	list, _ := p.localAccounts()
	for _, cur := range list {
		if cur != nil && cur.UID == a.UID && cur.Channel == ChannelRoute {
			// 就地更新令牌三件（Slh/Ph/DeviceD 有新值则覆盖），保留创建时间与昵称回落。
			cur.ServiceToken = a.ServiceToken
			for _, pair := range []struct {
				dst *string
				src string
			}{{&cur.Slh, a.Slh}, {&cur.Ph, a.Ph}, {&cur.DeviceD, a.DeviceD},
				{&cur.PassToken, a.PassToken}, {&cur.CUserID, a.CUserID}} {
				if pair.src != "" {
					*pair.dst = pair.src
				}
			}
			cur.ServiceAt = a.ServiceAt
			if cur.Nickname == "" || strings.HasPrefix(cur.Nickname, "route:") {
				cur.Nickname = a.Nickname
			}
			if err := cur.SaveAtomic(); err != nil {
				writeMimoJSON(w, http.StatusOK, map[string]any{"ok": false, "uid": cur.UID, "error": "落盘失败: " + err.Error()})
				return
			}
			action = "updated"
			a = cur
			goto done
		}
	}
	if err := p.saveAuth(a); err != nil {
		writeMimoJSON(w, http.StatusOK, map[string]any{"ok": false, "uid": a.UID, "error": err.Error()})
		return
	}
done:
	writeMimoJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": a.UID, "nickname": a.Nickname, "action": action})
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
