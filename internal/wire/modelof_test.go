package wire

import "testing"

// TestModelOfReadsTopLevelField 读顶层 model 字段。
func TestModelOfReadsTopLevelField(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"普通", `{"model":"glm-5.2","messages":[]}`, "glm-5.2"},
		{"model 在后", `{"stream":true,"model":"gpt-6","messages":[]}`, "gpt-6"},
		{"只有 model", `{"model":"hy3"}`, "hy3"},
		{"model 非字符串", `{"model":123}`, ""},
		{"model 为 null", `{"model":null}`, ""},
		{"缺 model", `{"messages":[]}`, ""},
		{"空体", ``, ""},
		{"非法 JSON", `{not json`, ""},
		{"顶层是数组", `[{"model":"x"}]`, ""},
		{"顶层是标量", `"model"`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ModelOf([]byte(c.body)); got != c.want {
				t.Errorf("ModelOf(%q) = %q，want %q", c.body, got, c.want)
			}
		})
	}
}

// TestModelOfIgnoresModelInsideMessages **不能**把消息正文里的 model 当顶层。
//
// # 这条是一个真实风险
//
// 最省事的实现是 `bytes.Contains(body, []byte("\"model\""))` + 手工截串。
// 它会在这个输入上给出错误答案：用户问"model 字段是什么意思"，
// 正文里出现 `"model"`，而真正的顶层 model 在它**之后**。
//
// 流式解析（json.Decoder 只读顶层键）天然不受影响 —— 本用例把那个差别钉住。
func TestModelOfIgnoresModelInsideMessages(t *testing.T) {
	// 正文里先出现 "model"，真正的顶层 model 在后面
	body := `{"messages":[{"role":"user","content":"what does \"model\" mean?"}],"model":"glm-5.2"}`
	if got := ModelOf([]byte(body)); got != "glm-5.2" {
		t.Errorf("ModelOf = %q，want glm-5.2 —— "+
			"字符串搜索会把正文里的 \"model\" 当顶层（那是错的）", got)
	}

	// 反向：正文里有 model，而顶层**没有** → 必须返回空串
	body2 := `{"messages":[{"role":"user","content":"the \"model\" field"}]}`
	if got := ModelOf([]byte(body2)); got != "" {
		t.Errorf("ModelOf = %q，want 空串 —— "+
			"顶层没有 model 时不该从正文里捡一个", got)
	}
}

// TestModelOfSkipsNestedContainers 嵌套容器要正确跳过（不误把内层的 model 当顶层）。
func TestModelOfSkipsNestedContainers(t *testing.T) {
	body := `{"a":{"model":"inner"},"b":[{"model":"inArray"}],"model":"outer"}`
	if got := ModelOf([]byte(body)); got != "outer" {
		t.Errorf("ModelOf = %q，want outer —— "+
			"嵌套对象/数组里的 model 不是顶层字段", got)
	}
}

// TestModelOfHandlesLargeBody 大请求体（含 base64 图片）不 panic 且能读到 model。
//
// 判据是"不 panic 且结果正确"：流式解析的价值在大体上体现为**不做无用功**，
// 而这里只能验证行为正确（性能由实现方式保证，不在此断言）。
func TestModelOfHandlesLargeBody(t *testing.T) {
	big := make([]byte, 200_000)
	for i := range big {
		big[i] = 'A'
	}
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"` +
		string(big) + `"}]}`
	if got := ModelOf([]byte(body)); got != "glm-5.2" {
		t.Errorf("ModelOf(大体) = %q，want glm-5.2", got)
	}
}
