package codearts

// manuallogin_test.go —— T-231 的守卫。
//
// # 本文件要钉住的风险
//
//  1. **扩展点必须挂在 `*Provider` 上**：核心走 `ExtOf[ManualLoginExt](p)` 类型断言，
//     访问器形态（cline 那种）在生产**静默失效** —— 不报错、只是界面上没有输入框。
//     所以这里不仅测返回值，更用 `gateway.ExtOf` 走**生产那条路**（见 test 1）。
//  2. **端点必须真挂上**：`ManualLogin()` 报的路径若不在 `AdminRoutes()` 里，
//     前端 POST 过去就是 404。断言"报的路径 ∈ 已注册路由"。
//  3. **投递必须复用本地回调那条路**：粘贴的 code 要能从 `Session.Wait()` 里出来
//     （而不是另起一套换证）。test 3 用**真 `*Session`** 证明这一点。
//  4. **粘贴内容解析不许 panic**：用户会粘各种东西（整条 URL / query / 裸 code / 空 / 垃圾）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// ---------------------------------------------------------------------------
// 1. 自报 + 生产路径（ExtOf）必须认出来 + 端点必须真挂上
// ---------------------------------------------------------------------------

func TestManualLoginDeclaredOnProvider(t *testing.T) {
	p := &Provider{}

	// ★ 生产路径：核心就是这么问的（admin/schedule.go 的 fillManualLogin）。
	ext, ok := gateway.ExtOf[gateway.ManualLoginExt](p)
	if !ok {
		t.Fatal("ExtOf[ManualLoginExt] 认不出 *Provider —— manifest 会下发 login.manual=null，" +
			"界面上就没有「添加账号」按钮（这正是 cline 的访问器形态踩的坑）")
	}
	path, instructions := ext.ManualLogin()
	if !strings.HasPrefix(path, "/") {
		t.Errorf("端点路径必须以 / 开头，得到 %q", path)
	}
	if strings.TrimSpace(instructions) == "" {
		t.Error("指引文案为空 —— 前端会渲染一个没有说明的输入框")
	}
	if !strings.Contains(instructions, "code") {
		t.Errorf("指引里应告诉用户粘什么（含 code 的回跳地址），实际 %q", instructions)
	}

	// ★ 端点必须真的注册在 AdminRoutes 里，否则前端 POST 过去是 404。
	var found bool
	for _, r := range p.AdminRoutes() {
		if r.Path == path {
			found = true
			if r.Method != http.MethodPost {
				t.Errorf("完成端点应为 POST，实际 %s", r.Method)
			}
			if r.Handler == nil {
				t.Error("完成端点缺 Handler —— 挂上去会是 404")
			}
		}
	}
	if !found {
		t.Errorf("ManualLogin 报的路径 %q 不在 AdminRoutes() 里 —— 粘贴后必然 404", path)
	}

	// 补一条**容易搞混的对比**（本仓文档里专门写过，我自己第一版就写反了）：
	//
	//	ExtOf[LoginFlow] 只回答"方法集在不在"（编译期形状）⇒ 恒 true；
	//	Configured()     才回答"这份部署真能用吗"（运行期）⇒ 未配 login 时为 false。
	//
	// 少了后者，界面会给出一个"点了必然失败"的按钮。
	if _, ok := gateway.ExtOf[gateway.LoginFlow](p); !ok {
		t.Error("ExtOf[LoginFlow] 应认出 *Provider（*Provider 挂了 Start/Poll/Configured ⇒ 形状成立）")
	}
	if p.Configured() {
		t.Error("未配 login 时 Configured() 必须为 false —— 否则界面会给一个点了必然失败的按钮")
	}
}

// ---------------------------------------------------------------------------
// 2. 粘贴内容解析：四种形态 + 错误路径（不许 panic）
// ---------------------------------------------------------------------------

func TestExtractPastedForms(t *testing.T) {
	cases := []struct {
		name      string
		pasted    string
		hint      string
		wantState string
		wantCode  string
		wantErr   bool
	}{
		{"完整回跳地址", "http://127.0.0.1:8080/oauth/callback?code=CODE-A&state=ST-1", "", "ST-1", "CODE-A", false},
		{"裸 query", "code=CODE-B&state=ST-2", "", "ST-2", "CODE-B", false},
		{"query 带前导问号", "?code=CODE-C&state=ST-3", "", "ST-3", "CODE-C", false},
		{"参数在 fragment", "http://127.0.0.1:8080/oauth/callback#code=CODE-D&state=ST-4", "", "ST-4", "CODE-D", false},
		{"URL 编码的 code", "http://127.0.0.1/cb?code=a%2Fb%2Bc&state=ST-5", "", "ST-5", "a/b+c", false},
		{"只粘 code（state 用界面给的）", "CODE-E", "ST-6", "ST-6", "CODE-E", false},
		{"只粘 code 但没 state", "CODE-F", "", "", "CODE-F", false}, // state 由调用方报错，见下一条断言
		{"空", "   ", "ST-7", "", "", true},
		{"有地址但没 code", "http://127.0.0.1:8080/oauth/callback?state=ST-8", "", "", "", true},
		{"有 query 但没 code", "state=ST-8&x=1", "", "", "", true},
		// ★ 垃圾输入不再是"当成裸 code 投出去"，而是**可读报错**：
		// 第一版把 `////` 当 code 收下了 —— 那正是"看起来成功、实际必失败"的形态。
		{"垃圾输入", "////", "", "", "", true},
		{"含等号的裸串（不是 code 的形状）", "abc=def", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, code, err := extractPasted(c.pasted, c.hint)
			if c.wantErr {
				if err == nil {
					t.Fatalf("应报错，实际 state=%q code=%q", state, code)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if state != c.wantState || code != c.wantCode {
				t.Errorf("(state, code) = (%q, %q)，期望 (%q, %q)", state, code, c.wantState, c.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. ★ 投递复用：粘贴的 code 必须能从真 Session.Wait() 里出来
// ---------------------------------------------------------------------------

func TestCompleteManualDeliversThroughSameSession(t *testing.T) {
	// 用**真** *Session（不是 fakeSession）—— 这条测的就是"同一条投递路径"。
	sess := &Session{State: "ST-1", resultCh: make(chan callbackResult, 1)}
	lf := newTestFlow(&fakeManager{}, sess, "ST-1")

	if err := lf.completeManual("ST-1", "CODE-PASTED"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	// 投递发生在 Wait 之前也必须成立（resultCh 缓冲 1）。
	code, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait 报错: %v", err)
	}
	if code != "CODE-PASTED" {
		t.Errorf("Wait 拿到 %q，期望 CODE-PASTED —— 说明粘贴的 code 没走本地回调那条投递路径", code)
	}

	// 重复粘贴：与"回调只该来一次"同语义 —— 第二个被忽略，不 panic、不覆盖。
	if err := lf.completeManual("ST-1", "CODE-SECOND"); err != nil {
		t.Fatalf("第二次投递不应报错（只是被 once 忽略）: %v", err)
	}
}

func TestCompleteManualRejectsUnknownAndEmpty(t *testing.T) {
	sess := &Session{State: "ST-1", resultCh: make(chan callbackResult, 1)}
	lf := newTestFlow(&fakeManager{}, sess, "ST-1")

	if err := lf.completeManual("ST-NOPE", "CODE"); err == nil {
		t.Error("未知 state 必须报错（复用 errStateGone 语义），而不是静默成功")
	} else if err != errStateGone {
		t.Errorf("未知 state 应返回 errStateGone，实际 %v", err)
	}
	if err := lf.completeManual("ST-1", "   "); err == nil {
		t.Error("空 code 必须报错")
	}
	if err := lf.completeManual("  ", "CODE"); err == nil {
		t.Error("空 state 必须报错")
	}
	if err := lf.completeManual("ST-1", ""); err == nil {
		t.Error("空 code 必须报错")
	}
}

// ---------------------------------------------------------------------------
// 4. 端点行为：合法粘贴 → 200 且投递成功；坏输入 → 400 且不 panic
// ---------------------------------------------------------------------------

func TestHandleManualComplete(t *testing.T) {
	newP := func(t *testing.T) (*Provider, *Session) {
		t.Helper()
		// ⚠ `Manager` 是具体类型 ⇒ 用零值即可：本路径只用到它撑起 loginFlow，
		// 不调它的任何方法（换证由核心的 Poll 触发，本测试不涉及）。
		p := &Provider{login: &Manager{}}
		f, ok := p.LoginFlow()
		if !ok {
			t.Fatal("LoginFlow() 应为 true（p.login 非 nil）")
		}
		lf, ok := f.(*loginFlow)
		if !ok {
			t.Fatal("LoginFlow() 应返回 *loginFlow")
		}
		sess := &Session{State: "ST-9", resultCh: make(chan callbackResult, 1)}
		lf.mu.Lock()
		lf.sessions["ST-9"] = &loginSession{oauth: sess, done: make(chan loginResult, 1)}
		lf.mu.Unlock()
		return p, sess
	}
	post := func(p *Provider, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, manualCompletePath, strings.NewReader(body))
		rec := httptest.NewRecorder()
		p.handleManualComplete(rec, req)
		return rec
	}

	t.Run("合法：整条回跳地址 → 200 且 Wait 拿得到", func(t *testing.T) {
		p, sess := newP(t)
		rec := post(p, `{"code":"http://127.0.0.1:8080/oauth/callback?code=CODE-H&state=ST-9"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("期望 200，实际 %d（%s）", rec.Code, rec.Body.String())
		}
		code, err := sess.Wait()
		if err != nil || code != "CODE-H" {
			t.Errorf("Wait = (%q, %v)，期望 (CODE-H, nil)", code, err)
		}
	})

	t.Run("坏 JSON → 400", func(t *testing.T) {
		p, _ := newP(t)
		if rec := post(p, `{oops`); rec.Code != http.StatusBadRequest {
			t.Errorf("期望 400，实际 %d", rec.Code)
		}
	})

	t.Run("空粘贴 → 400", func(t *testing.T) {
		p, _ := newP(t)
		rec := post(p, `{"state":"ST-9","code":"  "}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("期望 400，实际 %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "没有粘贴") {
			t.Errorf("错误文案应说清「没粘东西」，实际 %s", rec.Body.String())
		}
	})

	t.Run("只粘 code 且没 state → 400 并提示粘整条地址", func(t *testing.T) {
		p, _ := newP(t)
		rec := post(p, `{"code":"CODE-I"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("期望 400，实际 %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "整个回跳地址") {
			t.Errorf("应提示用户粘整条地址，实际 %s", rec.Body.String())
		}
	})

	t.Run("state 不存在 → 400（不是 500，也不是 panic）", func(t *testing.T) {
		p, _ := newP(t)
		rec := post(p, `{"state":"ST-GONE","code":"CODE-J"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("期望 400，实际 %d", rec.Code)
		}
	})

	t.Run("未配置登录流程 → 400 且不 panic", func(t *testing.T) {
		p := &Provider{} // login == nil
		rec := post(p, `{"state":"ST-9","code":"CODE-K"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("期望 400，实际 %d", rec.Code)
		}
	})
}
