// login.go 页内「添加账号」—— 走**手工粘贴**流程（ManualLogin）。
//
// # 为什么本上游没有真正的自动登录
//
// Verdent 的 access token（JWT）只在桌面端主进程内存里：
// Cookies / LocalStorage / vault / auth.json 全查过，都是空的；
// 也没有可脚本化的登录端点（对比 monkeycode 有百智云 OAuth 授权页可跳）。
//
// 所以「添加账号」在本上游的形态是：**弹一个粘贴框，让用户把 token 贴进来**。
// 这不是"偷懒没做" —— 它是这个上游唯一存在的收号路径。
//
// # Start 的 authURL 回空串是刻意的
//
// 前端按"auth_url 有没有值"分两个分支：
//
//	有值   → 打开链接让用户去授权，然后轮询
//	空串   → 不走"打开链接"，直接显示 ManualLogin 的粘贴框
//
// 所以我们**不编**一个假的授权页地址（那会把用户导到一个他什么也做不了的页面）。
// （这条判据是 monkeycode 的 login.go 里写下来的，那一版曾经编过假地址。）
package verdent

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言。
var _ gateway.LoginFlow = (*Provider)(nil)
var _ gateway.ManualLoginExt = (*Provider)(nil)

// adminManualCompletePath 页内「添加账号」的粘贴完成端点（由 ManualLogin 报给前端）。
const adminManualCompletePath = "/admin/verdent/login/complete"

// loginStateTTL 未完成的登录会话保留多久（够用户去 DevTools 复制一次 token）。
const loginStateTTL = 15 * time.Minute

// loginSession 一次未完成的登录。
type loginSession struct{ created time.Time }

// Start 开一次"登录"（本上游 = 开一个粘贴会话）。
//
// ⚠ authURL 回**空串** —— 见文件头。前端据此走粘贴框分支。
func (p *Provider) Start() (state, authURL string, err error) {
	id, err := randomState()
	if err != nil {
		return "", "", err
	}
	p.mu.Lock()
	if p.logins == nil {
		p.logins = map[string]loginSession{}
	}
	now := time.Now()
	// 顺手清理过期会话：不清理的话这张表只增不减（每次点添加账号都加一条）。
	for k, v := range p.logins {
		if now.Sub(v.created) > loginStateTTL {
			delete(p.logins, k)
		}
	}
	p.logins[id] = loginSession{created: now}
	p.mu.Unlock()
	return id, "", nil
}

// Poll 恒回 ErrLoginPending。
//
// # 这不是"没实现"，是**正确语义**
//
// 本上游的登录**不会自动完成** —— 它等的是用户把手上的 token 粘进来。
// 那个动作走 ManualLogin 的完成端点（handleManualComplete），不走轮询。
// 回 ErrLoginPending 让前端继续显示粘贴框，而不是报"登录失败"。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	return gateway.Credential{}, gateway.ErrLoginPending
}

// Configured 恒 true：登录不需要任何部署侧配置（粘贴 token 而已）。
func (p *Provider) Configured() bool { return true }

// ManualLogin 粘贴完成端点与给用户的指引。
//
// 指引里必须写清**去哪拿 token** —— 本上游没有"点一下就好"的路径，
// 用户第一次点「添加账号」时唯一的疑问就是"token 在哪"。
func (p *Provider) ManualLogin() (string, string) {
	return adminManualCompletePath,
		"Verdent 的登录令牌只在桌面端的主进程里，没有可自动跳转的授权页，所以这里手工粘贴。" +
			"取法：打开 Verdent 桌面端（或已登录的网页版）→ F12 打开控制台 → 执行 " +
			"$c.getToken() 复制返回值；也可以用抓包工具从 /llm/stream 请求的 " +
			"Authorization 头里取。把值粘到下面（带 Bearer 前缀、或从别处复制的整行都可以）。" +
			"一次可以粘多条：一行一个。"
}

// handleManualComplete 粘贴完成端点：{state, code} → 解析 → 落盘。
//
// ⚠ 本方法**只落盘**，不碰账号池 —— 池对齐由核心的「重载 auths」负责
// （上游包不得 import internal/pool，这是架构硬约束）。
// 所以返回体里带上 uid 列表，前端据此提示"已入库，点重载生效"。
func (p *Provider) handleManualComplete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}
	if err := jsonDecode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有粘贴任何内容"})
		return
	}
	// 有 state 就消费掉它（它在本次是"一次性票据"）。
	if s := strings.TrimSpace(body.State); s != "" {
		p.mu.Lock()
		delete(p.logins, s)
		p.mu.Unlock()
	}

	creds, err := p.ImportCredentials(body.Code)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	imported := 0
	var uids, notes []string
	for _, c := range creds {
		var a *Auth
		a, err = parseAnyCredential(string(c.Raw))
		if err != nil || a == nil {
			// ImportCredentials 已经把 Raw 序列化成规范形态，重新解析一次
			// 是最省的取回 *Auth 的方式（也顺带验证了"写出去的东西读得回来"）。
			notes = append(notes, shortErr(err, "这一条解析不回来"))
			continue
		}
		a.fillFromJWT()
		if _, serr := p.SaveAuth(a, "页内添加"); serr != nil {
			notes = append(notes, "落盘失败 "+serr.Error())
			continue
		}
		imported++
		uids = append(uids, a.UID())
		log.Printf("verdent: 已添加账号 %s（uid=%s）", a.DisplayName(), a.UID())
	}
	if imported == 0 {
		msg := "没有成功导入任何账号"
		if len(notes) > 0 {
			msg += "：" + strings.Join(notes, "；")
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"imported": imported,
		"uids":     uids,
		"notes":    notes,
	})
}

// randomState 生成随机 state（16 字节 hex）。
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
