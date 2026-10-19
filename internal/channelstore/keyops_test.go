// keyops_test.go 多 key 运维语义的守卫。
//
// # 守的是什么
//
// 用户的要求原话：「一个渠道多 apikey、多 apikey 轮询使用、apikey 不能用了自动禁用、
// 有一个 apikey 管理可以手动禁用启用」。这四条都在这里钉住 —— 尤其**轮询必须真的轮**：
// 改造前转发层自己写了个"取第一个启用的"，界面上写着轮询、实际永远打同一把 key。
package channelstore

import (
	"path/filepath"
	"strings"
	"testing"
)

func newKeyTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建存储失败: %v", err)
	}
	s.rand = newRand(1) // 固定种子：随机分支可复现
	return s
}

// upsertKeyChannel 建一条带 keys 的渠道并取回**深拷贝**（模拟选路时的输入）。
func upsertKeyChannel(t *testing.T, s *Store, keys ...ChannelKey) *Channel {
	t.Helper()
	if err := s.Upsert(&Channel{
		ID: "c1", Name: "多key渠道", BaseURL: "https://a.example.com/v1",
		Status: StatusEnabled, Models: []string{"m1"}, Keys: keys,
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	got, ok := s.Get("c1")
	if !ok {
		t.Fatal("建完取不到渠道")
	}
	return got
}

// ★ 轮询：3 个启用 key ⇒ 连续 6 次必须是 a,b,c,a,b,c。
func TestPickKeyIdxRoundRobin(t *testing.T) {
	s := newKeyTestStore(t)
	upsertKeyChannel(t, s,
		ChannelKey{Key: "sk-a", Status: StatusEnabled},
		ChannelKey{Key: "sk-b", Status: StatusEnabled},
		ChannelKey{Key: "sk-c", Status: StatusEnabled},
	)
	if err := s.SetKeySelectionMode("c1", "round_robin"); err != nil {
		t.Fatalf("设选择模式失败: %v", err)
	}
	want := []string{"sk-a", "sk-b", "sk-c", "sk-a", "sk-b", "sk-c"}
	for i, w := range want {
		ch, _ := s.Get("c1")
		idx, key := s.PickKeyIdx(ch)
		if key != w {
			t.Fatalf("第 %d 次轮询应取 %s，实际 %s（下标 %d）", i+1, w, key, idx)
		}
	}
}

// 禁用的 key 不参与轮询；全禁 ⇒ (-1, "")（调用方据此判渠道不可用，而不是拿空串当 key 去发）。
func TestPickKeyIdxHonoursStatus(t *testing.T) {
	s := newKeyTestStore(t)
	upsertKeyChannel(t, s,
		ChannelKey{Key: "sk-off", Status: StatusManuallyDisabled},
		ChannelKey{Key: "sk-auto", Status: StatusAutoDisabled},
		ChannelKey{Key: "sk-ok", Status: StatusEnabled},
	)
	for i := 0; i < 4; i++ {
		ch, _ := s.Get("c1")
		if idx, key := s.PickKeyIdx(ch); key != "sk-ok" || idx != 2 {
			t.Fatalf("只应挑唯一个启用的 key（下标 2），实际 (%d,%q)", idx, key)
		}
	}

	s2 := newKeyTestStore(t)
	upsertKeyChannel(t, s2, ChannelKey{Key: "sk-x", Status: StatusAutoDisabled})
	ch, _ := s2.Get("c1")
	if idx, key := s2.PickKeyIdx(ch); idx != -1 || key != "" {
		t.Fatalf("全禁用应回 (-1,\"\")，实际 (%d,%q)", idx, key)
	}
}

// ★ 手动停用必须记原因，重新启用必须清干净（否则界面会一直显示"自动禁用"的旧原因）。
func TestSetKeyStatusRecordsAndClearsReason(t *testing.T) {
	s := newKeyTestStore(t)
	upsertKeyChannel(t, s, ChannelKey{Key: "sk-a", Status: StatusEnabled})

	if err := s.SetKeyStatus("c1", 0, StatusAutoDisabled, ReasonUpstreamAuth); err != nil {
		t.Fatalf("自动禁用失败: %v", err)
	}
	ch, _ := s.Get("c1")
	if ch.Keys[0].Reason != ReasonUpstreamAuth || ch.Keys[0].DisabledAt == 0 {
		t.Fatalf("应记下原因与时间，实际 reason=%q at=%d", ch.Keys[0].Reason, ch.Keys[0].DisabledAt)
	}

	if err := s.SetKeyStatus("c1", 0, StatusEnabled, ""); err != nil {
		t.Fatalf("重新启用失败: %v", err)
	}
	ch, _ = s.Get("c1")
	if ch.Keys[0].Reason != "" || ch.Keys[0].DisabledAt != 0 {
		t.Fatalf("启用后应清空原因与时间，实际 reason=%q at=%d", ch.Keys[0].Reason, ch.Keys[0].DisabledAt)
	}

	// 越界与非法状态都要报错（不能静默成功）
	if err := s.SetKeyStatus("c1", 9, StatusEnabled, ""); err == nil {
		t.Fatal("越界下标应报错")
	}
	if err := s.SetKeyStatus("nope", 0, StatusEnabled, ""); err == nil {
		t.Fatal("不存在的渠道应报错")
	}
	if err := s.SetKeyStatus("c1", 0, 9, ""); err == nil {
		t.Fatal("非法状态应报错")
	}
}

// ★ 追加去重：重复 key 会让轮询连续选中同一条（看起来像"轮询没生效"）。
func TestAddKeysDedupesAndSkipsBlank(t *testing.T) {
	s := newKeyTestStore(t)
	upsertKeyChannel(t, s, ChannelKey{Key: "sk-a", Status: StatusEnabled})

	added, err := s.AddKeys("c1", []string{"sk-a", " sk-b ", "", "sk-b", "sk-c"})
	if err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	if added != 2 {
		t.Fatalf("应只新增 sk-b 与 sk-c 两个，实际 %d", added)
	}
	ch, _ := s.Get("c1")
	if len(ch.Keys) != 3 {
		t.Fatalf("应有 3 个 key，实际 %d", len(ch.Keys))
	}
	if ch.Keys[1].Key != "sk-b" || ch.Keys[1].Status != StatusEnabled {
		t.Fatalf("新加的 key 应启用且去空白，实际 %+v", ch.Keys[1])
	}
	if !ch.ChannelInfo.MultiKey {
		t.Fatal("多 key 渠道应标记 MultiKey=true")
	}
}

func TestRemoveKey(t *testing.T) {
	s := newKeyTestStore(t)
	upsertKeyChannel(t, s,
		ChannelKey{Key: "sk-a", Status: StatusEnabled},
		ChannelKey{Key: "sk-b", Status: StatusEnabled},
		ChannelKey{Key: "sk-c", Status: StatusEnabled},
	)
	if err := s.RemoveKey("c1", 1); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	ch, _ := s.Get("c1")
	if len(ch.Keys) != 2 || ch.Keys[1].Key != "sk-c" {
		t.Fatalf("应删掉中间那个，实际 %+v", ch.Keys)
	}
	if err := s.RemoveKey("c1", 5); err == nil {
		t.Fatal("越界应报错")
	}
}

// MaskKey：短 key 全打码（露一半等于露全部），长 key 只留首尾 4 位。
func TestMaskKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"sk-abc", "******"},
		{"sk-12345", "********"},   // 正好 8 字符 ⇒ 全打码（露一半等于露全部）
		{"sk-123456789", "sk-1…6789"}, // 11 字符 ⇒ 首 4 + 末 4
	}
	for _, c := range cases {
		if got := MaskKey(c.in); got != c.want {
			t.Fatalf("MaskKey(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ★ KeysView 绝不返回明文（列表/轮询接口泄露 key 是最容易犯的错）。
func TestKeysViewNeverLeaksPlaintext(t *testing.T) {
	s := newKeyTestStore(t)
	const secret = "sk-super-secret-value-1234"
	upsertKeyChannel(t, s,
		ChannelKey{Key: secret, Status: StatusEnabled},
		ChannelKey{Key: "sk-bbbbbbbbbbbbbb", Status: StatusAutoDisabled, Reason: ReasonUpstreamAuth, DisabledAt: 1700000000},
	)
	views, err := s.KeysView("c1")
	if err != nil {
		t.Fatalf("取视图失败: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("应有 2 条视图，实际 %d", len(views))
	}
	for _, v := range views {
		if strings.Contains(v.Mask, secret) || v.Mask == secret {
			t.Fatalf("掩码里出现了明文: %+v", v)
		}
		if v.Index != 0 && v.Index != 1 {
			t.Fatalf("下标应逐条递增，实际 %+v", v)
		}
	}
	if !views[1].Unavailable {
		t.Fatal("自动禁用的 key 应标 unavailable（前端标红）")
	}
	if views[0].Unavailable {
		t.Fatal("启用的 key 不该标 unavailable")
	}
	if views[1].Reason != ReasonUpstreamAuth {
		t.Fatalf("应带出停用原因，实际 %q", views[1].Reason)
	}
}
