// convert_test.go —— 边界翻译层的不变量守卫（B1）。
//
// 这些用例**不需要数据库**：它们只验"结构体 ⇄ 结构体"的映射本身。
// 为什么值得单独一组：这一层错了不会编译失败、也不会报错 —— 表现是
// "key 状态挂了三天才发现少了一把"或"改一次渠道把别人的设置清空了"。
package channelstore

import (
	"encoding/json"
	"testing"

	"workbuddy2api/internal/store"
)

// TestConvertKeyStateRoundTrip（I1/I2）Key 列与三张下标表必须一起往返。
//
// 两条断言分别钉住两种错法：
//
//	① 少写一张表（例如只写 status 不写 reason/disabled_at）⇒ 往返后 reason 丢；
//	② 顺序错（先写表再 SetKeys）⇒ 删 key 后旧下标残留（幽灵状态）。
func TestConvertKeyStateRoundTrip(t *testing.T) {
	keys := []ChannelKey{
		{Key: "sk-a", Status: StatusEnabled},
		{Key: "sk-b", Status: StatusManuallyDisabled, Reason: ReasonManual, DisabledAt: 111},
		{Key: "sk-c", Status: StatusAutoDisabled, Reason: ReasonUpstreamAuth, DisabledAt: 222},
	}
	row := &store.Channel{}
	applyKeysToRow(row, keys)

	if got := row.GetKeys(); len(got) != 3 || got[0] != "sk-a" || got[2] != "sk-c" {
		t.Fatalf("Key 列往返错: %#v", got)
	}
	if row.ChannelInfo.MultiKeySize != 3 || !row.ChannelInfo.IsMultiKey {
		t.Fatalf("多 Key 事实没写全: %+v", row.ChannelInfo)
	}

	back := keysFromRow(row)
	if len(back) != 3 {
		t.Fatalf("往返后 key 数 = %d，want 3", len(back))
	}
	if back[0].Status != StatusEnabled {
		t.Errorf("I2：没被记录的下标必须是**启用**，实际 %d", back[0].Status)
	}
	if back[1].Status != StatusManuallyDisabled || back[1].Reason != ReasonManual || back[1].DisabledAt != 111 {
		t.Errorf("① 状态/原因/时刻没一起往返: %+v", back[1])
	}
	if back[2].Status != StatusAutoDisabled || back[2].Reason != ReasonUpstreamAuth || back[2].DisabledAt != 222 {
		t.Errorf("① 状态/原因/时刻没一起往返: %+v", back[2])
	}

	// ② 删掉中间那把（下标 1）⇒ 原下标 2 的状态必须跟着搬到 1，且**不许**残留 2。
	applyKeysToRow(row, []ChannelKey{keys[0], keys[2]})
	if row.ChannelInfo.MultiKeySize != 2 {
		t.Fatalf("删 key 后 MultiKeySize = %d，want 2", row.ChannelInfo.MultiKeySize)
	}
	if _, stale := row.ChannelInfo.MultiKeyStatusList[2]; stale {
		t.Errorf("I1：删 key 后仍残留越界下标 2（顺序写反了）: %+v", row.ChannelInfo.MultiKeyStatusList)
	}
	after := keysFromRow(row)
	if len(after) != 2 || after[1].Status != StatusAutoDisabled || after[1].Reason != ReasonUpstreamAuth {
		t.Fatalf("删 key 后状态错位: %+v", after)
	}
}

// TestConvertModeMapping（I3）round_robin ⇄ polling 只在这一层翻译；不认识的值不许瞎映射。
func TestConvertModeMapping(t *testing.T) {
	if got := modeToStore("round_robin"); got != store.MultiKeyModePolling {
		t.Errorf("round_robin 应翻成 %q，实际 %q", store.MultiKeyModePolling, got)
	}
	if got := modeToStore("random"); got != store.MultiKeyModeRandom {
		t.Errorf("random 应翻成 %q，实际 %q", store.MultiKeyModeRandom, got)
	}
	if got := modeToStore(""); got != "" {
		t.Errorf("空串应保持空串，实际 %q", got)
	}
	if got := modeFromStore(store.MultiKeyModePolling); got != "round_robin" {
		t.Errorf("polling 应翻回 round_robin，实际 %q", got)
	}
	if got := modeFromStore(store.MultiKeyModeRandom); got != "random" {
		t.Errorf("random 应翻回 random，实际 %q", got)
	}
	if got := modeToStore("nonsense"); got != "" {
		t.Errorf("不认识的值必须落空串（PickKeyIdx 会当随机），实际 %q", got)
	}
}

// TestConvertExtrasMergeWrite（I4）OtherSettings 必须**合并写**：别人的键逐字保留。
func TestConvertExtrasMergeWrite(t *testing.T) {
	row := &store.Channel{OtherSettings: `{"别人的键":{"a":1},"n":7}`}
	extrasToRow(row, channelExtras{ContextLength: 200000, MaxOutputTokens: 8192})

	var all map[string]json.RawMessage
	if err := json.Unmarshal([]byte(row.OtherSettings), &all); err != nil {
		t.Fatalf("合并后不是合法 JSON: %v (%s)", err, row.OtherSettings)
	}
	if string(all["别人的键"]) != `{"a":1}` {
		t.Errorf("别人的键被改动了: %s", all["别人的键"])
	}
	if _, ok := all["n"]; !ok {
		t.Errorf("别人的标量键被删了: %s", row.OtherSettings)
	}

	back := extrasFromRow(row)
	if back.ContextLength != 200000 || back.MaxOutputTokens != 8192 {
		t.Fatalf("往返丢值: %+v", back)
	}

	// 清零 ⇒ 只删自己的键，别人的还在。
	//
	// ⚠ all = nil 不能省：json.Unmarshal 往**已有 map** 里解是**合并**语义，
	//   不清空的话上一次的 channelExtrasKey 会留在 map 里，断言就成了假阳性。
	extrasToRow(row, channelExtras{})
	all = nil
	if err := json.Unmarshal([]byte(row.OtherSettings), &all); err != nil {
		t.Fatalf("清零后不是合法 JSON: %v", err)
	}
	if _, ok := all[channelExtrasKey]; ok {
		t.Errorf("清零后自己的键应被删: %s", row.OtherSettings)
	}
	if string(all["别人的键"]) != `{"a":1}` || all["n"] == nil {
		t.Errorf("清零伤到了别人的键: %s", row.OtherSettings)
	}

	// 脏数据（不是 JSON 对象）⇒ **不覆盖**（不许替别人清洗）。
	dirty := &store.Channel{OtherSettings: "not-json"}
	extrasToRow(dirty, channelExtras{ContextLength: 1})
	if dirty.OtherSettings != "not-json" {
		t.Errorf("解析失败时必须原样保留，实际 %q", dirty.OtherSettings)
	}
}

// TestConvertMaskedKeysDoNotTouchSource（I5）掩码只改副本。
func TestConvertMaskedKeysDoNotTouchSource(t *testing.T) {
	src := []ChannelKey{{Key: "sk-abcdefghijkl", Status: StatusEnabled}}
	masked := maskedKeys(src)
	if masked[0].Key == src[0].Key {
		t.Fatal("掩码没生效")
	}
	if src[0].Key != "sk-abcdefghijkl" {
		t.Fatalf("掩码改到了源数据（明文会被写回库）: %q", src[0].Key)
	}
	if masked[0].Status != StatusEnabled || masked[0].Key == "" {
		t.Fatalf("掩码丢了状态或掩码为空: %+v", masked[0])
	}
}

// TestConvertRowByExternalIDEmpty（I6）空串一律当"不存在"，不许去命中所有非外部行。
func TestConvertRowByExternalIDEmpty(t *testing.T) {
	row, ok, err := rowByExternalID("")
	if err != nil || ok || row != nil {
		t.Fatalf("空串应回 (nil,false,nil)，实际 (%v,%v,%v)", row, ok, err)
	}
	if _, err := mustRowByExternalID("  "); err == nil {
		t.Fatal("空白 id 也应报「渠道不存在」")
	}
}
