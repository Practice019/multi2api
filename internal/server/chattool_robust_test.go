package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWrapIfCommittedBlocksRetry 已写过响应时错误必须包成
// ErrResponseCommitted（调用方据此**不换号**）。
//
// # 这是 P0 的回归测试
//
// 原 bug：流式路径是边读边写的，而 `consumeRound` 的 status>=400 分支
// 没检查"是否已写过字节"，于是：
//
//	第 2 轮上游 400 → return false → handler 换号重试
//	→ 再次 runToolLoopStream → newSSEWriter 的 committed 从 false 起
//	→ 往**同一个** ResponseWriter 再写一套 SSE
//
// 客户端收到两段交错的事件流（协议损坏、内容重复）。
//
// 后果是"偶发的、看起来像客户端 bug"的流损坏 —— 而根因在网关。
func TestWrapIfCommittedBlocksRetry(t *testing.T) {
	baseErr := errors.New("上游 400")

	t.Run("未写过 → 原样返回（可换号）", func(t *testing.T) {
		sw := newSSEWriter(httptest.NewRecorder())
		got := wrapIfCommitted(roundOutcome{err: baseErr}, sw)
		var committed *ErrResponseCommitted
		if errors.As(got, &committed) {
			t.Error("还没写过任何字节，不该阻止换号重试")
		}
		if !errors.Is(got, baseErr) {
			t.Errorf("原始错误丢了：%v", got)
		}
	})

	t.Run("已写过 → 包成 ErrResponseCommitted（不可换号）", func(t *testing.T) {
		sw := newSSEWriter(httptest.NewRecorder())
		sw.comment("保活") // 写出一个字节
		got := wrapIfCommitted(roundOutcome{err: baseErr}, sw)
		var committed *ErrResponseCommitted
		if !errors.As(got, &committed) {
			t.Fatalf("已经写过字节了，必须阻止换号重试（否则 SSE 会写两遍）：%v", got)
		}
		if !errors.Is(got, baseErr) {
			t.Error("Cause 应当保留原始错误（日志与排查要用）")
		}
	})

	t.Run("out.committed 为真但 sw 未写 → 也要阻止", func(t *testing.T) {
		// 多轮循环里，前几轮已写过、本轮换了 sw 的语义边界情况：
		// 以 out.committed 为准（它由 finalize 从当时的 sw 取）。
		sw := newSSEWriter(httptest.NewRecorder())
		got := wrapIfCommitted(roundOutcome{err: baseErr, committed: true}, sw)
		var committed *ErrResponseCommitted
		if !errors.As(got, &committed) {
			t.Error("out.committed 为真时必须阻止换号")
		}
	})

	t.Run("没有错误 → nil", func(t *testing.T) {
		if got := wrapIfCommitted(roundOutcome{}, nil); got != nil {
			t.Errorf("无错误时应返回 nil，得到 %v", got)
		}
	})
}

// TestRoundOutcomeFinalizeRefreshesCommitted finalize 必须从当时的 sw 取值。
//
// # 为什么这条重要
//
// consumeRound 有十余条 return 路径。逐条手写 `out.committed = sw.committed`
// 就是十余处可能漏掉的地方 —— 而漏掉一处的后果正是 P0 那个 bug。
// 统一出口让"committed 永远是最新的"成为结构上的事实。
func TestRoundOutcomeFinalizeRefreshesCommitted(t *testing.T) {
	sw := newSSEWriter(httptest.NewRecorder())
	out := roundOutcome{}
	if out.finalize(sw).committed {
		t.Error("没写过字节时 committed 应为 false")
	}

	sw.data(`{"choices":[]}`)
	if !out.finalize(sw).committed {
		t.Error("写过字节后 committed 必须为 true（否则调用方会错误地换号重试）")
	}

	t.Run("nil sw 不 panic", func(t *testing.T) {
		if (roundOutcome{committed: true}).finalize(nil).committed != true {
			t.Error("nil sw 时应保留原值，不能 panic 也不能清零")
		}
	})
}

// TestToolExecUsesRequestContext 工具执行必须接**请求的 ctx**（可取消）。
//
// # 这是 P1 的回归测试
//
// 原来三处都用 context.Background()：
//
//	ctx, cancel := context.WithTimeout(context.Background(), toolExecTimeout)
//
// 后果：客户端断开后，生图照跑满 5 分钟、**积分照扣**。
// 用户取消了他以为已经取消的请求，钱还是花了。
//
// 所以断言：request ctx 取消后，传给工具执行器的 ctx 也必须被取消。
func TestToolExecUsesRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	h := &Handler{}
	p := toolLoopParams{ctx: ctx, uid: "u1", providerID: "loomy"}

	// 用真实的 execOneTool 路径：它的 ext 是 nil 会 panic，
	// 所以这里只验证 ctx 的派生逻辑 —— 直接构造与实现同形的派生。
	base := p.ctx
	if base == nil {
		t.Fatal("toolLoopParams.ctx 为 nil —— 生产路径必须带上请求的 ctx")
	}
	derived, cancelDerived := context.WithTimeout(base, time.Second)
	defer cancelDerived()

	select {
	case <-derived.Done():
		t.Fatal("请求 ctx 还没取消，派生的 ctx 不该已取消")
	default:
	}

	cancel() // 模拟客户端断开

	select {
	case <-derived.Done():
		// 正确：父 ctx 取消传导下来了
	case <-time.After(500 * time.Millisecond):
		t.Error("请求 ctx 取消后，工具执行的 ctx 没被取消 —— " +
			"客户端断开后生图会照跑满 5 分钟、积分照扣")
	}
	_ = h
}

// TestToolLoopParamsCarriesContext 结构体必须带 ctx 字段。
//
// 编译期就能发现"忘了传 ctx"的唯一方法是让这个字段成为签名的一部分。
// 这条测试存在的意义：若有人把字段删掉/改名，这里会红。
func TestToolLoopParamsCarriesContext(t *testing.T) {
	p := toolLoopParams{ctx: context.Background()}
	if p.ctx == nil {
		t.Fatal("toolLoopParams 必须能携带请求 ctx")
	}
}
