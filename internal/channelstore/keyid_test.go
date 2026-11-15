// keyid_test.go Key 稳定资源 ID 的判据（U-06 剩余段）。
//
// # 这条判据要钉住的缺陷
//
// 改造前轮询游标存的是**数组下标**（`s.rr[ch.ID] = pick + 1`）。
// 删掉第 0 把 key 之后所有下标左移 —— 游标 1 指到了原来第 2 把 key 身上。
// 表现：轮询**跳过**一把明明可用的 key，而每次请求都成功，日志里毫无异常。
//
// 下面 TestKeyCursorSurvivesDeletion 就是那个场景的逐字复现：
// 四个 key 转一圈之后删掉第一个，**下一把必须是它的后继**，不能跳过。
package channelstore

import (
	"encoding/json"
	"strings"
	"testing"
)

// keyCh 造一条带若干 key 的渠道（值即 "a"/"b"/"c" 便于读）。
func keyCh(values ...string) *Channel {
	keys := make([]ChannelKey, 0, len(values))
	for _, v := range values {
		keys = append(keys, ChannelKey{Key: v, Status: StatusEnabled})
	}
	return &Channel{ID: "ch", Name: "ch", Kind: "standard", Status: StatusEnabled, Keys: keys}
}

// TestKeyCursorSurvivesDeletion ★ 删掉别的 key 之后，轮询不得跳过任何可用 key。
//
// 这是本文件存在的理由。用旧的下标游标跑这条，第 5 次会拿到下标 1（c）——
// 跳过了 b。
func TestKeyCursorSurvivesDeletion(t *testing.T) {
	s := &Store{}
	ch := keyCh("a", "b", "c")

	// 转一圈：a → b → c → a，游标停在 "a" 上。
	for i, want := range []string{"a", "b", "c", "a"} {
		idx, key := s.PickKeyIdx(ch)
		if key != want {
			t.Fatalf("第 %d 次挑到 %q，want %q", i+1, key, want)
		}
		if idx != strings.Index("abc", want) && i < 3 {
			t.Fatalf("第 %d 次的声明下标 = %d，want %d", i+1, idx, strings.Index("abc", want))
		}
	}

	// 删掉第 0 把（"a"）—— 游标正指着它。
	ch.Keys = ch.Keys[1:] // 剩 b, c

	idx, key := s.PickKeyIdx(ch)
	if key != "b" {
		t.Fatalf("删掉 a 之后下一把 = %q（下标 %d），want \"b\" ——\n"+
			"游标指向的 key 被删时必须落到它的**后继**；落到 \"c\" 说明游标还在按下标走，"+
			"轮询会静默跳过 b", key, idx)
	}
	if idx != 0 {
		t.Fatalf("b 现在在声明下标 0，实际返回 %d", idx)
	}

	// 继续转：b 已选过 ⇒ 下一把是 c，然后绕回 b。
	if _, key = s.PickKeyIdx(ch); key != "c" {
		t.Fatalf("接着应挑到 c，实际 %q", key)
	}
	if _, key = s.PickKeyIdx(ch); key != "b" {
		t.Fatalf("应绕回 b，实际 %q", key)
	}
}

// TestKeyCursorSurvivesInsertion 在**前面插入**一把 key 不得打乱轮转。
//
// 旧实现里插入会让游标指到别人身上；新实现按 ID 找，插在前面毫无影响。
func TestKeyCursorSurvivesInsertion(t *testing.T) {
	s := &Store{}
	ch := keyCh("a", "b", "c")

	_, _ = s.PickKeyIdx(ch) // a
	if _, key := s.PickKeyIdx(ch); key != "b" {
		t.Fatalf("第二次应挑到 b，实际 %q", key)
	}

	// 在前面插一把 "z"
	ch.Keys = append([]ChannelKey{{Key: "z", Status: StatusEnabled}}, ch.Keys...)

	// 游标在 b 上 ⇒ 后继仍是 c（与插入无关）。
	if _, key := s.PickKeyIdx(ch); key != "c" {
		t.Fatalf("插入后应继续挑 c，实际 %q —— 插入打乱了轮转", key)
	}
	// 然后 z → a → b（声明顺序的新一圈）
	if _, key := s.PickKeyIdx(ch); key != "z" {
		t.Fatalf("c 是最后一个 ⇒ 应绕回第一个 z，实际 %q", key)
	}
}

// TestKeyIDsAreStableAndDistinct ID 必须确定、互不相同，且与位置无关。
func TestKeyIDsAreStableAndDistinct(t *testing.T) {
	keys := []ChannelKey{{Key: "alpha"}, {Key: "beta"}, {Key: "gamma"}}
	first := keyIDsOf(keys)

	// 确定性：再算一遍结果相同（没有随机、没有位置依赖以外的因素）。
	second := keyIDsOf(keys)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("第 %d 个 ID 不确定：%q vs %q", i, first[i], second[i])
		}
	}

	// 互不相同。
	seen := map[string]bool{}
	for _, id := range first {
		if id == "" {
			t.Fatal("ID 不该为空")
		}
		if seen[id] {
			t.Fatalf("ID 重复：%q", id)
		}
		seen[id] = true
	}

	// 与位置无关：把列表**反过来**，每把 key 的 ID 不变。
	reversed := []ChannelKey{{Key: "gamma"}, {Key: "beta"}, {Key: "alpha"}}
	revIDs := keyIDsOf(reversed)
	if revIDs[0] != first[2] || revIDs[2] != first[0] {
		t.Fatalf("ID 依赖了位置：正序 %v vs 反序 %v —— 那删除后 ID 会整体变化，缺陷原样复现", first, revIDs)
	}

	// 值变了 ID 就变（换 key 是一次真实的资源替换）。
	changed := keyIDsOf([]ChannelKey{{Key: "alpha-rotated"}})
	if changed[0] == first[0] {
		t.Fatal("换 key 之后 ID 没变")
	}
}

// TestKeyIDsDisambiguateDuplicates 同一渠道里两把**值相同**的 key 必须得到不同 ID，
// 否则游标永远停在同一个 ID 上，轮询退化。
func TestKeyIDsDisambiguateDuplicates(t *testing.T) {
	ids := keyIDsOf([]ChannelKey{{Key: "same"}, {Key: "same"}, {Key: "same"}})
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			t.Fatal("重复 key 的 ID 不该为空")
		}
		if seen[id] {
			t.Fatalf("重复 key 得到了相同 ID %q：%v", id, ids)
		}
		seen[id] = true
	}
	// 而且轮询不能死循环在同一个 ID 上。
	s := &Store{}
	ch := &Channel{ID: "dup", Keys: []ChannelKey{
		{Key: "same", Status: StatusEnabled},
		{Key: "same", Status: StatusEnabled},
	}}
	got := map[int]int{}
	for i := 0; i < 4; i++ {
		idx, _ := s.PickKeyIdx(ch)
		got[idx]++
	}
	if got[0] != 2 || got[1] != 2 {
		t.Fatalf("重复 key 的轮询分布 = %v，want 各 2 次", got)
	}
}

// TestKeyCursorIsPerChannel 两个渠道的游标互不影响（即使 key 值相同）。
func TestKeyCursorIsPerChannel(t *testing.T) {
	s := &Store{}
	a := &Channel{ID: "A", Keys: []ChannelKey{{Key: "x", Status: 1}, {Key: "y", Status: 1}}}
	b := &Channel{ID: "B", Keys: []ChannelKey{{Key: "x", Status: 1}, {Key: "y", Status: 1}}}

	if _, key := s.PickKeyIdx(a); key != "x" {
		t.Fatalf("A 第一次应挑 x，实际 %q", key)
	}
	// B 是另一个渠道：它自己的第一次也应该是 x，不该被 A 的游标带跑。
	if _, key := s.PickKeyIdx(b); key != "x" {
		t.Fatalf("B 第一次应挑 x，实际 %q —— 两个渠道共享了游标", key)
	}
	if _, key := s.PickKeyIdx(a); key != "y" {
		t.Fatalf("A 第二次应挑 y，实际 %q", key)
	}
}

// TestKeyIDIsNotSerialized ID 不得落盘。
//
// ⚠ 它是 Key 值的短哈希：写进配置文件等于把密钥指纹存了一份。
// 渠道列表本来就要掩码，没必要顺手泄漏一个可比对的属性。
func TestKeyIDIsNotSerialized(t *testing.T) {
	ch := &Channel{ID: "ch", Name: "ch", Keys: []ChannelKey{{Key: "secret-value", Status: 1}}}
	ch.Keys[0].ID = "k0123456789" // 即便显式塞进去也不许被序列化

	raw, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, "k0123456789") {
		t.Fatalf("Key.ID 被序列化了（json 标签应为 `-`）：%s", body)
	}
	// 顺带确认 key 值本身在 Channel 结构里是可以序列化的（掩码由上层负责），
	// 这条只是防止有人把 json:"-" 加到 Key 上造成"落盘丢 key"。
	if !strings.Contains(body, "secret-value") {
		t.Fatal("Key 值本身必须能序列化 —— 否则落盘会丢 key")
	}
}
