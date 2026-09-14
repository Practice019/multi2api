// desktop_test.go 桌面指纹事件链的形状钉子。
//
// # 为什么这些测试是这次移植最重要的增量
//
// 上游判据是**逐字段**的形状匹配（多一个少一个字段都可能被静默丢弃），
// 而 B 的原仓库对这块**零测试覆盖** —— 17 个动作全靠人工实测撑着。
// 一旦有人"顺手清理"掉一个看似冗余的字段，唯一的表现是
// "任务莫名其妙不亮了"，没有任何报错。
//
// 因此这里逐条钉住：
//
//	① 事件链的**条数与顺序**（上游按序消费）
//	② 每个事件的 eventCode 与关键业务字段
//	③ 指纹注入**不覆盖**业务字段（业务优先）
//	④ 端点的 host / path / UA / body 形态（数组而非对象）
//	⑤ idRegex 的形状判据（服务端 requestId 的正则）
package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// captureDesktop 起一个假上游并捕获请求。
type captureDesktop struct {
	path   string
	host   string
	ua     string
	domain string
	body   string
	ct     string
	hits   int
}

func newDesktopServer(t *testing.T, cap *captureDesktop) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.hits++
		cap.path = r.URL.Path
		cap.host = r.Host
		cap.ua = r.Header.Get("User-Agent")
		cap.domain = r.Header.Get("X-Domain")
		cap.ct = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		cap.body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// decodeEventsAny 解出事件数组（桌面域的事件是 map 数组）。
func decodeEventsAny(t *testing.T, body string) []map[string]any {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("请求体不是事件数组：%v; body=%.200s", err, body)
	}
	return arr
}

// eventCodes 提取事件链的 eventCode 序列。
func eventCodes(events []DesktopEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		s, _ := e["eventCode"].(string)
		out = append(out, s)
	}
	return out
}

// TestDesktopChatSequenceShape 对话链必须是 6 事件且顺序固定。
//
// # 为什么顺序也要钉
//
// 上游按序消费这条链（先 task_created 再 message_send … 最后 request_response）。
// 顺序错了会表现为"事件都收到了但一个都没计分" —— 最难归因的一类。
func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")

	want := []string{
		"agent_task_created",
		"chat_message_send",
		"chat_request_send",
		"chat_message_response",
		"chat_message_status",
		"chat_request_response",
	}
	got := eventCodes(events)
	if len(got) != len(want) {
		t.Fatalf("事件数=%d，期望 %d（%v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个事件=%q，期望 %q", i, got[i], want[i])
		}
	}

	// JOIN 字段：上游用 conversationId/requestId 把这条链关联起来。
	byCode := map[string]DesktopEvent{}
	for _, e := range events {
		byCode[e["eventCode"].(string)] = e
	}
	if v, _ := byCode["agent_task_created"]["conversationId"].(string); v != "conv-1" {
		t.Errorf("agent_task_created.conversationId=%v，期望 conv-1", v)
	}
	if v, _ := byCode["chat_message_response"]["rootRequestId"].(string); v != "req-1" {
		t.Errorf("chat_message_response.rootRequestId=%v，期望 req-1", v)
	}
	// RichMeow_Chat 的判据要求"消息成功回执"—— 这个字段是硬要求。
	if v, _ := byCode["chat_message_response"]["isSuccessful"].(bool); !v {
		t.Error("chat_message_response.isSuccessful 必须为 true（判据要求成功回执）")
	}
	// codebuddy.* 两个点号键是客户端原样上报的形态，不能被"规范化"成下划线。
	if _, ok := byCode["chat_request_send"]["codebuddy.session_id"]; !ok {
		t.Error("chat_request_send 缺 codebuddy.session_id（键名必须逐字保留）")
	}
	if _, ok := byCode["chat_request_send"]["codebuddy.conversation_request_id"]; !ok {
		t.Error("chat_request_send 缺 codebuddy.conversation_request_id")
	}
}

// TestReportDesktopEventWire 出站形态：host / path / UA / 数组 body / 指纹注入。
func TestReportDesktopEventWire(t *testing.T) {
	cap := &captureDesktop{}
	srv := newDesktopServer(t, cap)

	c := New()
	c.ChatBaseCN = srv.URL
	c.BillingBaseCN = "https://billing.invalid"
	c.WebBaseCN = "https://web.invalid"

	a := &auth.Auth{UID: "uid-abc", Nickname: "昵称", AccessToken: "at"}
	events := DesktopChatSequence("c1", "r1", "m1", "fast-model", "fast-model")
	if err := c.ReportDesktopEvent(a, events...); err != nil {
		t.Fatalf("上报失败: %v", err)
	}

	if cap.path != "/v2/report" {
		t.Errorf("path=%q，期望 /v2/report", cap.path)
	}
	if cap.ua != desktopUA {
		t.Errorf("UA=%q，期望桌面端 UA %q（CLI UA 不会点亮桌面任务）", cap.ua, desktopUA)
	}
	if !strings.HasPrefix(cap.domain, "http://127.0.0.1") {
		t.Errorf("X-Domain=%q，应为 chatBase", cap.domain)
	}

	arr := decodeEventsAny(t, cap.body)
	if len(arr) != len(events) {
		t.Fatalf("wire 事件数=%d，期望 %d", len(arr), len(events))
	}
	// 每个事件都必须带完整桌面指纹。
	fp := []string{
		"ideName", "ideType", "ideVersion", "extName", "extVersion",
		"machineId", "sessionId", "os", "arch", "osVersion",
		"cpuCores", "memorySize", "commit", "releaseDate",
		"userId", "userNickname", "timezone", "product",
	}
	for i, ev := range arr {
		for _, k := range fp {
			if _, ok := ev[k]; !ok {
				t.Errorf("第 %d 个事件缺指纹字段 %q —— 缺它上游按非桌面客户端处理（不计分）", i, k)
			}
		}
		if ev["extName"] != "workbuddy-desktop" {
			t.Errorf("第 %d 个事件 extName=%v，期望 workbuddy-desktop", i, ev["extName"])
		}
		if ev["userId"] != "uid-abc" {
			t.Errorf("第 %d 个事件 userId=%v，期望 uid-abc", i, ev["userId"])
		}
	}
}

// TestDesktopFingerprintBusinessFieldsWin 业务字段优先于指纹（覆盖而非被覆盖）。
//
// ReportDesktopEvent 先铺指纹、再铺业务字段 —— 顺序反了会让业务字段
// 被同名的指纹字段冲掉（例如 expert_actual_use 里的 userId 若被指纹覆盖，
// 多账号场景下就会把 A 号的事件记到别人的账上）。
func TestDesktopFingerprintBusinessFieldsWin(t *testing.T) {
	cap := &captureDesktop{}
	srv := newDesktopServer(t, cap)
	c := New()
	c.ChatBaseCN = srv.URL

	a := &auth.Auth{UID: "real-uid", AccessToken: "at"}
	if err := c.ReportDesktopEvent(a, DesktopEvent{"eventCode": "x", "userId": "override-uid"}); err != nil {
		t.Fatal(err)
	}
	ev := decodeEventsAny(t, cap.body)[0]
	if ev["userId"] != "override-uid" {
		t.Errorf("userId=%v，业务字段必须覆盖指纹里的同名键", ev["userId"])
	}
}

// TestDeriveIDDeterministicPerAccount 设备号按 uid 稳定派生（模拟固定设备）。
func TestDeriveIDDeterministicPerAccount(t *testing.T) {
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}

	if deriveID(a1, "machine") != deriveID(a1, "machine") {
		t.Error("同一账号必须每次派生出相同的设备号（否则每次上报都像换了一台机器）")
	}
	if deriveID(a1, "machine") == deriveID(a2, "machine") {
		t.Error("不同账号必须派生不同设备号（否则多个号共用一个设备指纹，是明显的异常特征）")
	}
	if deriveID(a1, "machine") == deriveID(a1, "session") {
		t.Error("不同 salt 必须派生不同值")
	}
	if n := len(deriveID(a1, "machine")); n != 36 {
		t.Errorf("设备号长度=%d，期望 36 位 hex", n)
	}
}

// TestDesktopBuddyAppSequenceShape 五连事件 + 固定 buddyID。
func TestDesktopBuddyAppSequenceShape(t *testing.T) {
	events := DesktopBuddyAppSequence("cb_test_id", "测试应用")
	want := []string{
		"buddyapp_discover_click", "buddyapp_show", "buddyapp_enter_click",
		"buddyapp_auth_confirm_click", "buddyapp_bindaccount_skip_click",
	}
	got := eventCodes(events)
	if len(got) != len(want) {
		t.Fatalf("事件数=%d，期望 5", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个=%q，期望 %q", i, got[i], want[i])
		}
	}
	for i, e := range events {
		if e["buddyId"] != "cb_test_id" || e["buddyName"] != "测试应用" {
			t.Errorf("第 %d 个事件缺 buddyId/buddyName: %v", i, e)
		}
		if e["mode"] != "LOCAL" {
			t.Errorf("第 %d 个事件 mode=%v，期望 LOCAL", i, e["mode"])
		}
	}
}

// TestDesktopTemplateUseSequenceShape chat 链 + 2 个模板事件。
func TestDesktopTemplateUseSequenceShape(t *testing.T) {
	events := DesktopTemplateUseSequence("c", "r", "3", "竞品分析")
	if len(events) != 8 { // 6 + 2
		t.Fatalf("事件数=%d，期望 8（6 条 chat 链 + 2 条模板）", len(events))
	}
	last2 := eventCodes(events)[6:]
	if last2[0] != "agent_task_created_with_template" || last2[1] != "template_used" {
		t.Errorf("尾部两事件=%v", last2)
	}
	if events[6]["id"] != "3" || events[6]["name"] != "竞品分析" {
		t.Errorf("模板事件字段不对: %v", events[6])
	}
	if events[7]["template_id"] != "3" || events[7]["task_mode"] != "working" {
		t.Errorf("template_used 字段不对: %v", events[7])
	}
}

// TestDesktopExpertSequences 召唤链 3 事件 + actual_use 的 mode 变体。
func TestDesktopExpertSequences(t *testing.T) {
	e := MarketExpert{
		ExpertID: "ex_abc", ExpertType: "agent",
		DisplayNameZH: "专家", ProfessionZH: "职称", Version: "1.0.2",
		Categories: []any{"cat-a"},
	}
	summon := DesktopExpertSummonSequence(e)
	if len(summon) != 3 {
		t.Fatalf("召唤链=%d 事件，期望 3", len(summon))
	}
	if sum := eventCodes(summon); sum[0] != "web_element_click" || sum[1] != "expert_summon_click" || sum[2] != "expert_summoned" {
		t.Errorf("召唤链顺序=%v", sum)
	}
	if summon[0]["type"] != "cat-a" {
		t.Errorf("web_element_click.type=%v，期望取 categories[0]", summon[0]["type"])
	}

	craft := DesktopExpertActualUseEvent(e, "c", "0123456789abcdef0123456789abcdef")
	if craft["mode"] != "craft" {
		t.Errorf("ExpertActualUseEvent.mode=%v，期望 craft（expert_5 / team_use_3 口径）", craft["mode"])
	}
	local := DesktopExpertActualUseLocal(e, "c", "0123456789abcdef0123456789abcdef")
	if local["mode"] != "LOCAL" {
		t.Errorf("ExpertActualUseLocal.mode=%v，期望 LOCAL（lighthouse 口径）", local["mode"])
	}
	if local["id"] != "ex_abc" || local["expertType"] != "agent" {
		t.Errorf("actual_use 字段不对: %v", local)
	}
	// messageId 由 requestId 尾部 8 位派生（对齐真实样本）。
	if local["messageId"] != "msg-89abcdef" {
		t.Errorf("messageId=%v，期望 msg-<requestId 尾 8 位>", local["messageId"])
	}
}

// TestMarketExpertListWire 专家市场：POST + 固定路径 + 分页体。
func TestMarketExpertListWire(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"experts":[{"expert_id":"ex_1","expert_type":"agent","display_name_zh":"甲"}]}}`))
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.ChatBaseCN = srv.URL
	list, err := c.MarketExpertList(&auth.Auth{UID: "u", AccessToken: "at"}, "team")
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if gotPath != "/portal/operation-platform/market/expert/list" {
		t.Errorf("path=%q", gotPath)
	}
	if !strings.Contains(gotBody, `"expert_type":"team"`) {
		t.Errorf("body 未带 expert_type: %s", gotBody)
	}
	if len(list) != 1 || list[0].ExpertID != "ex_1" {
		t.Errorf("解析结果=%+v", list)
	}
}

// TestIDRegex 服务端 requestId 的形状判据。
//
// 这条正则决定"我们抓到的是不是真的服务端 id" —— 判错了会拿一个
// 假 id 去上报 expert_actual_use，而**自造 requestId 不计数**（静默失败）。
func TestIDRegex(t *testing.T) {
	yes := []string{
		"0123456789abcdef0123456789abcdef",
		"cmb-0123456789abcdef0123456789abcdef",
	}
	for _, s := range yes {
		if !idRegex.MatchString(s) {
			t.Errorf("应匹配服务端 id 形状：%q", s)
		}
	}
	no := []string{
		"", "wb2api-conv-123", "0123456789abcdef0123456789abcde", // 31 位
		"0123456789abcdef0123456789abcdefg",    // 33 位
		"0123456789ABCDEF0123456789ABCDEF",     // 大写
		"cmb0123456789abcdef0123456789abcdef",  // 缺连字符
		"msg-0123456789abcdef0123456789abcdef", // 别的形状
	}
	for _, s := range no {
		if idRegex.MatchString(s) {
			t.Errorf("不该匹配：%q", s)
		}
	}
}

// TestReportWebEventShape web 域事件：浏览器指纹 + x-client-platform。
//
// 与桌面域的关键差别：**走 webBase（workbuddy.cn）而不是 chatBase**。
// 发错域会被静默丢弃（Library_read 从此不亮，且没有任何错误）。
func TestReportWebEventShape(t *testing.T) {
	var gotPath, gotPlatform, gotUA string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPlatform = r.Header.Get("x-client-platform")
		gotUA = r.Header.Get("User-Agent")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.WebBaseCN = srv.URL
	c.ChatBaseCN = "https://chat.invalid"

	a := &auth.Auth{UID: "u-web", Nickname: "n", EnterpriseID: "e", AccessToken: "at"}
	if err := c.ReportWebEvent(a, "web_element_click", "https://www.workbuddy.cn/space/d/x", "library_doc_intro_click", "资料库"); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	if gotPath != "/v2/report" {
		t.Errorf("path=%q", gotPath)
	}
	if gotPlatform != "web" {
		t.Errorf("x-client-platform=%q，期望 web", gotPlatform)
	}
	if !strings.Contains(gotUA, "Mozilla/5.0") {
		t.Errorf("UA=%q，web 域必须是浏览器 UA", gotUA)
	}
	ev := decodeEventsAny(t, gotBody)[0]
	if ev["os"] != "Win32" || ev["elementId"] != "library_doc_intro_click" {
		t.Errorf("事件字段不对: %v", ev)
	}
	if _, ok := ev["machineId"]; !ok {
		t.Error("web 事件缺 machineId")
	}
}

// TestWebBaseFallback 未注入 WebBaseCN 时回落默认值而不是空串。
//
// 空串会拼出 "/v2/report"（无 host）→ http.NewRequest 报错。
// 测试里只注入 ChatBaseCN/BillingBaseCN 时很容易踩到。
func TestWebBaseFallback(t *testing.T) {
	c := &Client{}
	if got := c.webBase(); got != defaultWebBaseCN {
		t.Errorf("webBase()=%q，期望回落 %q", got, defaultWebBaseCN)
	}
	c2 := &Client{WebBaseCN: "https://custom.example"}
	if got := c2.webBase(); got != "https://custom.example" {
		t.Errorf("webBase()=%q，注入值应优先", got)
	}
}

// TestReportDesktopEventRejectsEmpty 空事件列表直接报错（不发空数组）。
func TestReportDesktopEventRejectsEmpty(t *testing.T) {
	c := New()
	if err := c.ReportDesktopEvent(&auth.Auth{UID: "u"}); err == nil {
		t.Error("空事件列表应返回错误（上游收到空数组是无意义请求）")
	}
}
