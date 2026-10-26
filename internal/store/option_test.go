// option_test.go options 表 + config.json 首次导入的门禁。
//
// # 为什么导入测试要构造**真的 config.json**而不是直接喂 map
//
// 因为这条路径上最容易错的不是 SQL，而是"把 JSON 读成键值"这段：
// 注释、嵌套对象、数组、null、带双斜杠的 URL、数字精度。这些全都只在
// **文件形态**下才出现。用 map 构造输入等于跳过了唯一的难点。
package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig 把一段配置写成临时文件，返回路径。
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时配置: %v", err)
	}
	return p
}

// TestPutAndLoadOptions 基本读写与覆盖语义。
func TestPutAndLoadOptions(t *testing.T) {
	openTempDB(t)

	if err := PutOption("site_name", "gw"); err != nil {
		t.Fatalf("PutOption: %v", err)
	}
	if err := PutOption("listen", "127.0.0.1:7863"); err != nil {
		t.Fatalf("PutOption: %v", err)
	}
	// 覆盖同一个键：必须改值而不是插第二行（主键是 key）。
	if err := PutOption("site_name", "gateway"); err != nil {
		t.Fatalf("PutOption 覆盖: %v", err)
	}

	got, err := LoadOptions()
	if err != nil {
		t.Fatalf("LoadOptions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应当恰好 2 个键，实际 %d（%v）", len(got), got)
	}
	if got["site_name"] != "gateway" {
		t.Fatalf("覆盖后应当是新值 gateway，实际 %q", got["site_name"])
	}
	if n := countRows(t, &Option{}); n != 2 {
		t.Fatalf("表里应当 2 行，实际 %d 行", n)
	}
}

// TestLoadOptionsReturnsCopy 返回值必须是副本，改它不影响库。
//
// 这一条看似吹毛求疵，但 LoadOptions 的返回值会被塞进模板、被上层就地补
// 默认值 —— 若它其实是缓存/共享 map，一次渲染就会把"读到的配置"改掉。
func TestLoadOptionsReturnsCopy(t *testing.T) {
	openTempDB(t)
	if err := PutOption("a", "1"); err != nil {
		t.Fatalf("PutOption: %v", err)
	}
	first, err := LoadOptions()
	if err != nil {
		t.Fatalf("LoadOptions: %v", err)
	}
	first["a"] = "tampered"
	first["b"] = "injected"

	second, err := LoadOptions()
	if err != nil {
		t.Fatalf("LoadOptions(2): %v", err)
	}
	if second["a"] != "1" || second["b"] != "" {
		t.Fatalf("改返回值污染了库：%v", second)
	}
}

// TestPutOptionRejectsBadKey 非法键在写库之前就被拒。
func TestPutOptionRejectsBadKey(t *testing.T) {
	openTempDB(t)
	for _, k := range []string{"", "has space", "has\ttab", "line\nbreak", strings.Repeat("k", optionMaxKeyLen+1)} {
		if err := PutOption(k, "v"); !errors.Is(err, ErrBadOption) {
			t.Errorf("键 %q 应当被拒（ErrBadOption），实际 %v", k, err)
		}
	}
	if n := countRows(t, &Option{}); n != 0 {
		t.Fatalf("被拒的键不该落库，实际 %d 行", n)
	}
}

// TestOptionExists 存在性与"值为空串"必须区分得开。
func TestOptionExists(t *testing.T) {
	openTempDB(t)
	if err := PutOption("notice", ""); err != nil {
		t.Fatalf("PutOption: %v", err)
	}
	ok, err := OptionExists("notice")
	if err != nil {
		t.Fatalf("OptionExists: %v", err)
	}
	if !ok {
		t.Fatal("写过的空串键必须报告存在 —— 否则导入会把它再写一遍并把值改掉")
	}
	ok, err = OptionExists("missing")
	if err != nil {
		t.Fatalf("OptionExists(missing): %v", err)
	}
	if ok {
		t.Fatal("没写过的键不该报告存在")
	}
}

// TestImportFromConfigJSON 验收 ⑤：首次导入写入，二次调用**不写**。
func TestImportFromConfigJSON(t *testing.T) {
	openTempDB(t)

	path := writeConfig(t, `{
  // 行注释：监听地址
  "listen": "127.0.0.1:7863",
  "api_key": "should-not-be-imported",
  "admin_token": "also-not",
  "pool": {
    "max_in_flight": 3,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m"
  },
  "upstream": {
    /* 块注释 */
    "timeout_seconds": 120
  },
  "monkeycode": {
    "proxy": "http://127.0.0.1:10808"
  },
  "schedule": {
    "activity_hours": [9, 21],
    "checkin_hours": null
  },
  "features": {
    "sanitize_blacklist_fingerprints": true
  }
}`)

	n, err := ImportFromConfigJSON(path)
	if err != nil {
		t.Fatalf("ImportFromConfigJSON: %v", err)
	}
	if n <= 0 {
		t.Fatalf("首次导入必须写入若干行，实际 %d", n)
	}

	opts, err := LoadOptions()
	if err != nil {
		t.Fatalf("LoadOptions: %v", err)
	}

	// 1) 嵌套键以点号路径落库，标量按原字面量存。
	want := map[string]string{
		"listen":                              "127.0.0.1:7863",
		"pool.max_in_flight":                  "3",
		"pool.breaker_threshold":              "3",
		"pool.breaker_cooldown":               "30m",
		"upstream.timeout_seconds":            "120",
		"schedule.activity_hours":             "[9,21]",
		"features.sanitize_blacklist_fingerprints": "true",
	}
	for k, v := range want {
		if got := opts[k]; got != v {
			t.Errorf("option %q = %q，want %q", k, got, v)
		}
	}

	// 2) 带双斜杠的 URL 必须完整（按行剔注释会把它切掉）。
	if got := opts["monkeycode.proxy"]; got != "http://127.0.0.1:10808" {
		t.Errorf("带 // 的 URL 被切坏了：%q", got)
	}

	// 3) null 不落库（"没配"不该变成一个值）。
	if _, ok := opts["schedule.checkin_hours"]; ok {
		t.Error("null 不该被导入")
	}

	// 4) 凭据与部署参数一律不导入（安全取舍，见 importSkipKeyNames）。
	for _, k := range []string{"api_key", "admin_token"} {
		if _, ok := opts[k]; ok {
			t.Errorf("凭据 %q 不该进 options 表（它会被渲染到设置页、被导出进备份）", k)
		}
	}

	before := countRows(t, &Option{})
	n2, err := ImportFromConfigJSON(path)
	if err != nil {
		t.Fatalf("二次 ImportFromConfigJSON: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("二次导入必须写入 0 行，实际 %d 行", n2)
	}
	if after := countRows(t, &Option{}); after != before {
		t.Fatalf("二次导入后行数从 %d 变成 %d", before, after)
	}
}

// TestImportKeepsEditedValue 界面上改过的值不能被下次导入冲回去。
//
// # 这条是 D3 的核心语义
//
// D3 把 config.json 降级为"首次导入 + 备份"，SQL 是唯一权威。若导入覆盖已有键，
// 那"在设置页改了一项、重启后变回配置文件里的值"就会成为一个永远修不掉的
// 报障 —— 而它看起来完全像是"保存按钮没生效"。
func TestImportKeepsEditedValue(t *testing.T) {
	openTempDB(t)
	path := writeConfig(t, `{"listen": "127.0.0.1:7863", "pool": {"max_in_flight": 3}}`)
	if _, err := ImportFromConfigJSON(path); err != nil {
		t.Fatalf("首次导入: %v", err)
	}

	// 模拟"用户在设置页把它改成 8"。
	if err := PutOption("pool.max_in_flight", "8"); err != nil {
		t.Fatalf("PutOption: %v", err)
	}

	if n, err := ImportFromConfigJSON(path); err != nil {
		t.Fatalf("二次导入: %v", err)
	} else if n != 0 {
		t.Fatalf("二次导入不该写任何行，实际 %d", n)
	}

	opts, err := LoadOptions()
	if err != nil {
		t.Fatalf("LoadOptions: %v", err)
	}
	if got := opts["pool.max_in_flight"]; got != "8" {
		t.Fatalf("导入把用户改过的值冲回去了：%q（want \"8\"）", got)
	}
}

// TestImportMissingFile 文件不存在 ⇒ (0, nil)，不是错误。
func TestImportMissingFile(t *testing.T) {
	openTempDB(t)
	n, err := ImportFromConfigJSON(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("文件不存在不该报错，实际 %v", err)
	}
	if n != 0 {
		t.Fatalf("文件不存在应当返回 0 行，实际 %d", n)
	}
}

// TestImportBadJSON 解析失败必须报错**且一行都不写**。
//
// # 为什么"一行都不写"要单独断言
//
// 导入是逐条 INSERT 的。若哪天有人把事务去掉，一份解析到一半失败的配置会留下
// 半套键 —— 于是"配置导入失败"变成了"配置只导了一半，而且看起来成功了"。
func TestImportBadJSON(t *testing.T) {
	openTempDB(t)
	path := writeConfig(t, `{"listen": "127.0.0.1:7863", oops}`)
	if _, err := ImportFromConfigJSON(path); err == nil {
		t.Fatal("坏 JSON 应当报错")
	}
	if n := countRows(t, &Option{}); n != 0 {
		t.Fatalf("解析失败不该写任何行，实际 %d 行", n)
	}
}

// TestStripJSONComments 注释剥离的边界（这些是真正会出事的形态）。
func TestStripJSONComments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "字符串里的双斜杠不动",
			in:   `{"proxy":"http://127.0.0.1:10808"}`,
			want: `{"proxy":"http://127.0.0.1:10808"}`,
		},
		{
			name: "行注释被删掉但换行保留",
			in: `{ // note
 "a": 1 }`,
			want: `{
 "a": 1 }`,
		},
		{
			name: "块注释被删掉",
			in: `{ /* a
b */ "a": 1 }`,
			want: `{ "a": 1 }`,
		},
		{
			name: "字符串里的转义引号不提前结束字符串",
			in:   `{"a":"he said "hi" // not a comment"}`,
			want: `{"a":"he said "hi" // not a comment"}`,
		},
		{
			name: "无注释时逐字节不变",
			in:   `{"a":1,"b":[1,2]}`,
			want: `{"a":1,"b":[1,2]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(stripJSONComments([]byte(tc.in))); got != tc.want {
				t.Fatalf(`stripJSONComments(%q)
  got:  %q
  want: %q`, tc.in, got, tc.want)
			}
		})
	}
}

// TestFlattenConfigJSON 摊平的边界：空数组、嵌套数组、数字精度。
func TestFlattenConfigJSON(t *testing.T) {
	got, err := flattenConfigJSON([]byte(`{
		"big": 9007199254740993,
		"empty": [],
		"nested": {"a": {"b": "c"}},
		"nil": null
	}`))
	if err != nil {
		t.Fatalf("flattenConfigJSON: %v", err)
	}
	if got["big"] != "9007199254740993" {
		t.Errorf("大整数被改了值：%q（UseNumber 缺失会变成 9007199254740992）", got["big"])
	}
	if got["empty"] != "[]" {
		t.Errorf("空数组应当保留成 %q，实际 %q", "[]", got["empty"])
	}
	if got["nested.a.b"] != "c" {
		t.Errorf("多层嵌套路径 = %q，want %q", got["nested.a.b"], "c")
	}
	if _, ok := got["nil"]; ok {
		t.Error("null 不该出现在摊平结果里")
	}
}

// TestOptionsClosedDatabase DB 未打开时返回 ErrNotOpen 而不是 panic。
func TestOptionsClosedDatabase(t *testing.T) {
	if err := Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := LoadOptions(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("LoadOptions 应当返回 ErrNotOpen，实际 %v", err)
	}
	if err := PutOption("a", "b"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("PutOption 应当返回 ErrNotOpen，实际 %v", err)
	}
	if _, err := ImportFromConfigJSON("whatever.json"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("ImportFromConfigJSON 应当返回 ErrNotOpen，实际 %v", err)
	}
}
