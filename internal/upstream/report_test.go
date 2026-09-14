// report_test.go 对话活跃上报（/v2/report）的形状判据。
//
// # 为什么形状判据在这里格外重要
//
// 这个端点的失败模式是**200 但静默丢弃** —— 没有错误码、没有响应体差异、
// 日志上什么都看不出来。唯一的防线就是"发出去的请求体形状正确"，
// 因此每条关键字段都要有断言。
//
// 尤其是 userId：缺它时上游收下但不计分，而调用方拿到的是 nil error
// （看起来完全成功）。这一条是本文件存在的首要理由。
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

// reportCapture 捕获一次上报请求。
type reportCapture struct {
	path string
	body string
	hits int
}

func newReportServer(t *testing.T, cap *reportCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.hits++
		raw, _ := io.ReadAll(r.Body)
		cap.body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// decodeEvents 解出事件数组（上游收数组，不是对象）。
func decodeEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	if err := json.Unmarshal([]byte(body), &events); err != nil {
		t.Fatalf("上报体不是数组（上游收数组）：%v; body=%s", err, body)
	}
	return events
}

// TestReportChatActivityPathAndShape 路径、方法、事件形状。
func TestReportChatActivityPathAndShape(t *testing.T) {
	cap := &reportCapture{}
	srv := newReportServer(t, cap)
	c := New()
	c.BillingBaseCN = srv.URL
	// 上报走 billing 域，ChatBaseCN 刻意指向一个不存在的地址：
	// 若实现走错域，请求会失败而不是静默通过。
	c.ChatBaseCN = "https://wrong.invalid"

	a := &auth.Auth{UID: "uid-123", AccessToken: "at"}
	if err := c.ReportChatActivity(a, "wb2api-1", ""); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	if cap.hits != 1 {
		t.Fatalf("上游收到 %d 次请求，期望 1", cap.hits)
	}
	if cap.path != "/v2/report" {
		t.Errorf("path=%q，期望 /v2/report", cap.path)
	}
	events := decodeEvents(t, cap.body)
	if len(events) != 1 {
		t.Fatalf("事件数=%d，期望 1", len(events))
	}
	if got := events[0]["eventCode"]; got != "chat_request_send" {
		t.Errorf("eventCode=%v，期望 chat_request_send", got)
	}
	if got := events[0]["userId"]; got != "uid-123" {
		t.Errorf("userId=%v，期望 uid-123 —— "+
			"缺它上游返回 200 但静默丢弃（调用方拿到 nil error，看起来完全成功）", got)
	}
	if got := events[0]["conversationId"]; got != "wb2api-1" {
		t.Errorf("conversationId=%v", got)
	}
}

// TestReportChatActivityRequiredFieldsPresent 所有会被形状校验的字段都必须存在。
//
// # 为什么列出这么多
//
// 上游对事件体做形状校验，缺字段会被静默丢弃。既然"200 但丢弃"是主要失败模式，
// 唯一稳妥的做法就是照抄客户端真实事件的完整形状 —— 所以这里逐个钉住，
// 让"删掉一个字段"这种顺手改动立刻红。
func TestReportChatActivityRequiredFieldsPresent(t *testing.T) {
	cap := &reportCapture{}
	srv := newReportServer(t, cap)
	c := New()
	c.BillingBaseCN = srv.URL
	if err := c.ReportChatActivity(&auth.Auth{UID: "u"}, "conv", "req"); err != nil {
		t.Fatal(err)
	}
	ev := decodeEvents(t, cap.body)[0]

	// 必须存在的字段（值可以是零值，但**键必须在**）。
	required := []string{
		"eventCode", "timestamp", "reportDelay", "mode",
		"conversationId", "requestId", "inputLength",
		"requestModelId", "requestModelName",
		"isPlan", "isAutoExecuteTerminal", "isAutoModify", "codebaseEnable",
		"maxToken", "maxSteps", "temperature", "maxRetries",
		"mentionContexts", "knowledgeId", "knowledgeName",
		"codebaseId", "mentionContextCount", "command",
		"expertId", "recommendId", "skillId", "skillCount", "totalCount",
		"fileUri", "presentAt", "traceId", "rootRequestId", "parentConversationId",
		"agentName", "agentType", "userId",
	}
	for _, k := range required {
		if _, ok := ev[k]; !ok {
			t.Errorf("事件缺字段 %q —— 上游做形状校验，缺字段会被**静默丢弃**"+
				"（200 但不计分，调用方看不到任何异常）", k)
		}
	}
}

// TestReportChatActivityEmptyRequestIDFallsBackToConversation 空 requestID 回落会话 id。
func TestReportChatActivityEmptyRequestIDFallsBackToConversation(t *testing.T) {
	cap := &reportCapture{}
	srv := newReportServer(t, cap)
	c := New()
	c.BillingBaseCN = srv.URL
	if err := c.ReportChatActivity(&auth.Auth{UID: "u"}, "conv-9", ""); err != nil {
		t.Fatal(err)
	}
	ev := decodeEvents(t, cap.body)[0]
	if got := ev["requestId"]; got != "conv-9" {
		t.Errorf("requestId=%v，期望回落成 conversationId", got)
	}
}

// TestReportChatActivityModelOverride 可指定上报携带的模型。
//
// 某些任务要求"体验特定模型"，此时上报必须带真实模型名（且 requestId 独立），
// 否则事件形状完整却不对应任务判据。
func TestReportChatActivityModelOverride(t *testing.T) {
	cap := &reportCapture{}
	srv := newReportServer(t, cap)
	c := New()
	c.BillingBaseCN = srv.URL
	if err := c.ReportChatActivityModel(&auth.Auth{UID: "u"}, "conv", "req", "glm-5.2", "GLM 5.2"); err != nil {
		t.Fatal(err)
	}
	ev := decodeEvents(t, cap.body)[0]
	if got := ev["requestModelId"]; got != "glm-5.2" {
		t.Errorf("requestModelId=%v，期望 glm-5.2", got)
	}
	if got := ev["requestModelName"]; got != "GLM 5.2" {
		t.Errorf("requestModelName=%v", got)
	}
}

// TestReportChatActivityDefaultModelIsCommon 默认模型必须是所有账号都有的档。
//
// 用一个冷门模型会让上报因"该号没有这个模型"被丢弃 —— 而丢弃仍然是静默的。
func TestReportChatActivityDefaultModelIsCommon(t *testing.T) {
	cap := &reportCapture{}
	srv := newReportServer(t, cap)
	c := New()
	c.BillingBaseCN = srv.URL
	if err := c.ReportChatActivity(&auth.Auth{UID: "u"}, "conv", ""); err != nil {
		t.Fatal(err)
	}
	if got := decodeEvents(t, cap.body)[0]["requestModelId"]; got != defaultReportModelID {
		t.Errorf("默认模型=%v，期望 %v", got, defaultReportModelID)
	}
}

// TestReportChatActivitySendsDeviceTokenAndServiceHeaders 上报走 billing 头族。
//
// 上报与签到/余额同属 billing 域，因此共享 BillingHeaders：
// 带 Authorization，并且（配了的话）带设备风控头。
//
// ⚠ 与 chat 请求的关键差别：billing 域**不带** X-No-* 约定头
// （那些是 chat 域的缺失标记）。
func TestReportChatActivitySendsDeviceTokenAndServiceHeaders(t *testing.T) {
	var gotAuth, gotDevice string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDevice = r.Header.Get("X-Device-Token")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BillingBaseCN = srv.URL
	c.DeviceToken = "dev-1"
	if err := c.ReportChatActivity(&auth.Auth{UID: "u", AccessToken: "at-1"}, "conv", ""); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer at-1" {
		t.Errorf("Authorization=%q", gotAuth)
	}
	if gotDevice != "dev-1" {
		t.Errorf("X-Device-Token=%q，期望 dev-1（billing 域同样注入设备风控头）", gotDevice)
	}
}

// TestNewReportConversationID 生成的会话 id 带前缀且互不相同。
func TestNewReportConversationID(t *testing.T) {
	a := NewReportConversationID()
	if !strings.HasPrefix(a, "wb2api-") {
		t.Errorf("会话 id %q 应以 wb2api- 开头（便于在日志/抓包里认出是网关自造的）", a)
	}
	// 同一毫秒内可能相同，但至少要能生成（不 panic）。
	_ = NewReportConversationID()
}
