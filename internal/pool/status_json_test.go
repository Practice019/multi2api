// status_json_test.go 「成功」列的 JSON 契约。
//
// # 守的是一个我自己修出来的 bug 的第二半
//
// 用户报"缺少成功列"，我加上了列 —— 但**列加上了数字还是看不见**：
// `SuccessCount` 原来带 `omitempty`，于是值为 0 时**字段整个不出现在
// JSON 里**，前端 `esc(a.success_count)` 收到 `undefined` → **空白单元格**。
//
//	0     = 确定的事实「这个号一次都没成功过」
//	空白  = 读的人分不清是「0」还是「我们没在数」
//
// 用户刚要求每个上游都要有这一列 —— 一列全空白比没有这一列更困惑。
//
// # 为什么必须单独一条测试
//
// 这个缺陷**不会让任何既有测试变红**：
//
//	· 列集断言（列 id 在不在）与 JSON 形状无关
//	· 计数断言（`st.SuccessCount == 2`）读的是**结构体字段**，
//	  而 omitempty 只影响**序列化**，字段本身照样是 2
//
// 也就是说"列报了、计数也对了"，只有**中间的 JSON 那一层**是坏的 ——
// 而它恰好是前端唯一能看到的那层。
package pool

import (
	"encoding/json"
	"strings"
	"testing"
)

// 「成功」计数为 0 时，JSON 里必须有这个字段，且值为 0（不是缺字段）。
//
// 这条直接对应用户会看到的画面：新加的账号成功数是 0，
// 那一格该显示 `0`，不该是空的。
func TestStatusJSONKeepsZeroSuccessCount(t *testing.T) {
	st := Status{UID: "u1"}
	st.SuccessCount = 0

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	if !strings.Contains(got, `"success_count":0`) {
		t.Errorf("成功计数为 0 时 JSON 里必须是 `\"success_count\":0`，实际:\n%s\n\n"+
			"带 omitempty 会让字段整个消失 → 前端渲染成**空白**，"+
			"而空白与 0 对读的人来说是两件事（本仓 TokenExpireSec 的同款教训）。", got)
	}
}

// 非零值当然也要在（防"顺手改成恒不发"这种反向错法）。
func TestStatusJSONCarriesNonZeroSuccessCount(t *testing.T) {
	st := Status{UID: "u1"}
	st.SuccessCount = 7

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"success_count":7`) {
		t.Errorf("成功计数 7 必须出现在 JSON 里，实际:\n%s", raw)
	}
}

// 对照组：同结构体里的 in_flight / breaker_fails **也**必须在 0 时出现。
//
// # 为什么把它们一起钉住
//
// 这三个是同一类"计数器"字段，而它们的行为原本**不一致**：
//
//	in_flight      `json:"in_flight"`        ← 0 会出现
//	breaker_fails  `json:"breaker_fails"`    ← 0 会出现
//	success_count  `json:"success_count,omitempty"` ← 0 **消失**（bug）
//
// 修法有两种方向：把 success_count 的 omitempty 去掉（选了这个，
// 因为要显示它），或者给另外两个也加上（那是错的 —— 会引入同一个 bug）。
// 这条断言把"另外两个也必须在 0 时出现"钉住，防止有人"统一"成带 omitempty。
func TestStatusJSONKeepsZeroCounters(t *testing.T) {
	st := Status{UID: "u1"} // 全部计数为 0

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	for _, want := range []string{`"in_flight":0`, `"breaker_fails":0`, `"success_count":0`} {
		if !strings.Contains(got, want) {
			t.Errorf("计数器 %s 在 0 时必须出现在 JSON 里（前端要显示 0 而不是空白），实际:\n%s",
				want, got)
		}
	}
}

// 落盘那份（stateAccount）**应当保留 omitempty** —— 两条路径的取舍不同。
//
// # 为什么这里允许（并要求）omitempty
//
//   - state.json 是内部格式，Go 零值天然 round-trip，
//     不需要像前端那样区分"缺字段"与"是 0"
//   - 全池几百个账号时省下的字节是有意义的
//   - 更重要的是：把这两个契约**显式区分开**，防止有人看到
//     `Status` 那一份去掉了 omitempty，就顺手也把落盘那份改掉
//     （或反过来，把 Status 那份加回来）。两条路径各有理由，不是疏漏。
func TestStateAccountKeepsOmitEmptyForSize(t *testing.T) {
	// 用反射读 tag 太重，这里直接断言"落盘结构体的 tag 文本"不存在于
	// Status 上 —— 更有意义的是行为：落盘 0 值不该写成字段。
	//
	// 实测手段：造一个全零的 stateAccount，序列化后不该出现 success_count。
	sa := stateAccount{}
	raw, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"success_count"`) {
		t.Errorf("落盘的 stateAccount 应保留 omitempty（省字节，零值天然 round-trip），"+
			"实际写进了 JSON:\n%s", raw)
	}
}
