// report.go 对话活跃上报：POST {billingBase}/v2/report（借鉴 workbuddy2api-panel）。
//
// # 它解决什么问题（这是本文件必须存在的唯一理由）
//
// 上游的成长任务与连登天数**不由网关的聊天流量点亮**，而是靠客户端上报的
// 行为事件。网关此前完全不发这个事件，于是：
//
//	first_buddy（领取第一只 Buddy）的门槛永远不满足
//	  → 领养恒失败于 HTTP 400 "first_buddy task not completed yet"
//	  → 而 first_buddy 是其余 17 个任务的**前置**
//
// 改造前本仓库把那个 400 当作"上游的合理门槛"写在注释里
// （见 workbuddy/travel.go 的 travelAdopt），实际是**我们自己少调了一步**。
//
// B 分支已在真实多账号环境验证：补上这条上报后领养 +300 到账（3/3 账号）。
//
// # 为什么一条上报能同时解决两件事
//
// `chat_request_send` 事件同时被两个机制消费：
//
//	连登（streak）       —— 每日活跃
//	first_buddy 的解锁   —— 领养前置
//
// 所以它是"一次上报、两处收益"，不需要为每个目的各发一条。
//
// # 风控口径：每号每天 1 次
//
// 日活跃奖励按天去重，多次上报没有额外收益，只会给上游留下异常高频的画像。
// 因此本函数由**每天一次**的排程调用（见 workbuddy 的活动上报任务），
// 不在请求路径上被触发。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// reportPath 活跃上报通道（billing 域，与签到/余额同源）。
const reportPath = "/v2/report"

// billingJSON 发 billing 域（billingBase，codebuddy.cn）请求并解信封。
//
// # 移植说明（来自 workbuddy2api-panel/internal/upstream/report.go）
//
// body 为 nil 时不带请求体。与 growthJSON 对称：
//
//	growth 域（copilot.tencent.com）→ growthJSON
//	billing 域（codebuddy.cn）      → billingJSON
//
// 请求头统一走 BillingHeaders，信封与错误语义同 doJSON。
//
// ⚠ 路径是**调用方传入**的完整路径，本函数不加任何前缀 ——
// B 的用法里既有 "/v2/report" 也有 "/billing/meter/claim-gift"（后者**不带 /v2**）。
// 1:1 移植时刻意保留这个差异，而不是"顺手统一加 /v2"：
// 那会改变实际请求路径，而正确性只能靠上游验证，不能靠推断。
func (c *Client) billingJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.billingBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	return c.doJSON(req)
}

// chatRequestEvent 客户端 `chat_request_send` 事件的完整形状。
//
// # 为什么把所有字段都填上，而不是只发最小三字段
//
// 上游对事件体做**形状校验**：缺字段的事件实测会被服务端收下但**静默丢弃**
// （返回 200、没有错误、不计分）。B 的记录："事件必须带 userId，
// 缺失则服务端 200 但静默丢弃"。
//
// 既然"200 但丢弃"是这个端点的主要失败模式，唯一稳妥的做法就是
// 照抄客户端真实事件的完整形状 —— 少一个字段就可能白跑，
// 而白跑**没有任何可观测信号**（这正是它危险的地方）。
//
// ⚠ 字段名必须与上游逐字一致（camelCase）。有测试钉住关键字段存在
// （见 report_test.go 的 TestChatRequestEventShape）。
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`

	// UserID 是**唯一必须非空**的字段。
	//
	// 实测：缺它时服务端返回 200 但静默丢弃 —— 表现为
	// "上报成功、连登天数不涨、first_buddy 依旧不解锁"，
	// 而日志上什么都看不出来。
	UserID string `json:"userId"`
}

// defaultReportModelID / Name 上报里声明的模型。
//
// 用 flash 档：它是所有账号都有的基础模型，不会因为"这个号没有该模型"
// 而被上游当作无效事件丢弃。
const (
	defaultReportModelID   = "deepseek-v4-flash"
	defaultReportModelName = "DeepSeek V4 Flash"
)

// ReportChatActivity 发一条对话活跃上报（默认模型）。
//
// conversationID 由调用方生成（如 `wb2api-<ms>`）—— 服务端不校验它是否
// 对应真实会话，只要求形状完整。requestID 为本轮独立标识；空时回落 conversationID。
//
// 错误语义与其它 billing 调用一致：HTTP 非 2xx / 业务 code != 0 → *Error。
// ⚠ 注意"返回 nil"**不代表计分成功** —— 上游可能 200 静默丢弃。
// 需要确认时用工作区的 `streak 自检`（回读连登天数）。
func (c *Client) ReportChatActivity(a *auth.Auth, conversationID, requestID string) error {
	return c.ReportChatActivityModel(a, conversationID, requestID,
		defaultReportModelID, defaultReportModelName)
}

// ReportChatActivityModel 同上，但可指定上报携带的模型。
//
// 用途：某些任务要求"体验某个特定模型"（例如 Model_chat_GLM5.2 需要
// requestModelId 与该模型一致且 requestID 独立），此时上报必须带上真实模型名，
// 否则事件虽然形状完整却不对应任务判据。
func (c *Client) ReportChatActivityModel(a *auth.Auth, conversationID, requestID, modelID, modelName string) error {
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = defaultReportModelID
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()

	ev := chatRequestEvent{
		EventCode:            "chat_request_send",
		Timestamp:            now,
		ReportDelay:          0,
		Mode:                 "craft",
		ConversationID:       conversationID,
		RequestID:            requestID,
		InputLength:          12,
		RequestModelID:       modelID,
		RequestModelName:     modelName,
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		PresentAt:            now,
		RootRequestID:        requestID,
		ParentConversationID: conversationID,
		AgentName:            "default",
		AgentType:            "conversation",
		UserID:               a.UID,
	}

	// 上游收的是**数组**（一次可报多个事件）。发对象会被拒。
	raw, err := json.Marshal([]chatRequestEvent{ev})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+reportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	c.BillingHeaders(req, a)
	_, err = c.doJSON(req)
	return err
}

// NewReportConversationID 生成一个上报用的会话 id。
//
// 形如 `wb2api-<毫秒时间戳>`：带前缀便于在日志/抓包里认出这是网关自造的，
// 时间戳保证同一账号多次上报各自不同（便于区分"今天报过没有"）。
func NewReportConversationID() string {
	return "wb2api-" + itoa64(time.Now().UnixMilli())
}

// itoa64 极简 int64 → 十进制字符串（避免为一个 id 引入 strconv）。
func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	p := len(b)
	for v > 0 {
		p--
		b[p] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
