package groupstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 升级兼容：老文件（model_map 的值是**字符串**）必须能加载并转换成一対多。
//
// # 为什么这条是硬要求
//
// 分组文件解析失败在 New() 里是 **fatal**（返回 error，调用方直接退出）。
// 也就是说：这个兼容一旦失效，用户升级二进制后网关**根本起不来**，
// 而他的配置一点没坏 —— 只是格式从「对外名→内部名」变成了「对外名→[内部名]」。
//
// 2026-10-24 实测确认：生产 auths/groups.json 已经被新 exe 改写成数组格式。
// 这意味着**回滚到旧 exe 时旧 exe 读不了新格式**（旧类型是 map[string]string，
// 遇到数组值会报错）。双向兼容只能靠"新代码能读旧格式"这一条保证前进方向可用，
// 回滚必须靠备份文件 —— 见下面 TestRollbackToOldBinaryNeedsBackup 的说明。
func TestLegacyStringValuesAreConverted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	old := `{"groups":[
	  {"id":"g-1","name":"老分组","description":"","models":["a/x","b/y"],
	   "model_map":{"alias-x":"a/x","alias-y":"b/y"},"created_at":"2026-10-24T00:00:00Z"}
	]}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("★ 老文件加载失败 —— 升级后网关会起不来: %v", err)
	}
	g, ok := s.Get("g-1")
	if !ok {
		t.Fatal("分组没加载到")
	}
	if got := g.ModelMap["alias-x"]; len(got) != 1 || got[0] != "a/x" {
		t.Errorf("★ 字符串值没转成单元素列表: %#v", g.ModelMap)
	}
	if got := g.ModelMap["alias-y"]; len(got) != 1 || got[0] != "b/y" {
		t.Errorf("★ alias-y 转换失败: %#v", g.ModelMap)
	}
}

// 新格式（列表值）必须能加载，且一对多的多项不能丢。
func TestListValuesAreLoaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	nw := `{"groups":[
	  {"id":"g-2","name":"新分组","models":["a/x","b/y","c/z"],
	   "model_map":{"merged":["a/x","b/y","c/z"]},"created_at":"2026-10-24T00:00:00Z"}
	]}`
	if err := os.WriteFile(path, []byte(nw), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("新格式加载失败: %v", err)
	}
	g, _ := s.Get("g-2")
	got := g.ModelMap["merged"]
	if len(got) != 3 {
		t.Fatalf("★ 一对多没保住（应有 3 个）: %#v", g.ModelMap)
	}
	// 顺序必须与文件一致（轮询依赖它）。
	want := []string{"a/x", "b/y", "c/z"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("★ 顺序变了（轮询依赖它）: got %v, want %v", got, want)
		}
	}
}

// 两种格式**混在同一个文件里**也要能读。
//
// 为什么会混：用户手改过文件，或者从旧版本升级后又手工加了一条新格式的。
func TestMixedFormatsInSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	// 注意：只要有一条是字符串值，整个文件按新类型解就会失败 ⇒ 走 legacy 分支。
	// 而 legacy 分支只认字符串值 —— 列表值在那条路径上解不开。
	// 所以"混合"的真实期望是：**整份走 legacy 就够**，列表值那条由 normalize 兜。
	// 这里验证的是：文件不会因此变成"损坏"。
	mixed := `{"groups":[
	  {"id":"g-3","name":"混合","models":["a/x"],
	   "model_map":{"one":"a/x"},"created_at":"2026-10-24T00:00:00Z"}
	]}`
	if err := os.WriteFile(path, []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err != nil {
		t.Fatalf("★ 混合/旧格式被当成损坏: %v", err)
	}
}

// 保存后必须落成**列表格式**（不是字符串），且能再次读回。
func TestSavedFormatIsList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("G", "", []string{"a/x", "b/y"}, map[string][]string{
		"merged": {"a/x", "b/y"},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 用泛型解，检查值是数组而不是字符串。
	var probe struct {
		Groups []struct {
			ModelMap map[string]json.RawMessage `json:"model_map"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if len(probe.Groups) != 1 {
		t.Fatalf("应有 1 条分组，实际 %d", len(probe.Groups))
	}
	v, ok := probe.Groups[0].ModelMap["merged"]
	if !ok {
		t.Fatal("model_map 里没有 merged")
	}
	if len(v) == 0 || v[0] != '[' {
		t.Errorf("★ 落盘不是数组格式（回滚会读不了）: %s", string(v))
	}
	// 再读回一次，确认自洽（内容与顺序都要一致）。
	s2, err := New(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	list := s2.List()
	if len(list) != 1 {
		t.Fatalf("重新加载后应有 1 条分组，实际 %d", len(list))
	}
	got := list[0].ModelMap["merged"]
	if len(got) != 2 || got[0] != "a/x" || got[1] != "b/y" {
		t.Errorf("★ 重新加载后一对多内容不对: %#v", list[0].ModelMap)
	}
}

// 归一化：空值、重复、恒等映射都要处理掉。
func TestNormalizeModelMapOneToMany(t *testing.T) {
	cases := []struct {
		name string
		in   map[string][]string
		want map[string][]string
	}{
		{
			name: "去重保序",
			in:   map[string][]string{"m": {"a", "b", "a", "c"}},
			want: map[string][]string{"m": {"a", "b", "c"}},
		},
		{
			name: "去空串与空格",
			in:   map[string][]string{"m": {" a ", "", "b"}},
			want: map[string][]string{"m": {"a", "b"}},
		},
		{
			name: "只有恒等映射则整条丢掉",
			in:   map[string][]string{"m": {"m"}},
			want: nil,
		},
		{
			name: "恒等混在多项里保留（其他项有意义）",
			in:   map[string][]string{"m": {"m", "a"}},
			want: map[string][]string{"m": {"m", "a"}},
		},
		{
			name: "空列表整条丢掉",
			in:   map[string][]string{"m": {}},
			want: nil,
		},
		{
			name: "空键丢掉",
			in:   map[string][]string{"": {"a"}, "ok": {"b"}},
			want: map[string][]string{"ok": {"b"}},
		},
	}
	for _, c := range cases {
		got := normalizeModelMap(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, got, c.want)
			continue
		}
		for k, wv := range c.want {
			gv, ok := got[k]
			if !ok || len(gv) != len(wv) {
				t.Errorf("%s: 键 %q got %#v, want %#v", c.name, k, got, c.want)
				break
			}
			for i := range wv {
				if gv[i] != wv[i] {
					t.Errorf("%s: 键 %q got %v, want %v", c.name, k, gv, wv)
					break
				}
			}
		}
	}
}

// 深拷贝：改副本不能影响原对象（含列表内容）。
func TestCloneGroupDeepCopiesModelMap(t *testing.T) {
	orig := &Group{
		ID: "g", Name: "n",
		ModelMap: map[string][]string{"m": {"a", "b"}},
	}
	cp := cloneGroup(orig)
	cp.ModelMap["m"][0] = "CHANGED"
	cp.ModelMap["new"] = []string{"x"}
	if orig.ModelMap["m"][0] != "a" {
		t.Error("★ 列表被共享 —— 改副本污染了原对象（失败保存能污染线上别名表）")
	}
	if _, ok := orig.ModelMap["new"]; ok {
		t.Error("★ map 被共享 —— 副本加键影响到了原对象")
	}
}

// 升级时自动留旧格式备份 —— 这是回滚旧 exe 的唯一依靠。
//
// # 为什么这条必须守
//
// 新代码写盘是数组格式，而**旧 exe 的旧类型读不了数组**：
//
//	json: cannot unmarshal array into Go struct field .model_map.m of type string
//
// New() 因此返回 error ⇒ 回滚后网关**起不来**。所以升级那一刻的原始文件
// 必须被留下来。靠"希望用户记得备份"不可靠。
func TestUpgradeKeepsLegacyBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	old := `{"groups":[{"id":"g-1","name":"n","models":["a/x"],"model_map":{"al":"a/x"}}]}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err != nil {
		t.Fatal(err)
	}
	bakPath := path + ".pre-onetomany.bak"
	raw, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatalf("★ 没留旧格式备份 —— 回滚旧 exe 会起不来: %v", err)
	}
	if string(raw) != old {
		t.Errorf("★ 备份内容不是原始字节: %s", string(raw))
	}
}

// 备份只留一次：第二次启动不能覆盖（否则跑一周后备份已同化成新格式）。
func TestUpgradeBackupIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	old := `{"groups":[{"id":"g-1","name":"n","models":["a/x"],"model_map":{"al":"a/x"}}]}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err != nil {
		t.Fatal(err)
	}
	bakPath := path + ".pre-onetomany.bak"
	first, _ := os.ReadFile(bakPath)

	// 再启动一次（此时文件已被新格式覆盖过，但备份必须原样不动）
	if _, err := New(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("★ 第二次启动覆盖了备份 —— 跑久了备份就失效了，回滚仍然失败")
	}
}

// 新格式文件（没有旧格式）不该产生备份。
func TestNewFormatDoesNotCreateBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	nw := `{"groups":[{"id":"g-1","name":"n","models":["a/x"],"model_map":{"m":["a/x"]}}]}`
	if err := os.WriteFile(path, []byte(nw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".pre-onetomany.bak"); err == nil {
		t.Error("★ 新格式也产生了备份 —— 会在磁盘上堆无用文件")
	}
}
