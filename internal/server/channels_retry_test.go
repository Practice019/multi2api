// channels_retry_test.go P4.5：同档内失败换渠道。
//
// # 要修的缺陷
//
// 改造前 forwardViaChannelAttemptAs 只挑**一条**候选就发：随机挑到渠道 A，
// A 挂了（502/429/401）就立刻把错误丢给客户端 —— 同一档里明明还有渠道 B，
// 却完全没有机会。而账号池路径一直有轮转，于是"渠道模式反而更脆"。
//
// # 四条要钉住的
//
//	① 同档两条，第一条失败 ⇒ 自动试第二条，请求**成功**
//	② 全部同档都失败 ⇒ 才报错
//	③ 只在同档内换：低优先级档**不会**被用来兜底（那是降级，语义不同）
//	④ 不重复挑同一条（试过的渠道不再被选中）
package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/openaicompat"
)

// retryUpstream 一个可按渠道配置成败的假上游集合。
type retryUpstream struct {
	mu    sync.Mutex
	hits  map[string]int  // 渠道 ID → 命中次数
	fails map[string]bool // 渠道 ID → 是否总是失败
	srvs  map[string]*httptest.Server
}

func newRetryUpstream(t *testing.T) *retryUpstream {
	t.Helper()
	return &retryUpstream{hits: map[string]int{}, fails: map[string]bool{}, srvs: map[string]*httptest.Server{}}
}

// add 造一条假上游并把它的 BaseURL 写进 store 的对应渠道。
func (r *retryUpstream) add(t *testing.T, s *channelstore.Store, id string, fail bool, status int) {
	t.Helper()
	r.mu.Lock()
	r.fails[id] = fail
	r.mu.Unlock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits[id]++
		r.mu.Unlock()
		if fail {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"ok":true}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	r.srvs[id] = srv

	ch, ok := s.Get(id)
	if !ok {
		t.Fatalf("渠道 %s 不存在", id)
	}
	ch.BaseURL = srv.URL + "/v1"
	if err := s.Upsert(ch); err != nil {
		t.Fatalf("改 BaseURL 失败: %v", err)
	}
}

func (r *retryUpstream) hitCount(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[id]
}

// retryStore 造一个含若干条同优先级渠道的 store。
//
// 同档 = 同一 priority。这是 P4.5 的核心场景：同一服务水平下有多条渠道。
func retryStore(t *testing.T, specs []spec) *channelstore.Store {
	t.Helper()
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range specs {
		if err := s.Upsert(&channelstore.Channel{
			ID: sp.id, Name: sp.id, BaseURL: "https://placeholder.invalid/v1",
			Status: channelstore.StatusEnabled, Priority: sp.prio, Weight: sp.weight,
			Models: []string{"m"},
			Keys:   []channelstore.ChannelKey{{Key: "sk-" + sp.id, Status: channelstore.StatusEnabled}},
		}); err != nil {
			t.Fatalf("建渠道 %s 失败: %v", sp.id, err)
		}
	}
	return s
}

func retryHandler(s *channelstore.Store) *Handler {
	return NewHandler(Config{
		Channels: s,
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			return openaicompat.NewOpenAICompat(id, baseURL)
		},
	})
}

type spec = struct {
	id     string
	prio   int
	weight int
}

// ★ ① 同档两条，一条坏一条好 ⇒ 请求成功，且两条都被打过（换人了）。
//
// # 为什么用**权重**钉住首发顺序，而不是靠随机跑很多轮
//
// 同档内是加权随机的（生产行为必须保持随机，不能为了测试改成确定性）。
// 我第一版写了 "跑 8 轮，总有一轮先挑中 bad" —— 那是**错的**：那是概率断言，
// 4/8 轮先挑中 good 就直接成功，bad 一次都没被打，测试红了但代码是对的。
//
// 正确做法是把"首次一定挑 bad"变成**确定的事实**：bad 权重 1000000、good 权重 1。
// 加权随机下 bad 必然首选（概率 1-1e-6）。而 bad 失败后，「已试过」集合里
// 已经有 bad ⇒ 同档只剩 good ⇒ **必须**选中 good。
//
// 于是这条测试同时钉住两件事：
//
//	· 失败会换人（否则 bad 失败后直接报错）
//	· 换人时排除已试过的（若实现没排除，权重会让它再次挑中 bad，
//	  表现为 bad 被打多次、good 一次都没被打 ⇒ 红）
func TestSameTierFailoverSucceeds(t *testing.T) {
	s := retryStore(t, []spec{
		{id: "ch-bad", prio: 10, weight: 1000000}, // 首次必然挑中它
		{id: "ch-good", prio: 10, weight: 1},
	})
	up := newRetryUpstream(t)
	up.add(t, s, "ch-bad", true, http.StatusBadGateway)
	up.add(t, s, "ch-good", false, 0)
	h := retryHandler(s)

	fw, err := h.forwardViaChannel(context.Background(), "m", []byte(`{"model":"m"}`))
	// ① 最终成功
	if err != nil {
		t.Fatalf("★ 同档还有健康渠道时必须重试成功，实际失败: %v", err)
	}
	if fw.Stream.Body != nil {
		fw.Stream.Body.Close()
	}
	// ② 由 good 服务
	if fw.ChannelID != "ch-good" {
		t.Errorf("最终应由 ch-good 服务，实际 %s", fw.ChannelID)
	}
	// ③ bad 真被调用过（不是跳过它直接挑 good）
	if n := up.hitCount("ch-bad"); n != 1 {
		t.Errorf("★ ch-bad 应恰好被打 1 次（首发必中它；打 0 次说明没真试过就跳过了，"+
			"打多次说明换人时没排除已试过的），实际 %d", n)
	}
	// ④ good 也被调用过
	if n := up.hitCount("ch-good"); n != 1 {
		t.Errorf("ch-good 应恰好被打 1 次，实际 %d", n)
	}
}

// ★ ② 全部同档都失败 ⇒ 才报错，且错误来自最后一条（用户该看到的原因）。
func TestSameTierAllFailReportsError(t *testing.T) {
	s := retryStore(t, []spec{{id: "ch-a", prio: 10, weight: 1}, {id: "ch-b", prio: 10, weight: 1}})
	up := newRetryUpstream(t)
	up.add(t, s, "ch-a", true, http.StatusBadGateway)
	up.add(t, s, "ch-b", true, http.StatusBadGateway)
	h := retryHandler(s)

	_, err := h.forwardViaChannel(context.Background(), "m", []byte(`{"model":"m"}`))
	if err == nil {
		t.Fatal("★ 全部失败时必须报错")
	}
	// 两条都试过了
	if up.hitCount("ch-a") == 0 || up.hitCount("ch-b") == 0 {
		t.Errorf("两条同档渠道都该被尝试，实际 ch-a=%d ch-b=%d", up.hitCount("ch-a"), up.hitCount("ch-b"))
	}
	// 错误应当是上游 HTTP 错误（供 writeChannelError 透传状态码）
	if _, ok := gateway.AsUpstreamHTTPError(err); !ok {
		t.Logf("（提示）最终错误不是上游 HTTP 错误：%v", err)
	}
}

// ★ ③ 只在同档内换：低优先级档**不会**被用来兜底。
//
// 这是与"降级"的分界线。若实现靠递增 attempt 换人，这条会红 ——
// 而红的表现正是"一条渠道偶发 502 就把全部流量降到低优先级档"。
func TestFailoverDoesNotCrossTiers(t *testing.T) {
	s := retryStore(t, []spec{{id: "ch-high", prio: 100, weight: 1}, {id: "ch-low", prio: 1, weight: 1}})
	up := newRetryUpstream(t)
	up.add(t, s, "ch-high", true, http.StatusBadGateway)
	up.add(t, s, "ch-low", false, 0)
	h := retryHandler(s)

	_, err := h.forwardViaChannel(context.Background(), "m", []byte(`{"model":"m"}`))
	if err == nil {
		t.Fatal("★ 高档唯一渠道失败时应当报错 —— 跨档兜底是「降级」，语义不同，" +
			"不能顺手在同档重试里做掉")
	}
	if up.hitCount("ch-low") != 0 {
		t.Errorf("★ 低优先级档不该被打（那是静默降级），实际命中 %d 次", up.hitCount("ch-low"))
	}
}

// ★ ④ 不重复挑同一条：同档三条，坏1 → 坏2 → 好，各打一次。
//
// 用权重把顺序**钉死**（good 权重最小）：若实现没记住已试渠道，随机会让
// 「good → 报错」或「坏1 被打两次」这两种形态出现，而它们都是 bug。
// 钉住顺序之后，正确实现必然走 坏1 → 坏2 → good 这条唯一路径，
// 于是"每条恰好 1 次"就是一个**确定**的断言。
func TestFailoverDoesNotRetrySameChannel(t *testing.T) {
	s := retryStore(t, []spec{
		{id: "ch-bad1", prio: 10, weight: 1000000},
		{id: "ch-bad2", prio: 10, weight: 1000},
		{id: "ch-good", prio: 10, weight: 1},
	})
	up := newRetryUpstream(t)
	up.add(t, s, "ch-bad1", true, http.StatusBadGateway)
	up.add(t, s, "ch-bad2", true, http.StatusBadGateway)
	up.add(t, s, "ch-good", false, 0)
	h := retryHandler(s)

	fw, err := h.forwardViaChannel(context.Background(), "m", []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("应当换到健康渠道，实际 %v", err)
	}
	if fw.Stream.Body != nil {
		fw.Stream.Body.Close()
	}
	if fw.ChannelID != "ch-good" {
		t.Errorf("最终应由 ch-good 服务，实际 %s", fw.ChannelID)
	}
	for _, id := range []string{"ch-bad1", "ch-bad2", "ch-good"} {
		if n := up.hitCount(id); n != 1 {
			t.Errorf("★ %s 应恰好被打 1 次（试过就不再挑），实际 %d", id, n)
		}
	}
}

// 单条渠道失败 ⇒ 行为与改造前一致（仍是明确失败，不多打）。
//
// 这条守住"没有同档候选时不要做无谓的动作"。
func TestSingleChannelFailureUnchanged(t *testing.T) {
	s := retryStore(t, []spec{{id: "ch-only", prio: 10, weight: 1}})
	up := newRetryUpstream(t)
	up.add(t, s, "ch-only", true, http.StatusBadGateway)
	h := retryHandler(s)

	_, err := h.forwardViaChannel(context.Background(), "m", []byte(`{"model":"m"}`))
	if err == nil {
		t.Fatal("唯一渠道失败应报错")
	}
	if n := up.hitCount("ch-only"); n != 1 {
		t.Errorf("只有一条候选时只该打 1 次，实际 %d", n)
	}
	var ue *gateway.UpstreamHTTPError
	if !errors.As(err, &ue) || ue.Status != http.StatusBadGateway {
		t.Errorf("错误应保留上游状态码供 writeChannelError 透传，实际 %v", err)
	}
}

// 没配渠道存储 ⇒ errNoChannelMode（既有行为不变）。
func TestRetryPathNoStoreStillModeDisabled(t *testing.T) {
	h := NewHandler(Config{})
	_, err := h.forwardViaChannel(context.Background(), "m", []byte(`{}`))
	if !errors.Is(err, errNoChannelMode) {
		t.Fatalf("没配渠道存储应报 errNoChannelMode，实际 %v", err)
	}
}
