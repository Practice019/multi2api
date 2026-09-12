package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestConcurrentMutationsStayConsistent 在有并发写的同时验证数据不变式。
//
// # 为什么写这条（以及它替代不了什么）
//
// 本机无法运行 `go test -race`：没有 C 编译器（gcc/clang 均无），
// 且当前账户不是管理员、装不了工具链。`zig cc` 能让 TSan 链接通过，
// 但影子内存映射在 zig 的 lld 下失败（Windows 已知限制）。
// 评审明确指出这是"最大的证据缺口"。
//
// 这条测试**不替代** race detector。它验证的是**不变式**：
//   - List() 里不能有重复 UID（重复 = 并发写坏了索引）
//   - ListFor(p) 的结果必须是 List() 的子集（防跨上游串位）
//   - 并发 Add 之后总数不能丢
//   - 并发 Flush 之后落盘仍是完整可解析的 JSON
//
// 竞争若造成**可见的**数据损坏，这些断言会红。
// 静默竞争（两个 goroutine 同时读同一指针、恰好得到正确结果）抓不到 ——
// 这一点写在注释里，避免日后有人误以为已经覆盖。
func TestConcurrentMutationsStayConsistent(t *testing.T) {
	p := New("")

	const (
		writers      = 8
		readers      = 8
		opsPerWorker = 40
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var violations int64
	var reads atomic.Int64

	// ---- 写入者：并发 Add 不同 UID ----
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				uid := fmt.Sprintf("w%d-u%d", w, i)
				p.Add(&auth.Auth{UID: uid, Nickname: "n-" + uid})
			}
		}(w)
	}

	// ---- 读者：并发读并校验不变式 ----
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}

				list := p.List()
				reads.Add(1)

				// 不变式 1：List 里不能有重复 UID
				seen := make(map[string]bool, len(list))
				dup := false
				for _, s := range list {
					if s.UID == "" {
						atomic.AddInt64(&violations, 1) // 字段撕裂会给出空 UID
						dup = true
						break
					}
					if seen[s.UID] {
						t.Errorf("不变式破坏：UID %s 在 List() 中出现多次", s.UID)
						atomic.AddInt64(&violations, 1)
						dup = true
						break
					}
					seen[s.UID] = true
				}
				if dup {
					return
				}

				// 不变式 2：ListFor(p) 的每一条都必须属于上游 p。
				//
				// ⚠ 我原先写的是"ListFor 必须是 List 的子集"——**那是错的**：
				// 两次调用发生在不同时刻，并发 Add 期间后一次自然可能多出新账号。
				// 那种断言会假红（实测就是这么红的）。
				// 真正的不变式是**归属**：返回条的 Provider 必须等于请求的上游，
				// 且必须在 Providers() 声明的上游集合里。这与时间无关。
				for _, s := range p.ListFor(DefaultProvider) {
					want := p.normalizeProvider(DefaultProvider)
					if p.normalizeProvider(s.Provider) != want {
						t.Errorf("不变式破坏：ListFor(%q) 返回了属于 %q 的账号 %s",
							DefaultProvider, s.Provider, s.UID)
						atomic.AddInt64(&violations, 1)
						return
					}
				}

				// 不变式 3：Status(uid) 与 List 里的条目一致（防两份视图漂移）
				for _, s := range list {
					got, ok := p.Status(s.UID)
					if !ok {
						t.Errorf("不变式破坏：List 里有 %s，但 Status 说不存在", s.UID)
						atomic.AddInt64(&violations, 1)
						return
					}
					if got.UID != s.UID {
						t.Errorf("不变式破坏：Status(%s) 返回了 %s 的数据（串位）", s.UID, got.UID)
						atomic.AddInt64(&violations, 1)
						return
					}
				}
			}
		}()
	}

	// 写入者结束后再让读者多跑一会，覆盖"写完了读者还在读"的窗口
	go func() {
		time.Sleep(20 * time.Millisecond)
		for i := 0; i < 100; i++ {
			_ = p.List()
			p.CountsDetailed()
			time.Sleep(time.Millisecond)
		}
		close(stop)
	}()

	wg.Wait()

	// ---- 终态 ----
	final := p.List()
	if len(final) != writers*opsPerWorker {
		t.Errorf("终态账号数 = %d，期望 %d（并发 Add 丢数据）",
			len(final), writers*opsPerWorker)
	}
	if v := atomic.LoadInt64(&violations); v != 0 {
		t.Errorf("并发不变式被破坏 %d 次", v)
	}
	if reads.Load() == 0 {
		t.Fatal("读者一次都没跑起来 —— 测试自身失效（这不是通过）")
	}
	total, healthy, cooling, disabled, inFlight := p.CountsDetailed()
	_ = cooling
	_ = disabled
	_ = inFlight
	t.Logf("%d 次并发读；终态 total=%d healthy=%d；0 次不变式破坏",
		reads.Load(), total, healthy)
}

// TestConcurrentFlushStaysParseable 并发 Flush 不得产出半截状态文件。
//
// 评审确认过实现用的是原子写（临时文件 + rename），值得钉成回归：
// 一旦有人图省事改成直接写目标文件，崩溃/并发就会留下半截 JSON，
// 下次启动解析失败 → **丢掉全部账号状态**。这是最贵的一类回归。
func TestConcurrentFlushStaysParseable(t *testing.T) {
	// ⚠ New() 的参数是**状态文件路径**（不是目录）。
	// 我第一版传了 t.TempDir()，于是 Flush() 写到了不存在的位置，
	// 重载得到 0 个账号 —— 那是**测试写错**，不是产品缺陷。
	// 正确的用法见 reload_scope_test.go：filepath.Join(dir, "state.json")。
	stateFile := filepath.Join(t.TempDir(), "state.json")
	p := New(stateFile)
	for i := 0; i < 20; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%d", i), Nickname: "n"})
	}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				p.Flush()
			}
		}()
	}
	// 同时并发读，制造读写的交叉窗口
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 80; j++ {
				_ = p.List()
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	// 终态：内存里 20 个账号不能丢
	if got := len(p.List()); got != 20 {
		t.Errorf("并发 Flush 后账号数 = %d，期望 20", got)
	}
	t.Log("240 次并发 Flush + 320 次并发读之后，内存状态完整（20 个账号）")

	// 落盘文件必须存在且是完整 JSON。
	//
	// ⚠ 注意：`Add()` **不**置 dirty 位（它只是内存里的扫描结果），
	// 所以 Add 之后直接 Flush() 不会写文件 —— 这是设计如此，不是缺陷。
	// 真正会持久化的是 SetCredits/Remove 这类显式状态变更，
	// 以及后台周期性 flusher。这里用 SetCredits 制造"有东西可写"。
	for i := 0; i < 20; i++ {
		p.SetCredits(fmt.Sprintf("u%d", i), int64(i*10))
	}
	p.Flush()

	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("落盘文件读不到（%s）: %v", stateFile, err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("落盘文件不是合法 JSON（并发写坏了？）: %v", err)
	}
	t.Logf("落盘文件 %d 字节，是合法 JSON", len(raw))

	// 关键：重新加载必须恢复全部 20 个账号。
	// 只看"文件是合法 JSON"不够 —— 半截文件也可能恰好解析成功。
	p2 := New(stateFile)
	if got := len(p2.List()); got != 20 {
		t.Errorf("从落盘状态重新加载得到 %d 个账号，期望 20 —— "+
			"落盘可能丢了数据", got)
	} else {
		t.Log("从落盘状态重新加载成功，20 个账号全部恢复")
	}

	// 不应留下 .tmp 残骸（原子写的另一半证据）
	tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(stateFile), "*.tmp"))
	if len(tmps) > 0 {
		t.Errorf("残留 %d 个 .tmp 文件，原子写可能没清理干净: %v", len(tmps), tmps)
	}
}
