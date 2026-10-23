// param_override_test.go —— 把 ParamOverrides 的每条规则、每类失败都钉住。
//
// # 为什么是表驱动（而不是每个场景一个函数）
//
// 覆盖规则的失效方式**都不报错**：删错了一个键、default 覆盖了客户端的显式值、
// 重新编码把 seed 改了值 —— 上游照样回 200，只是答非所问。这类错误只能在
// 测试里把"输入 body × 规则 → 期望 body"铺成一张表来看，否则漏掉的那一行
// 永远不会有人想起来。
//
// # 本文件钉住三类"看不见"的性质
//
//  1. **原子性**：失败时 body 与入参逐字节相同（不是"大致相同"）。
//  2. **无改动不重新编码**：连 0.50 这种字面量都不能变（否则每次升级
//     都在给所有客户端制造差异面）。
//  3. **未触碰的大整数不丢精度**：seed 9007199254740993 必须原样出现
//     （UseNumber 那条约束的回归测试）。
package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// paramTestDecode 把 JSON 解成 map（UseNumber），供断言用。
func paramTestDecode(t *testing.T, body string) map[string]any {
	t.Helper()
	obj, err := paramDecodeObject([]byte(body))
	if err != nil {
		t.Fatalf("测试数据不是 json 对象：%v（%s）", err, body)
	}
	return obj
}

// paramTestAssertJSONEqual 按 JSON 语义比较两份 body。
//
// # 为什么不用字符串相等
//
// 真正改动时 body 会被重新 Marshal（键按字典序），期望值若按手写顺序写
// 会永远不等 —— 那种失败信息只会让人去调期望字符串的顺序，掩盖真正的差异。
// 语义比较后，键顺序不再影响断言；而"顺序/字面量"这类**必须**钉住的性质
// 由 paramTestAssertSameBytes 单独负责。
func paramTestAssertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	gotObj, err := paramDecodeObject(got)
	if err != nil {
		t.Fatalf("结果不是 json 对象：%v（%s）", err, got)
	}
	wantObj := paramTestDecode(t, want)
	if !paramValueEqual(gotObj, wantObj) {
		t.Fatalf("body 不符\n 得到：%s\n 想要：%s", got, want)
	}
}

// paramTestAssertSameBytes 断言字节完全一致（"没有改动"的强判据）。
func paramTestAssertSameBytes(t *testing.T, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("字节必须原样保留\n 得到：%q\n 想要：%q", got, want)
	}
}

// TestParamOverrideSet set：强制改写 / 新增 / 嵌套建立 / 数组下标。
func TestParamOverrideSet(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		rules []ParamRule
		want  string
	}{
		{
			name:  "改写客户端已有的值",
			body:  `{"model":"x","temperature":0.2}`,
			rules: []ParamRule{{Op: ParamOpSet, Path: "temperature", Value: 1}},
			want:  `{"model":"x","temperature":1}`,
		},
		{
			name:  "新增客户端没传的字段",
			body:  `{"model":"x"}`,
			rules: []ParamRule{{Op: ParamOpSet, Path: "top_k", Value: 40}},
			want:  `{"model":"x","top_k":40}`,
		},
		{
			// 中间节点不存在时自动建对象 —— 否则跨三层的规则永远写不成。
			name:  "嵌套路径自动建中间对象",
			body:  `{"model":"x"}`,
			rules: []ParamRule{{Op: ParamOpSet, Path: "stream_options.include_usage", Value: true}},
			want:  `{"model":"x","stream_options":{"include_usage":true}}`,
		},
		{
			name:  "三层嵌套",
			body:  `{}`,
			rules: []ParamRule{{Op: ParamOpSet, Path: "a.b.c", Value: 1}},
			want:  `{"a":{"b":{"c":1}}}`,
		},
		{
			// 数组按下标寻址：messages 是数组，规则要能打到第 0 条上。
			name:  "数组下标路径",
			body:  `{"messages":[{"role":"user","content":"hi"}]}`,
			rules: []ParamRule{{Op: ParamOpSet, Path: "messages.0.role", Value: "system"}},
			want:  `{"messages":[{"role":"system","content":"hi"}]}`,
		},
		{
			// 显式 null 是合法操作（客户端要"写 null"来表达关闭）。
			name:  "把值设成 null",
			body:  `{"model":"x","temperature":0.2}`,
			rules: []ParamRule{{Op: ParamOpSet, Path: "temperature", Value: nil}},
			want:  `{"model":"x","temperature":null}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ApplyParamOverrides([]byte(c.body), c.rules)
			if len(res.Problems) > 0 {
				t.Fatalf("不该有问题：%v", res.Err())
			}
			if !res.Changed {
				t.Fatal("应当判定为已改动")
			}
			if len(res.Applied) != len(c.rules) {
				t.Fatalf("Applied=%v，期望 %d 条", res.Applied, len(c.rules))
			}
			paramTestAssertJSONEqual(t, res.Body, c.want)
		})
	}
}

// TestParamOverrideSetSameValueIsNoop set 成**同一个值**不算改动。
//
// # 为什么这条重要
//
// 它是"无改动不重新编码"的入口：规则配了、但客户端本来就发对了值时，
// 重新编码毫无收益，却会改掉键顺序与 0.50 这类字面量。
//
// ⚠ 值来源不同：规则值来自 Go（float64），body 里的值来自 json.Number。
// 直接 DeepEqual 会判成"不同" —— 那会让上面的 no-op 失效且无声。
// 所以这里刻意用 float64(1) 对 json.Number("1")。
func TestParamOverrideSetSameValueIsNoop(t *testing.T) {
	body := []byte(`{"temperature":1,"seed":42}`)
	res := ApplyParamOverrides(body, []ParamRule{{Op: ParamOpSet, Path: "temperature", Value: float64(1)}})
	if res.Changed {
		t.Fatalf("值相同不该判定为改动：Applied=%v", res.Applied)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("没有实际改动时 Applied 必须为空，得到 %v", res.Applied)
	}
	paramTestAssertSameBytes(t, res.Body, body)
}

// TestParamOverrideDelete delete：存在则删、不存在是 no-op。
func TestParamOverrideDelete(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		rules   []ParamRule
		want    string
		changed bool
	}{
		{
			name:    "删除顶层字段",
			body:    `{"model":"x","reasoning_effort":"off"}`,
			rules:   []ParamRule{{Op: ParamOpDelete, Path: "reasoning_effort"}},
			want:    `{"model":"x"}`,
			changed: true,
		},
		{
			// 删除是幂等的：字段本来就不在 ⇒ "保证不在"已经成立，不该报错，
			// 否则一条无害的规则会经原子失败拖垮整张规则表。
			name:    "删除不存在的字段是 no-op",
			body:    `{"model":"x"}`,
			rules:   []ParamRule{{Op: ParamOpDelete, Path: "reasoning_effort"}},
			want:    `{"model":"x"}`,
			changed: false,
		},
		{
			name:    "删除嵌套字段",
			body:    `{"stream_options":{"include_usage":true,"x":1}}`,
			rules:   []ParamRule{{Op: ParamOpDelete, Path: "stream_options.x"}},
			want:    `{"stream_options":{"include_usage":true}}`,
			changed: true,
		},
		{
			// 删除后留下空对象而不是把父对象一起删掉：父对象可能还有别的键，
			// 而"顺手清理空容器"会让规则的效果超出它字面表达的范围。
			name:    "删除嵌套唯一字段后保留空对象",
			body:    `{"stream_options":{"include_usage":true}}`,
			rules:   []ParamRule{{Op: ParamOpDelete, Path: "stream_options.include_usage"}},
			want:    `{"stream_options":{}}`,
			changed: true,
		},
		{
			name:    "删除数组元素",
			body:    `{"stop":[1,2,3]}`,
			rules:   []ParamRule{{Op: ParamOpDelete, Path: "stop.1"}},
			want:    `{"stop":[1,3]}`,
			changed: true,
		},
		{
			name:    "删除越界下标是 no-op",
			body:    `{"stop":[1]}`,
			rules:   []ParamRule{{Op: ParamOpDelete, Path: "stop.9"}},
			want:    `{"stop":[1]}`,
			changed: false,
		},
		{
			// 路径穿过标量：没有更深层级可删，静默 no-op（不报错）。
			name:    "穿过标量的删除是 no-op",
			body:    `{"a":1}`,
			rules:   []ParamRule{{Op: ParamOpDelete, Path: "a.b"}},
			want:    `{"a":1}`,
			changed: false,
		},
		{
			name:    "穿过缺失父节点的删除是 no-op",
			body:    `{"a":{"b":1}}`,
			rules:   []ParamRule{{Op: ParamOpDelete, Path: "x.y"}},
			want:    `{"a":{"b":1}}`,
			changed: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(c.body)
			res := ApplyParamOverrides(body, c.rules)
			if len(res.Problems) > 0 {
				t.Fatalf("删除不该失败：%v", res.Err())
			}
			if res.Changed != c.changed {
				t.Fatalf("changed=%v，想要 %v（Applied=%v）", res.Changed, c.changed, res.Applied)
			}
			if !c.changed {
				paramTestAssertSameBytes(t, res.Body, body)
				return
			}
			paramTestAssertJSONEqual(t, res.Body, c.want)
		})
	}
}

// TestParamOverrideDefault default-only：客户端传了就必须听客户端的。
func TestParamOverrideDefault(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		rules   []ParamRule
		want    string
		changed bool
	}{
		{
			name:    "缺失时设默认值",
			body:    `{"model":"x"}`,
			rules:   []ParamRule{{Op: ParamOpDefault, Path: "temperature", Value: 1}},
			want:    `{"model":"x","temperature":1}`,
			changed: true,
		},
		{
			name:    "已存在时不动",
			body:    `{"model":"x","temperature":0.2}`,
			rules:   []ParamRule{{Op: ParamOpDefault, Path: "temperature", Value: 1}},
			want:    `{"model":"x","temperature":0.2}`,
			changed: false,
		},
		{
			// 零值也算"客户端传了"：0 与"没传"语义完全不同
			// （0 常用来表达"不要随机"），用存在性判定而不是判零。
			name: "已有的零值不被覆盖",
			body: `{"temperature":0,"top_p":0,"stream":false,"stop":""}`,
			rules: []ParamRule{
				{Op: ParamOpDefault, Path: "temperature", Value: 1},
				{Op: ParamOpDefault, Path: "top_p", Value: 1},
				{Op: ParamOpDefault, Path: "stream", Value: true},
				{Op: ParamOpDefault, Path: "stop", Value: "END"},
			},
			want:    `{"temperature":0,"top_p":0,"stream":false,"stop":""}`,
			changed: false,
		},
		{
			// null 是显式选择（"关闭这个可选参数"），不是"没传"。
			// 把它当缺失会用默认值改写客户端的意图 —— 见文件头边界 4。
			name:    "已有的 null 不被覆盖",
			body:    `{"temperature":null}`,
			rules:   []ParamRule{{Op: ParamOpDefault, Path: "temperature", Value: 1}},
			want:    `{"temperature":null}`,
			changed: false,
		},
		{
			name:    "嵌套缺失时建对象并设默认值",
			body:    `{"model":"x"}`,
			rules:   []ParamRule{{Op: ParamOpDefault, Path: "stream_options.include_usage", Value: true}},
			want:    `{"model":"x","stream_options":{"include_usage":true}}`,
			changed: true,
		},
		{
			name:    "嵌套父对象在、子字段缺失",
			body:    `{"stream_options":{}}`,
			rules:   []ParamRule{{Op: ParamOpDefault, Path: "stream_options.include_usage", Value: true}},
			want:    `{"stream_options":{"include_usage":true}}`,
			changed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(c.body)
			res := ApplyParamOverrides(body, c.rules)
			if len(res.Problems) > 0 {
				t.Fatalf("不该有问题：%v", res.Err())
			}
			if res.Changed != c.changed {
				t.Fatalf("changed=%v，想要 %v（Applied=%v）", res.Changed, c.changed, res.Applied)
			}
			if !c.changed {
				paramTestAssertSameBytes(t, res.Body, body)
				return
			}
			paramTestAssertJSONEqual(t, res.Body, c.want)
		})
	}
}

// TestParamOverrideNestedErrors 嵌套路径上的**失败**：非对象中间节点、非法下标。
//
// 每个用例的前提都是"唯一能成功的做法是静默破坏客户端数据"：
// 把字符串替换成对象、给数组补空元素。这些必须在解析/应用期拒绝。
func TestParamOverrideNestedErrors(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		rules     []ParamRule
		wantInErr string
	}{
		{
			name:      "穿过标量建对象会丢数据",
			body:      `{"metadata":"abc"}`,
			rules:     []ParamRule{{Op: ParamOpSet, Path: "metadata.trace_id", Value: "t1"}},
			wantInErr: "不是对象/数组",
		},
		{
			name:      "数组下标越界不补空元素",
			body:      `{"stop":[1]}`,
			rules:     []ParamRule{{Op: ParamOpSet, Path: "stop.5", Value: "x"}},
			wantInErr: "越界",
		},
		{
			name:      "数组上用非数字段",
			body:      `{"stop":[1]}`,
			rules:     []ParamRule{{Op: ParamOpSet, Path: "stop.first", Value: "x"}},
			wantInErr: "不是数组下标",
		},
		{
			// 负数下标必须**报错**，不能当"路径不存在"静默放过：
			// 运维写过 new-api 的规则，会以为 -1 是"最后一个"。
			// 静默 no-op 的表现是"配了但永远不生效"，日志一切正常。
			name:      "负数下标报错而不是静默 no-op",
			body:      `{"stop":[1,2]}`,
			rules:     []ParamRule{{Op: ParamOpDelete, Path: "stop.-1"}},
			wantInErr: "负数下标",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ApplyParamOverrides([]byte(c.body), c.rules)
			if len(res.Problems) == 0 {
				t.Fatal("应当报出问题")
			}
			if !strings.Contains(res.Err().Error(), c.wantInErr) {
				t.Fatalf("错误说明里应当提到 %q，得到：%v", c.wantInErr, res.Err())
			}
			// ★ 失败必须交出**原始** body。
			paramTestAssertSameBytes(t, res.Body, []byte(c.body))
			if res.Changed {
				t.Fatal("失败时 Changed 必须为 false")
			}
			if len(res.Applied) != 0 {
				t.Fatalf("失败时 Applied 必须清空（半改的规则不该被记成已生效），得到 %v", res.Applied)
			}
		})
	}
}

// TestParamOverrideAtomicity 中途失败 ⇒ 前面已应用的规则**一起撤销**。
//
// # 为什么这不是"保守"而是"必须"
//
// 半改的 body 是没有任何人设计过的东西。这条用例里规则表是
// 「删掉 reasoning_effort、再把 metadata.trace_id 设上」：第二条失败时，
// 半改的 body 已经删掉了用户明确要的推理强度，却也没拿到第二条的效果。
// 上游会按一份"既不是客户端要的、也不是运维要的"body 回答，
// 而错误现场只会显示"上游 400"。
func TestParamOverrideAtomicity(t *testing.T) {
	body := []byte(`{"model":"x","reasoning_effort":"high","metadata":"abc"}`)
	first := ParamRule{Op: ParamOpDelete, Path: "reasoning_effort"}
	second := ParamRule{Op: ParamOpSet, Path: "metadata.trace_id", Value: "t1"}

	// 先确认第一条**单独**应用是有效的 —— 否则本用例证明不了"回滚"。
	alone := ApplyParamOverrides(body, []ParamRule{first})
	if !alone.Changed {
		t.Fatal("前提不成立：第一条规则单独应用应当改动 body")
	}
	paramTestAssertJSONEqual(t, alone.Body, `{"model":"x","metadata":"abc"}`)

	res := ApplyParamOverrides(body, []ParamRule{first, second})
	if len(res.Problems) != 1 {
		t.Fatalf("应当恰好一个问题，得到 %v", res.Problems)
	}
	if res.Problems[0].Index != 1 {
		t.Fatalf("问题应指向第 2 条规则（index=1），得到 %d", res.Problems[0].Index)
	}
	if !strings.Contains(res.Problems[0].Rule, "metadata.trace_id") {
		t.Fatalf("问题里应带出错的规则，得到 %q", res.Problems[0].Rule)
	}
	paramTestAssertSameBytes(t, res.Body, body)
	if res.Changed {
		t.Fatal("回滚后 Changed 必须为 false")
	}
	if err := res.Err(); err == nil || !strings.Contains(err.Error(), "原 body 保持不变") {
		t.Fatalf("Err() 必须说明 body 未被修改，得到 %v", err)
	}
}

// TestParamOverrideNoReencode 没有改动时**逐字节**返回原 body。
//
// # 用户能感知的后果
//
// 重新编码会：按字典序重排键、把 0.50 写成 0.5、把大整数压成 float64。
// 对没配覆盖规则的渠道（绝大多数）请求必须原样转发 —— 否则每次升级
// 都在给所有客户端制造无谓的差异面（缓存 key 变化、上游审计对不上）。
func TestParamOverrideNoReencode(t *testing.T) {
	bodies := []string{
		`{"model":"x","temperature":0.50,"seed":9007199254740993}`,
		`{"z":1,"a":2}`,      // 键顺序必须保住
		`{"x": 1,\n "y": 2}`, // 空白必须保住
	}
	for _, b := range bodies {
		t.Run(b, func(t *testing.T) {
			body := []byte(b)
			// ① 没有规则
			res := ApplyParamOverrides(body, nil)
			paramTestAssertSameBytes(t, res.Body, body)
			if res.Changed {
				t.Fatal("没有规则时不该判定为改动")
			}
			// ② 规则全部 no-op（删除不存在的字段）
			res = ApplyParamOverrides(body, []ParamRule{{Op: ParamOpDelete, Path: "nope"}})
			paramTestAssertSameBytes(t, res.Body, body)
			if res.Changed {
				t.Fatal("全是 no-op 时不该重新编码")
			}
		})
	}
}

// TestParamOverridePreservesUntouchedNumbers 改动一个字段时，**未触碰**的数字不能变值。
//
// # 回归的 bug 形态
//
// 用默认的 map[string]any 解码会把所有数字变成 float64：
//
//	seed = 9007199254740993  ->  9007199254740992   （2^53 以上丢精度）
//	temperature = 0.50       ->  0.5               （字面量被改写）
//	best_of = 1e3            ->  1000
//
// 覆盖规则只改了 temperature，却把 seed 改了值 —— 只有在"配了覆盖 +
// 恰好带大整数"时出现，线上极难归因。UseNumber 是本文件对它的唯一防线。
func TestParamOverridePreservesUntouchedNumbers(t *testing.T) {
	body := []byte(`{"model":"x","seed":9007199254740993,"temperature":0.50,"stop":["END"]}`)
	res := ApplyParamOverrides(body, []ParamRule{{Op: ParamOpSet, Path: "temperature", Value: 1}})
	if len(res.Problems) > 0 {
		t.Fatalf("不该有问题：%v", res.Err())
	}
	out := string(res.Body)
	if !strings.Contains(out, "9007199254740993") {
		t.Fatalf("★ 未触碰的 seed 丢精度了：%s", out)
	}
	if !strings.Contains(out, `"temperature":1`) {
		t.Fatalf("规则该生效：%s", out)
	}
	// 另一条：只改别的字段时，0.50 这个字面量必须原样。
	res = ApplyParamOverrides(body, []ParamRule{{Op: ParamOpSet, Path: "model", Value: "y"}})
	if !strings.Contains(string(res.Body), "0.50") {
		t.Fatalf("★ 未触碰的 0.50 被改写成别的字面量：%s", res.Body)
	}
}

// TestParamOverrideConditions 条件（when）是"这条规则什么时候生效"。
func TestParamOverrideConditions(t *testing.T) {
	delOff := ParamRule{
		Op:   ParamOpDelete,
		Path: "reasoning_effort",
		When: []ParamCondition{{Path: "reasoning_effort", Mode: ParamCondIn, Value: []any{"off", "none"}}},
	}
	cases := []struct {
		name    string
		body    string
		rules   []ParamRule
		want    string
		changed bool
	}{
		{"in 命中则删除", `{"reasoning_effort":"off"}`, []ParamRule{delOff}, `{}`, true},
		{"in 未命中则保留", `{"reasoning_effort":"high"}`, []ParamRule{delOff}, `{"reasoning_effort":"high"}`, false},
		{
			// ★ 路径缺失时条件**不成立**：eq/ne/in 都不该把"缺失"当成零值去匹配，
			// 否则条件命中与否无法通过日志区分（规则看起来"生效了"，其实在别的请求上）。
			name:    "路径缺失时 in 不成立",
			body:    `{"model":"x"}`,
			rules:   []ParamRule{delOff},
			want:    `{"model":"x"}`,
			changed: false,
		},
		{
			name: "eq 命中",
			body: `{"stream":true}`,
			rules: []ParamRule{{
				Op: ParamOpSet, Path: "stream_options.include_usage", Value: true,
				When: []ParamCondition{{Path: "stream", Mode: ParamCondEq, Value: true}},
			}},
			want:    `{"stream":true,"stream_options":{"include_usage":true}}`,
			changed: true,
		},
		{
			name: "eq 未命中",
			body: `{"stream":false}`,
			rules: []ParamRule{{
				Op: ParamOpSet, Path: "stream_options.include_usage", Value: true,
				When: []ParamCondition{{Path: "stream", Mode: ParamCondEq, Value: true}},
			}},
			want:    `{"stream":false}`,
			changed: false,
		},
		{
			name: "ne 命中",
			body: `{"reasoning_effort":"auto"}`,
			rules: []ParamRule{{
				Op: ParamOpSet, Path: "service_tier", Value: "flex",
				When: []ParamCondition{{Path: "reasoning_effort", Mode: ParamCondNe, Value: "high"}},
			}},
			want:    `{"reasoning_effort":"auto","service_tier":"flex"}`,
			changed: true,
		},
		{
			name: "exists 命中（不关心值，null 也算存在）",
			body: `{"metadata":null}`,
			rules: []ParamRule{{
				Op: ParamOpSet, Path: "metadata.trace_id", Value: "t1",
				When: []ParamCondition{{Path: "metadata", Mode: ParamCondExists}},
			}},
			// metadata 是 null ⇒ 按"不存在"建对象（文件头：null 视为没有内容）
			want:    `{"metadata":{"trace_id":"t1"}}`,
			changed: true,
		},
		{
			name: "not_exists 命中",
			body: `{"model":"x"}`,
			rules: []ParamRule{{
				Op: ParamOpSet, Path: "temperature", Value: 1,
				When: []ParamCondition{{Path: "temperature", Mode: ParamCondNotExists}},
			}},
			want:    `{"model":"x","temperature":1}`,
			changed: true,
		},
		{
			name: "not_exists 未命中",
			body: `{"temperature":0.2}`,
			rules: []ParamRule{{
				Op: ParamOpSet, Path: "temperature", Value: 1,
				When: []ParamCondition{{Path: "temperature", Mode: ParamCondNotExists}},
			}},
			want:    `{"temperature":0.2}`,
			changed: false,
		},
		{
			// 多条条件是 AND：任何一条不成立，整条规则不生效。
			name: "多个条件是 AND",
			body: `{"stream":true,"reasoning_effort":"high"}`,
			rules: []ParamRule{{
				Op: ParamOpSet, Path: "stream_options.include_usage", Value: true,
				When: []ParamCondition{
					{Path: "stream", Mode: ParamCondEq, Value: true},
					{Path: "reasoning_effort", Mode: ParamCondEq, Value: "off"},
				},
			}},
			want:    `{"stream":true,"reasoning_effort":"high"}`,
			changed: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(c.body)
			res := ApplyParamOverrides(body, c.rules)
			if len(res.Problems) > 0 {
				t.Fatalf("不该有问题：%v", res.Err())
			}
			if res.Changed != c.changed {
				t.Fatalf("changed=%v，想要 %v（Applied=%v）", res.Changed, c.changed, res.Applied)
			}
			if !c.changed {
				paramTestAssertSameBytes(t, res.Body, body)
				return
			}
			paramTestAssertJSONEqual(t, res.Body, c.want)
		})
	}
}

// TestParamOverrideOrderMatters 顺序是语义：先 default 再 delete ⇒ 一定是空。
//
// # 为什么必须钉住（而不是"实现细节"）
//
// 如果规则表用 map 存，Go 的随机迭代顺序会让同一份配置**每次跑出不同结果**：
// 有时 temperature=1，有时没有 temperature。这种"偶发差异"排障成本极高
// （重启后不复现）。这条用例同时守住"切片 + 按序应用"这个决定。
func TestParamOverrideOrderMatters(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	defThenDel := []ParamRule{
		{Op: ParamOpDefault, Path: "temperature", Value: 1},
		{Op: ParamOpDelete, Path: "temperature"},
	}
	delThenDef := []ParamRule{
		{Op: ParamOpDelete, Path: "temperature"},
		{Op: ParamOpDefault, Path: "temperature", Value: 1},
	}
	paramTestAssertJSONEqual(t, ApplyParamOverrides(body, defThenDel).Body, `{"model":"x"}`)
	paramTestAssertJSONEqual(t, ApplyParamOverrides(body, delThenDef).Body, `{"model":"x","temperature":1}`)
}

// TestParamOverrideNonJSONBody 非 JSON 对象 body：规则不适用，原样返回 + 说明。
//
// # 为什么不是 panic 也不是静默
//
// body 可能是空（GET 式调用）、可能不是 JSON（客户端 bug / 二进制透传）。
// 这既不是"规则写错了"也不是"上游错了"，而是"这条请求不适用覆盖"。
// 报一个说明、把原 body 交出去 —— 上游会给出它自己的判断，
// 而我们不会因为一条配置把一批请求变成网关错误。
func TestParamOverrideNonJSONBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"空 body", ""},
		{"纯文本", "not json"},
		{"顶层是数组", `[1,2,3]`},
		{"顶层是字符串", `"x"`},
		{"顶层是数字", `42`},
		{"截断的 JSON", `{"model":"x"`},
	}
	rules := []ParamRule{{Op: ParamOpSet, Path: "temperature", Value: 1}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ApplyParamOverrides([]byte(c.body), rules)
			paramTestAssertSameBytes(t, res.Body, []byte(c.body))
			if res.Changed {
				t.Fatal("无法解析时不该判定为改动")
			}
			if c.body == "" {
				// 空 body 直接短路（没有可应用的东西），不制造"问题"噪音。
				if len(res.Problems) != 0 {
					t.Fatalf("空 body 不该报问题，得到 %v", res.Err())
				}
				return
			}
			if len(res.Problems) == 0 {
				t.Fatal("应当说明为什么规则没生效")
			}
			if res.Problems[0].Index != -1 {
				t.Fatalf("整体性问题用 index=-1，得到 %d", res.Problems[0].Index)
			}
		})
	}
}

// TestParamOverrideParseShorthand 简写形态：键 = 路径、值 = set 目标。
func TestParamOverrideParseShorthand(t *testing.T) {
	raw := map[string]any{
		"temperature":                  1,
		"stream_options.include_usage": true,
	}
	rules, problems := ParseParamOverrides(raw)
	if len(problems) != 0 {
		t.Fatalf("不该有问题：%v", problems)
	}
	if len(rules) != 2 {
		t.Fatalf("应当解析出 2 条规则，得到 %d", len(rules))
	}
	// map 无序 ⇒ 解析后必须按 key 排序，否则 Applied 日志每次抖动、测试无法断言。
	if rules[0].Path != "stream_options.include_usage" || rules[1].Path != "temperature" {
		t.Fatalf("规则应按 key 字典序排列，得到 %v / %v", rules[0].Path, rules[1].Path)
	}
	for _, r := range rules {
		if r.Op != ParamOpSet {
			t.Fatalf("简写形态只能表达 set，得到 %q", r.Op)
		}
	}
	// 端到端：解析结果直接能用。
	paramTestAssertJSONEqual(t,
		ApplyParamOverrides([]byte(`{"model":"x","temperature":0.2}`), rules).Body,
		`{"model":"x","temperature":1,"stream_options":{"include_usage":true}}`)
}

// TestParamOverrideParseShorthandBadKeys 简写形态里的非法 key：只丢坏规则。
func TestParamOverrideParseShorthandBadKeys(t *testing.T) {
	rules, problems := ParseParamOverrides(map[string]any{
		"a..b":        1,
		"temperature": 1,
	})
	if len(problems) != 1 {
		t.Fatalf("应当恰好一条问题，得到 %v", problems)
	}
	if problems[0].Index != 0 || !strings.Contains(problems[0].Reason, "为空") {
		t.Fatalf("问题应指向 a..b 并说明空段，得到 %+v", problems[0])
	}
	// ★ 好规则照常生效：一条写错的配置不该让整个渠道的覆盖失效。
	if len(rules) != 1 || rules[0].Path != "temperature" {
		t.Fatalf("合法规则必须保留，得到 %+v", rules)
	}
}

// TestParamOverrideParseOperations 正式形态：set / delete / default / when。
func TestParamOverrideParseOperations(t *testing.T) {
	config := `{
	  "operations": [
	    {"op":"default","path":"temperature","value":1},
	    {"op":"delete","path":"reasoning_effort",
	     "when":{"path":"reasoning_effort","mode":"in","value":["off","none"]}},
	    {"op":"set","path":"stream_options.include_usage","value":true}
	  ]
	}`
	var raw map[string]any
	if err := json.Unmarshal([]byte(config), &raw); err != nil {
		t.Fatalf("测试配置不是合法 JSON：%v", err)
	}
	rules, problems := ParseParamOverrides(raw)
	if len(problems) != 0 {
		t.Fatalf("不该有问题：%v", problems)
	}
	if len(rules) != 3 {
		t.Fatalf("应当解析出 3 条规则，得到 %d", len(rules))
	}
	// 顺序必须与配置里的书写顺序一致（顺序是语义）。
	if rules[0].Op != ParamOpDefault || rules[1].Op != ParamOpDelete || rules[2].Op != ParamOpSet {
		t.Fatalf("规则顺序必须与配置一致，得到 %v/%v/%v", rules[0].Op, rules[1].Op, rules[2].Op)
	}
	if len(rules[1].When) != 1 || rules[1].When[0].Mode != ParamCondIn {
		t.Fatalf("when 没有被解析成条件：%+v", rules[1].When)
	}

	// 客户端带了 off + 自己的 temperature：default 让路，delete 生效，set 生效。
	paramTestAssertJSONEqual(t,
		ApplyParamOverrides([]byte(`{"model":"x","temperature":0.2,"reasoning_effort":"off"}`), rules).Body,
		`{"model":"x","temperature":0.2,"stream_options":{"include_usage":true}}`)
	// 客户端带了 high：delete 不生效。
	paramTestAssertJSONEqual(t,
		ApplyParamOverrides([]byte(`{"model":"x","reasoning_effort":"high"}`), rules).Body,
		`{"model":"x","reasoning_effort":"high","temperature":1,"stream_options":{"include_usage":true}}`)
}

// TestParamOverrideParseInvalidRules 非法规则：逐条报问题，且**不进规则表**。
//
// # 为什么要这么细
//
// 规则是手写配置。写错的方式就那么几种，而每种都得给出"人能照着改"的说明：
// 少了 value、op 拼错、when 的 mode 拼错……报"invalid param override"
// 等于让人去猜。这里逐条断言 Reason 里有关键词。
func TestParamOverrideParseInvalidRules(t *testing.T) {
	cases := []struct {
		name string
		rule map[string]any
		want string // Reason 必须包含的关键词
	}{
		{"未知 op", map[string]any{"op": "move", "path": "a", "value": 1}, "未知的操作"},
		{"缺 op", map[string]any{"path": "a", "value": 1}, "未知的操作"},
		{"缺 path", map[string]any{"op": "set", "value": 1}, "路径为空"},
		{"双点路径", map[string]any{"op": "set", "path": "a..b", "value": 1}, "为空"},
		{"尾点路径", map[string]any{"op": "set", "path": "a.", "value": 1}, "为空"},
		{"set 缺 value", map[string]any{"op": "set", "path": "a"}, "缺少 value"},
		{"default 缺 value", map[string]any{"op": "default", "path": "a"}, "缺少 value"},
		{"when 的 mode 未知", map[string]any{
			"op": "set", "path": "a", "value": 1,
			"when": map[string]any{"path": "b", "mode": "regex", "value": "x"},
		}, "未知的比较方式"},
		{"when 的 in 不是数组", map[string]any{
			"op": "set", "path": "a", "value": 1,
			"when": map[string]any{"path": "b", "mode": "in", "value": "x"},
		}, "必须是数组"},
		{"when 的 eq 缺 value", map[string]any{
			"op": "set", "path": "a", "value": 1,
			"when": map[string]any{"path": "b", "mode": "eq"},
		}, "需要 value"},
		{"when 不是对象", map[string]any{
			"op": "set", "path": "a", "value": 1, "when": "b == 1",
		}, "条件必须是对象"},
		{"when 的 path 为空", map[string]any{
			"op": "set", "path": "a", "value": 1,
			"when": map[string]any{"path": "", "mode": "exists"},
		}, "路径为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rules, problems := ParseParamOverrides(map[string]any{
				"operations": []any{c.rule},
			})
			if len(problems) != 1 {
				t.Fatalf("应当报出 1 个问题，得到 %v", problems)
			}
			if !strings.Contains(problems[0].Reason, c.want) {
				t.Fatalf("问题说明应包含 %q，得到 %q", c.want, problems[0].Reason)
			}
			if len(rules) != 0 {
				t.Fatalf("坏规则不能进规则表，得到 %+v", rules)
			}
			if problems[0].Index != 0 {
				t.Fatalf("问题应指向第 1 条规则，得到 %d", problems[0].Index)
			}
		})
	}
}

// TestParamOverrideParseStructureErrors operations 的整体形状错误。
func TestParamOverrideParseStructureErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"operations 不是数组", map[string]any{"operations": "set a 1"}, "operations 必须是规则数组"},
		{"规则不是对象", map[string]any{"operations": []any{"set a 1"}}, "规则必须是对象"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, problems := ParseParamOverrides(c.raw)
			if len(problems) != 1 || !strings.Contains(problems[0].Reason, c.want) {
				t.Fatalf("应当报 %q，得到 %v", c.want, problems)
			}
		})
	}
	// nil / 空配置：不报问题、返回空表（调用方据此完全不碰 body）。
	for _, raw := range []map[string]any{nil, {}, {"operations": nil}} {
		rules, problems := ParseParamOverrides(raw)
		if len(rules) != 0 || len(problems) != 0 {
			t.Fatalf("空配置应当无规则无问题，得到 %v / %v", rules, problems)
		}
	}
}

// TestParamOverrideParseModeAlias 兼容 new-api 的字段名 "mode"（它的 ParamOperation 用它）。
//
// # 为什么值得这两行
//
// 照抄来的配置（文档、教程、别人家的备份）不必改写就能用 ——
// 而两种写法同时存在时必须明确谁优先：op 在前，mode 只是缺 op 时的别名。
func TestParamOverrideParseModeAlias(t *testing.T) {
	rules, problems := ParseParamOverrides(map[string]any{
		"operations": []any{map[string]any{"mode": "set", "path": "temperature", "value": 1}},
	})
	if len(problems) != 0 {
		t.Fatalf("不该有问题：%v", problems)
	}
	if len(rules) != 1 || rules[0].Op != ParamOpSet {
		t.Fatalf("mode 应当被当成 op 的别名，得到 %+v", rules)
	}
}

// TestParamOverrideDescribe Describe / Problem.String 的文案稳定性。
//
// # 为什么文案也要测
//
// 这两者会进**出站日志与前端**（"这次请求被改了什么"）。它们不是内部调试
// 字符串：换一种写法会让按日志 grep 的排障手册失效。钉住它们，改动就可见。
func TestParamOverrideDescribe(t *testing.T) {
	cases := []struct {
		name string
		rule ParamRule
		want string
	}{
		{"set", ParamRule{Op: ParamOpSet, Path: "temperature", Value: 1}, "set temperature = 1"},
		{"default", ParamRule{Op: ParamOpDefault, Path: "stream", Value: true}, "default stream = true"},
		{"delete 不带值", ParamRule{Op: ParamOpDelete, Path: "reasoning_effort"}, "delete reasoning_effort"},
		{"set null", ParamRule{Op: ParamOpSet, Path: "x", Value: nil}, "set x = null"},
		{"字符串带引号", ParamRule{Op: ParamOpSet, Path: "x", Value: "s"}, `set x = "s"`},
		{"未知 op 也要能打印", ParamRule{Op: ParamOp("move"), Path: "a"}, "move a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rule.Describe(); got != c.want {
				t.Fatalf("Describe()=%q，想要 %q", got, c.want)
			}
		})
	}

	if got := (ParamRuleProblem{Index: 2, Rule: "set a = 1", Reason: "路径为空"}).String(); got != "规则 #2（set a = 1）：路径为空" {
		t.Fatalf("Problem.String()=%q", got)
	}
	// index<0 且带标签：标签照出（"全部规则"比"整体"更能说明是什么没生效）。
	if got := (ParamRuleProblem{Index: -1, Rule: "全部规则", Reason: "r"}).String(); got != "全部规则：r" {
		t.Fatalf("整体性问题的文案不对：%q", got)
	}
	// index<0 且没标签：回落"整体"。
	if got := (ParamRuleProblem{Index: -1, Reason: "r"}).String(); got != "整体：r" {
		t.Fatalf("无标签的整体性文案不对：%q", got)
	}
}

// TestParamOverrideReasoningParity 现有 reasoning 归一化 == 一组内置覆盖规则。
//
// # 这条用例的意义（PLAN §4 风险表的兑现方式）
//
// 计划说"T6 落地后，reasoning_effort 归一化退化为 override 的一条内置规则"。
// 那句话能不能兑现，取决于本文件的接口**能不能表达**那条规则 ——
// 而不是取决于我们口头保证"差不多"。所以这里直接拿两个实现对同一批 body
// 做对照：语义必须逐例一致，否则集成时会出现"切过去之后某些取值行为变了"。
//
// 对照口径（语义，不是字节）：
//
//	off / none  ⇒ 该字段必须消失（两种实现都是删除）
//	low/high/max/缺失 ⇒ 客户端原样保留（normalize 返回 changed=false，
//	                    override 的 Changed 必须是 false）
func TestParamOverrideReasoningParity(t *testing.T) {
	rules := ReasoningOffOverrideRules()
	if len(rules) == 0 {
		t.Fatal("内置规则不能是空表")
	}

	values := []struct {
		value   string
		removed bool
	}{
		{"off", true},
		{"none", true},
		{"low", false},
		{"high", false},
		{"max", false},
		{"", false},
	}
	// 两个键名都要覆盖：本仓客户端两种写法都出现过。
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		for _, v := range values {
			name := key + "=" + v.value
			t.Run(name, func(t *testing.T) {
				body := []byte(`{"model":"x","` + key + `":"` + v.value + `"}`)
				if v.value == "" {
					// 空串形态：{"model":"x","reasoning_effort":""}
					body = []byte(`{"model":"x","` + key + `":""}`)
				}

				normalized, changed := normalizeReasoningEffortForUpstream(body)
				res := ApplyParamOverrides(body, rules)
				if len(res.Problems) > 0 {
					t.Fatalf("内置规则不该出错：%v", res.Err())
				}

				// ① 两种实现对"是否改动"的判断必须一致。
				if res.Changed != changed {
					t.Fatalf("改动判定不一致：override=%v normalize=%v", res.Changed, changed)
				}
				// ② 语义一致：字段在或不在。
				_, inNormalized := paramGetPath(mustParamObj(t, normalized), []string{key})
				_, inOverridden := paramGetPath(mustParamObj(t, res.Body), []string{key})
				if v.removed {
					if inNormalized || inOverridden {
						t.Fatalf("off/none 必须删除该字段（normalize 有=%v，override 有=%v）", inNormalized, inOverridden)
					}
					return
				}
				if !inNormalized || !inOverridden {
					t.Fatalf("非关闭取值必须保留（normalize 有=%v，override 有=%v）", inNormalized, inOverridden)
				}
			})
		}
	}

	// 没有该字段时两种实现都必须是"原样"（字节一致）。
	body := []byte(`{"model":"x","temperature":0.50}`)
	if _, changed := normalizeReasoningEffortForUpstream(body); changed {
		t.Fatal("前提不成立：无字段时 normalize 不该改动")
	}
	res := ApplyParamOverrides(body, rules)
	if res.Changed {
		t.Fatal("无字段时覆盖规则不该改动 body")
	}
	paramTestAssertSameBytes(t, res.Body, body)
}

// mustParamObj 解码测试用 body（失败即 Fatal）。
func mustParamObj(t *testing.T, body []byte) any {
	t.Helper()
	obj, err := paramDecodeObject(body)
	if err != nil {
		t.Fatalf("不是 json 对象：%v（%s）", err, body)
	}
	return obj
}

// TestParamOverrideAppliedExplainsChanges Applied 只记**真正改动了 body** 的规则。
//
// # 为什么要区分"配了"与"改动了"
//
// 出站日志的问题是"这次请求被改了什么"。一条没有生效的规则（条件不满足、
// 删除不存在的字段）如果也记进去，日志会变成配置的回显 ——
// 排障时得反过来推"这条到底生没生效"，那还不如不记。
func TestParamOverrideAppliedExplainsChanges(t *testing.T) {
	body := []byte(`{"reasoning_effort":"off","temperature":0.2}`)
	rules := []ParamRule{
		{Op: ParamOpDefault, Path: "temperature", Value: 1}, // 不生效（已存在）
		{Op: ParamOpDelete, Path: "reasoning_effort"},       // 生效
		{Op: ParamOpDelete, Path: "nope"},                   // 不生效（不存在）
		{Op: ParamOpSet, Path: "top_k", Value: 40},          // 生效
	}
	res := ApplyParamOverrides(body, rules)
	if len(res.Problems) > 0 {
		t.Fatalf("不该有问题：%v", res.Err())
	}
	want := []string{"delete reasoning_effort", "set top_k = 40"}
	if len(res.Applied) != len(want) {
		t.Fatalf("Applied=%v，想要 %v", res.Applied, want)
	}
	for i := range want {
		if res.Applied[i] != want[i] {
			t.Fatalf("Applied[%d]=%q，想要 %q（顺序必须是应用顺序）", i, res.Applied[i], want[i])
		}
	}
	paramTestAssertJSONEqual(t, res.Body, `{"temperature":0.2,"top_k":40}`)
}
