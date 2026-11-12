// preparebody_test.go 请求体改写的断言（★ 真机 502 的回归）。
//
// 这组用例的来源是一次**真机端到端实测**：
//
//	POST /v1/chat/completions（model=phanthywork/deepseek-v4.1-flash-1017）
//	→ HTTP 502 {"error":{"code":"upstream_parse",
//	            "message":"upstream stream contained no valid data events"}}
//
// 根因：anthroconv 对 stream **原样透传**（anthropic.go:130），而客户端发的是
// stream:false（或缺省）⇒ 上游回一次性 JSON ⇒ 出口层按 SSE 解析一个事件都读不到。
// codar 是本仓的标准修法（forceStream + 回归断言），本文件照它的形态写。
//
// ⚠ 用例必须能抓住"把 stream 原样透传回去"这个变异 —— 所以三种入参
// （false / 缺省 / true）都要断言**最终** body 里是 true。
package phanthywork

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestForceStreamForcesStreamTrue ★ 真机踩出来的回归：不强制流式 ⇒ 502 upstream_parse。
//
// 命名与断言形状照 internal/codar/contract_hermetic_test.go:176 的同名用例，
// 便于后来者在两个包之间对照（本仓"同一个坑修两次"的代价已经付过）。
func TestForceStreamForcesStreamTrue(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"显式 false", `{"model":"Phanthy-fast","messages":[{"role":"user","content":"hi"}],"max_tokens":32,"stream":false}`},
		{"缺省（客户端没给）", `{"model":"Phanthy-fast","messages":[{"role":"user","content":"hi"}],"max_tokens":32}`},
		{"已经是 true", `{"model":"Phanthy-fast","messages":[{"role":"user","content":"hi"}],"max_tokens":32,"stream":true}`},
		{"带前缀的 model", `{"model":"phanthywork/Phanthy-fast","messages":[{"role":"user","content":"hi"}],"stream":false}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := PrepareBody([]byte(c.in))
			if err != nil {
				t.Fatalf("PrepareBody 报错: %v", err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("产物不是 JSON: %v (%s)", err, out)
			}
			if got := string(m["stream"]); got != "true" {
				t.Fatalf("最终 body 的 stream = %s，必须是 true —— 否则上游回一次性 JSON、出口层按 SSE 解析 ⇒ 502 upstream_parse（真机实测）；产物: %s", got, out)
			}
			var model string
			_ = json.Unmarshal(m["model"], &model)
			if strings.HasPrefix(model, providerID+"/") {
				t.Errorf("model 带了前缀（坑③ ⇒ 403 model_not_allowed）: %q", model)
			}
			if _, ok := m["max_tokens"]; !ok {
				t.Errorf("产物缺 max_tokens（Anthropic 必填；强制流式不该影响它）: %s", out)
			}
			if _, ok := m["messages"]; !ok {
				t.Errorf("产物缺 messages: %s", out)
			}
		})
	}
}

// TestPrepareBodyKeepsClientMaxTokens 客户端给的 max_tokens 一律不动。
//
// anthroconv 的契约（它自己的测试也钉着）：客户端显式给了就一律不改，只有没给时
// 才填模型上限。这里从出口侧再确认一次 —— 因为"强制流式"与 max_tokens 落在同一个
// map 里，改错一个键的影响容易被另一个掩盖。
func TestPrepareBodyKeepsClientMaxTokens(t *testing.T) {
	out, err := PrepareBody([]byte(`{"model":"Phanthy-fast","messages":[{"role":"user","content":"hi"}],"max_tokens":32}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if got, _ := m["max_tokens"].(float64); got != 32 {
		t.Errorf("max_tokens = %v, want 32（客户端显式给的值必须原样保留）", m["max_tokens"])
	}
}

// TestPrepareBodyIsIdempotent 二次改写不改字节（幂等 ⇒ 不会与签名漂移）。
//
// ⚠ 这条同时是"签名用的字节 == 发出的字节"的**结构保证**：若同一份输入会产出
// 不同字节，那"先签名、后发送"之间就必然存在一个可漂移的窗口。
func TestPrepareBodyIsIdempotent(t *testing.T) {
	in := []byte(`{"model":"phanthywork/Phanthy-fast","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	once, err := PrepareBody(in)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := PrepareBody(once)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Errorf("二次改写改了字节 ⇒ 签名与发送之间会出现窗口；一次=%s 二次=%s", once, twice)
	}
}

// TestPrepareBodyKeepsOtherFieldsByteExact 只改该改的两个键，其余字段不动。
//
// 用 json.RawMessage 的理由就是这个：数字精度与嵌套结构要逐字节保留 ——
// 解成结构体再拼回去会把 0.25 写成 0.250000、把数字精度改掉，而那些改动
// 不会报错，只会让上游看到一份"看起来对、其实不同"的请求。
//
// ⚠ 两处**不是**本函数的改动，别把它们当成"字段被改坏"：
//
//	messages[].content 从字符串变成 Anthropic 的块形
//	   [{"type":"text","text":"hi"}]  ← 这是 anthroconv 的转换职责
//	未知字段（本例的 metadata）被转换层丢掉 ← 同样是 anthroconv 的行为
//
// 所以下面只断言"本函数负责保留"的那些字段 + 转换后的 content 仍在。
func TestPrepareBodyKeepsOtherFieldsByteExact(t *testing.T) {
	in := []byte(`{"model":"Phanthy-fast","messages":[{"role":"user","content":"hi"}],` +
		`"temperature":0.25,"top_p":0.7,"max_tokens":77,"metadata":{"trace":"abc"},"stream":false}`)
	out, err := PrepareBody(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"temperature":0.25`,
		`"top_p":0.7`,
		`"max_tokens":77`,
		// anthroconv 把 content 编成 Anthropic 的块形；这里只要求"内容还在"
		`"text":"hi"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("字段被改坏：期望含 %s；实际: %s", want, out)
		}
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Errorf("没有强制 stream=true: %s", out)
	}
}

// TestSignedBytesAreSentBytes 端到端：发给上游的那份字节就是被签名的那份。
//
// 判据由**假上游**给出 —— 它用收到的原始 body 独立重算 12 段 canonical 再验签
// （见 upstream_test.go 的 verifyInference）。所以这里拿到 200 就等价于
// "签名覆盖的字节 == 发出的字节"这一条在实际链路上成立。
//
// ⚠ 入参刻意用 stream:false —— 那正是真机 502 的形态；强制流式之后，假上游
// 还必须能验过含 stream:true 的那份 body 的签名。
func TestSignedBytesAreSentBytes(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")
	status, body, err := chatOnce(t, p, cred,
		`{"model":"phanthywork/deepseek-v4.1-flash-1017","messages":[{"role":"user","content":"1+1"}],"max_tokens":32,"stream":false}`)
	if err != nil {
		t.Fatalf("Chat 报错: %v", err)
	}
	if status != 200 {
		t.Fatalf("状态码 = %d（签名验不过时假上游回 403）；body=%s", status, body)
	}
	fake.mu.Lock()
	sawStream := fake.lastChatStreamTrue
	fake.mu.Unlock()
	if !sawStream {
		t.Error("假上游收到的 body 里 stream 不是 true（说明强制流式没有落到线上字节里）")
	}
}
