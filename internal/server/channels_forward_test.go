// channels_forward_test.go 转发链路的守卫。用一个**本地假上游**（httptest）证明：
//
//	① 请求真的打到了那条渠道的 Base URL
//	② 上游收到的 model 是**上游名**（不是客户端发的公开名）
//	③ 上游收到的 Authorization 是那条渠道的 key
//	④ 其他字段逐字节保留（我们不是协议转换层，不顺手美化）
//	⑤ 渠道停用 / 名字不在能力索引 ⇒ 明确失败，且**不回落**（不是 errNoChannelMode）
//	⑥ 没配渠道存储 ⇒ errNoChannelMode（调用方走既有路径）
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/openaicompat"
)

const fwdKey = "sk-fake-upstream-key"

func fwdStore(t *testing.T, mm map[string]string, status int) *channelstore.Store {
	t.Helper()
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建 store 失败: %v", err)
	}
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-fwd", Name: "假上游渠道", BaseURL: "https://placeholder.invalid/v1",
		Status: status, Priority: 10, Models: []string{"real-x"}, ModelMap: mm,
		Keys: []channelstore.ChannelKey{{Key: fwdKey, Status: channelstore.StatusEnabled}},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	return s
}

func fwdHandler(t *testing.T, s *channelstore.Store) *Handler {
	t.Helper()
	// ⚠ 工厂由组合根注入（核心不认识具体上游包，见 handler.go 的 ChannelProvider 注释）。
	// 测试文件里 import 具体上游是允许的：架构守卫用 `go list -deps`（非 -test）分析，
	// "测试里提到某个上游"不等于"这个包是上游实现"。
	return NewHandler(Config{
		Channels: s,
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			return openaicompat.NewOpenAICompat(id, baseURL)
		},
	})
}

// ① ② ③ ④ 一条请求走完，假上游把看到的都记下来。
func TestForwardViaChannelSendsUpstreamModelAndKey(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	defer srv.Close()

	// 渠道的 BaseURL 指向假上游（替代占位域名）
	s := fwdStore(t, map[string]string{"pub-y": "real-x"}, channelstore.StatusEnabled)
	ch, _ := s.Get("ch-fwd")
	ch.BaseURL = srv.URL + "/v1"
	if err := s.Upsert(ch); err != nil {
		t.Fatalf("改 BaseURL 失败: %v", err)
	}
	h := fwdHandler(t, s)

	body := []byte(`{"model":"pub-y","stream":true,"temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`)
	res, err := h.forwardViaChannel(context.Background(), "pub-y", body)
	if err != nil {
		t.Fatalf("转发应成功: %v", err)
	}
	defer res.Stream.Body.Close()

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("① 应打到渠道的 /v1/chat/completions，实际 %s", gotPath)
	}
	if gotBody["model"] != "real-x" {
		t.Fatalf("② 上游收到的 model 应是上游名 real-x，实际 %v", gotBody["model"])
	}
	if gotAuth != "Bearer "+fwdKey {
		t.Fatalf("③ Authorization 应是渠道的 key，实际 %q", gotAuth)
	}
	// ④ 其余字段原样保留
	if gotBody["stream"] != true || gotBody["temperature"] != 0.3 {
		t.Fatalf("④ 其他字段必须原样保留，实际 %v", gotBody)
	}
	if res.UpstreamModel != "real-x" || res.ChannelName != "假上游渠道" {
		t.Fatalf("返回体应带上游名与渠道名，实际 %+v", res)
	}
	raw, _ := io.ReadAll(res.Stream.Body)
	if string(raw) != "data: {\"ok\":true}\n\n" {
		t.Fatalf("上游流应原样回传，实际 %q", raw)
	}
}

// ⑤ 停用渠道 ⇒ 明确失败（错的是"无候选"），**不是** errNoChannelMode ⇒ 不会偷偷回落。
func TestForwardViaChannelFailsClosedAndDoesNotFallBack(t *testing.T) {
	s := fwdStore(t, map[string]string{"pub-y": "real-x"}, channelstore.StatusEnabled)
	h := fwdHandler(t, s)
	if err := s.SetStatus("ch-fwd", channelstore.StatusManuallyDisabled); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	_, err := h.forwardViaChannel(context.Background(), "pub-y", []byte(`{"model":"pub-y"}`))
	if err == nil {
		t.Fatal("停用渠道后应明确失败")
	}
	if errors.Is(err, errNoChannelMode) {
		t.Fatalf("这**不该**被当成'渠道模式未开启'（那会让调用方悄悄回落既有路径），实际 %v", err)
	}
	// 不在能力索引里的名字同样失败
	h2 := fwdHandler(t, fwdStore(t, nil, channelstore.StatusEnabled))
	if _, err := h2.forwardViaChannel(context.Background(), "не-существует", []byte(`{}`)); err == nil {
		t.Fatal("未声明的模型名应失败")
	}
}

// ⑥ 没配渠道存储 ⇒ errNoChannelMode（这是"模式未开启"，不是错误）。
func TestForwardViaChannelInactiveWithoutStore(t *testing.T) {
	h := NewHandler(Config{})
	_, err := h.forwardViaChannel(context.Background(), "anything", []byte(`{}`))
	if !errors.Is(err, errNoChannelMode) {
		t.Fatalf("未配渠道时应返回 errNoChannelMode，实际 %v", err)
	}
}

// key 全停用 ⇒ 明确报"没有可用 key"，而不是拿空 key 去换上游 401
// （空 Bearer 会把"我们没装上凭证"误报成"上游拒绝这条渠道"）。
func TestForwardViaChannelNoEnabledKey(t *testing.T) {
	s := fwdStore(t, nil, channelstore.StatusEnabled)
	ch, _ := s.Get("ch-fwd")
	ch.Keys = []channelstore.ChannelKey{{Key: fwdKey, Status: channelstore.StatusManuallyDisabled}}
	if err := s.Upsert(ch); err != nil {
		t.Fatalf("改 key 状态失败: %v", err)
	}
	h := fwdHandler(t, s)
	_, err := h.forwardViaChannel(context.Background(), "real-x", []byte(`{"model":"real-x"}`))
	if err == nil {
		t.Fatal("没有可用 key 时应明确失败")
	}
	if errors.Is(err, errNoChannelMode) {
		t.Fatalf("不该被当成模式未开启，实际 %v", err)
	}
}

// withUpstreamModel 只动 model 键；非 JSON / 无 model 字段时不动它（别把上游的真实抱怨改没了）。
func TestWithUpstreamModelShape(t *testing.T) {
	out, err := withUpstreamModel([]byte(`{"model":"a","keep":[1,2],"s":"x"}`), "b")
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("改写结果应是合法 JSON: %v (%s)", err, out)
	}
	if m["model"] != "b" {
		t.Fatalf("model 应改成 b，实际 %v", m["model"])
	}
	if _, ok := m["keep"]; !ok {
		t.Fatalf("其他字段不该丢: %s", out)
	}
	if got, _ := withUpstreamModel([]byte(`not json`), "b"); string(got) != "not json" {
		t.Fatalf("非 JSON 应原样返回，实际 %s", got)
	}
	if got, _ := withUpstreamModel([]byte(`{"no":"model"}`), "b"); string(got) != `{"no":"model"}` {
		t.Fatalf("没有 model 字段应原样返回，实际 %s", got)
	}
}
