// jsonc_test.go JSONC（带注释的 config）的守卫。
//
// # 为什么要支持注释（用户原话）
//
//	「我发现这个 Config 配置有点太丑了，不知道怎么回事啊，太乱了我只能说。」
//
// 我给他重写一份"干净配置"时，价值的一半来自**中文注释**（说明每一项是干什么的）。
// 而标准库 `encoding/json` 不认注释 —— 不支持就会变成"注释写了但启动失败"。
//
// # 最容易写错的一条：字符串里的 `//` 不是注释
//
// 配置文件里有 URL（`"https://..."`、`"http://127.0.0.1:7863"`）。
// 只做正则替换的话，`"https://x"` 里的 `//` 会被当成行注释 ——
// 于是配置**从那处起被截掉**，症状是"写了个 URL 就解析失败"，
// 而排查时几乎不会想到"注释剥离"。
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestStripJSONComments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 剥离后的**语义**（比较时统一去掉空白）
		why  string
	}{
		{
			name: "行注释",
			in:   "{\n  \"a\": 1 // 说明\n}",
			want: `{"a":1}`,
			why:  "最常见的形态",
		},
		{
			name: "块注释",
			in:   "{\n  /* 多行\n     说明 */\n  \"a\": 1\n}",
			want: `{"a":1}`,
			why:  "跨行块注释",
		},
		{
			name: "字符串里的 // 不是注释",
			in:   `{"url": "https://example.com/x"}`,
			want: `{"url":"https://example.com/x"}`,
			why:  "⚠ 这是最容易写错的一条：URL 被截掉会让配置莫名其妙地解析失败",
		},
		{
			name: "字符串里的 /* 不是注释",
			in:   `{"a": "x/*y*/z"}`,
			want: `{"a":"x/*y*/z"}`,
			why:  "同上",
		},
		{
			name: "字符串里的转义引号",
			in:   `{"a": "he said \"hi\" // not a comment"}`,
			want: `{"a":"he said \"hi\" // not a comment"}`,
			why:  "转义引号不能提前结束字符串，否则后面的 // 会被误判",
		},
		{
			name: "注释里的引号不开启字符串",
			in:   "{\n  // 这是一个 \" 引号\n  \"a\": 1\n}",
			want: `{"a":1}`,
			why:  "注释里的引号若被当成字符串起始，后面全部会被吃掉",
		},
		{
			name: "注释里的键名不是键",
			in:   "{\n  // \"api_key\": \"fake\"\n  \"a\": 1\n}",
			want: `{"a":1}`,
			why:  "关系到轮换钥匙那处替换会不会改错地方",
		},
		{
			name: "没有注释",
			in:   `{"a":1}`,
			want: `{"a":1}`,
			why:  "原样（unmarshalJSONC 会走快路径）",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripJSONComments([]byte(c.in))
			// 语义比较：去掉所有空白再比（剥离只保证"其余字符不变"，
			// 不保证空白形态 —— 它把注释替成了空格）。
			gotSem := strings.Join(strings.Fields(string(got)), "")
			wantSem := strings.Join(strings.Fields(c.want), "")
			if gotSem != wantSem {
				t.Errorf("stripJSONComments 结果不对：\n  得到 %q\n  期望 %q\n  —— %s",
					gotSem, wantSem, c.why)
			}
			// 剥离后必须是合法 JSON。
			var v any
			if err := json.Unmarshal(got, &v); err != nil {
				t.Errorf("剥离后仍不是合法 JSON: %v", err)
			}
		})
	}
}

// ⚠ 剥离必须**等长**（注释位置换成空格），否则错误行号会整体前移 ——
// 用户按报的坐标去文件里找，找到的是别的东西，比不给坐标更糟。
func TestStripJSONCommentsPreservesOffsets(t *testing.T) {
	in := []byte("{\n  // 注释\n  \"a\": 1\n}")
	got := stripJSONComments(in)
	if len(got) != len(in) {
		t.Fatalf("长度必须不变（等长才能报对行号）：%d → %d", len(in), len(got))
	}
	// 换行符数量与位置必须一致。
	if bytes.Count(got, []byte("\n")) != bytes.Count(in, []byte("\n")) {
		t.Error("换行符数量变了 —— 跨行块注释后面的报错行号会差行数")
	}
	for i, c := range in {
		if c == '\n' && got[i] != '\n' {
			t.Errorf("第 %d 字节的换行被吃掉了", i)
		}
	}
}

// unmarshalJSONC 必须能解析带注释的配置，且不带注释时走快路径。
func TestUnmarshalJSONC(t *testing.T) {
	type s struct {
		A int    `json:"a"`
		L string `json:"listen"`
	}
	withComment := []byte("{\n  // 监听地址\n  \"a\": 1,\n  \"listen\": \"http://127.0.0.1:7863\"\n}")
	var v s
	if err := unmarshalJSONC(withComment, &v); err != nil {
		t.Fatalf("带注释的配置应当能解析: %v", err)
	}
	if v.A != 1 || v.L != "http://127.0.0.1:7863" {
		t.Errorf("解析结果不对: %+v —— URL 里的 // 被误当成注释了吗？", v)
	}

	plain := []byte(`{"a":2}`)
	var v2 s
	if err := unmarshalJSONC(plain, &v2); err != nil || v2.A != 2 {
		t.Errorf("不带注释的配置解析失败: %v / %+v", err, v2)
	}

	// 剥离后仍非法 ⇒ 报错里要提示"注释是被支持的"，否则用户会以为
	// "有注释才失败"（实际是别的地方写错了）。
	bad := []byte("{\n  // 注释\n  \"a\": ,\n}")
	var v3 s
	err := unmarshalJSONC(bad, &v3)
	if err == nil {
		t.Fatal("非法 JSON 应当报错")
	}
	if !strings.Contains(err.Error(), "注释") {
		t.Errorf("报错里应当说明「注释是支持的、问题在别处」，实际: %v", err)
	}
}
