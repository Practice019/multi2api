// models_test.go 模型白名单在**存储层**的守卫。
//
// # 用户的需求
//
//	「API管理里面，我们设置那个API，我觉得需要设置这个API可以用哪些模型？」
//
// # 本文件只测"存"，不测"怎么比"
//
// 「白名单里的名字怎么与请求的模型名比对」**不在本包** —— 那一份实现住在
// internal/server（它认识 gateway，能用与选路同一个 gateway.SplitModel）。
//
// 为什么不能放这里（架构守卫实测抓到过）：
//
//	import 了 gateway 的包会被 discoverUpstreams 当成**上游实现**，
//	于是核心（admin / server）依赖本包 = 「核心依赖上游」= 判据 1 违规。
//
// 而"在本包再写一份近似的比对"更糟：同一个判据两份实现，在"模型名本身
// 含 '/'"时必然分叉 —— 分叉的表现是"白名单认的模型"与"请求会路由到的
// 模型"不是同一个东西，于是偶尔 403 一个看起来完全合法的请求。
//
// 所以分工是：**存储留在本包（零内部依赖），解释留在 server**。
package apikey

import "testing"

// mustKey 直接取内部记录（同包，测试用）。
func mustKey(t *testing.T, s *Store, id string) *Key {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		t.Fatalf("key %s 不存在", id)
	}
	return k
}

// 白名单在写入时被规整：去空白、丢空项、去重（保序）。
//
// 这条防一个很具体的现场：用户在输入框里多打一个回车 ⇒ `["glm-5.2",""]`
// ⇒ len>0 让这把 key 变成"受限制"，而那个空串永远匹配不上任何模型。
// 后果是"我明明留空了却全都不许用"，极难往"多打了一个回车"上想。
func TestCreateNormalizesModels(t *testing.T) {
	s := newTestStore(t)
	k, err := s.Create("app", 0, 0, []string{" glm-5.2 ", "", "glm-5.2", "  ", "deepseek-v4-flash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Models) != 2 {
		t.Fatalf("应规整成 2 个（去空白/丢空项/去重），实际 %#v —— "+
			"留下空串会让白名单「看起来设了」而实际拒掉一切", k.Models)
	}
	if k.Models[0] != "glm-5.2" || k.Models[1] != "deepseek-v4-flash" {
		t.Errorf("顺序应保持输入序，实际 %#v", k.Models)
	}
}

// 只输入空白的白名单 ⇒ 归成**不限制**（len==0 就是那个状态的表达）。
func TestCreateBlankModelsMeansUnrestricted(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, []string{"", "   "})
	if len(k.Models) != 0 {
		t.Errorf("全是空白的输入应当归成「不限制」（len==0），实际 %#v", k.Models)
	}
	// 空切片 / nil 是"不限制"的**规范表达**（出口层据此放行一切）。
	if k.Models == nil {
		// nil 也可以；出口层判据是 len==0，两种都行。
		// 这条断言存在的意义是把这个约定写下来，别改成"空 = 一个都不许"。
		_ = k.Models
	}
}

// Create 不传白名单 ⇒ 不限制（既有调用方的形态，必须向后兼容）。
func TestCreateWithoutModelsIsUnrestricted(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, nil)
	if len(k.Models) != 0 {
		t.Errorf("不传白名单应当是「不限制」，实际 %#v", k.Models)
	}
}

// Update 的**逐字段三态**：nil = 不改，0 = 设为不限，[] = 清空白名单。
//
// # 为什么这条最重要
//
// 前端有两个独立动作：「改配额」只带 limit/rpm，「改模型」只带 models。
// 若把"缺失"当成"设为 0 / 清空"，用户没动过的字段会被悄悄改掉 ——
// 改一次模型把额度清零，或改一次配额把模型限制解开。两者界面上都看不出来。
func TestUpdateIsTriStatePerField(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 100, 5, []string{"glm-5.2"})

	// ① 只改模型：limit/rpm 必须**原样不动**。
	if err := s.Update(k.ID, nil, nil, []string{"glm-5.2", "glm-5.1"}); err != nil {
		t.Fatal(err)
	}
	got := mustKey(t, s, k.ID)
	if got.Limit != 100 || got.RPM != 5 {
		t.Errorf("只改模型时不得动额度/限速，实际 limit=%d rpm=%d", got.Limit, got.RPM)
	}
	if len(got.Models) != 2 {
		t.Errorf("模型应更新为 2 个，实际 %#v", got.Models)
	}

	// ② 只改配额：模型白名单必须**原样不动**。
	newLimit := int64(200)
	newRPM := 9
	if err := s.Update(k.ID, &newLimit, &newRPM, nil); err != nil {
		t.Fatal(err)
	}
	got = mustKey(t, s, k.ID)
	if len(got.Models) != 2 {
		t.Errorf("只改配额时不得清空模型白名单（否则「改一次额度就把限制静默解开」），"+
			"实际 %#v", got.Models)
	}
	if got.Limit != 200 || got.RPM != 9 {
		t.Errorf("配额应更新，实际 limit=%d rpm=%d", got.Limit, got.RPM)
	}

	// ③ 显式 [] ⇒ 清空白名单 = 恢复不限制。
	if err := s.Update(k.ID, nil, nil, []string{}); err != nil {
		t.Fatal(err)
	}
	got = mustKey(t, s, k.ID)
	if len(got.Models) != 0 {
		t.Errorf("显式 [] 应当清空白名单，实际 %#v", got.Models)
	}

	// ④ 指向 0 ⇒ 真的设为不限（与"缺失"不同）。
	z, zr := int64(0), 0
	if err := s.Update(k.ID, &z, &zr, nil); err != nil {
		t.Fatal(err)
	}
	got = mustKey(t, s, k.ID)
	if got.Limit != 0 || got.RPM != 0 {
		t.Errorf("显式给 0 应当设为不限，实际 limit=%d rpm=%d", got.Limit, got.RPM)
	}
}

// ModelsOf 返回**副本**：调用方 append 不该改到存储内部。
func TestModelsOfReturnsCopy(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, []string{"glm-5.2"})

	got, ok := s.ModelsOf(k.ID)
	if !ok || len(got) != 1 {
		t.Fatalf("应查到 1 个，实际 (%v, %v)", got, ok)
	}
	got[0] = "被改掉了"
	again, _ := s.ModelsOf(k.ID)
	if again[0] != "glm-5.2" {
		t.Errorf("ModelsOf 必须返回副本，实际存储被改成了 %q", again[0])
	}
	if _, ok := s.ModelsOf("sk-nope"); ok {
		t.Error("不存在的 key 应报 ok=false")
	}
}

// 白名单随落盘持久化（重启后仍在）。
func TestModelsPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir + "/apikeys.json")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := s.Create("app", 0, 0, []string{"glm-5.2", "glm-5.1"})

	again, err := New(dir + "/apikeys.json")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.ModelsOf(k.ID)
	if !ok || len(got) != 2 {
		t.Fatalf("重启后白名单应还在，实际 (%v, %v)", got, ok)
	}
}

// 列表视图带上白名单（前端那列读它）。
func TestListIncludesModels(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Create("app", 0, 0, []string{"glm-5.2"})
	_, _ = s.Create("other", 0, 0, nil)

	views := s.List()
	if len(views) != 2 {
		t.Fatalf("应有 2 把，实际 %d", len(views))
	}
	byName := map[string]View{}
	for _, v := range views {
		byName[v.Name] = v
	}
	if len(byName["app"].Models) != 1 {
		t.Errorf("受限那把应当带 1 个模型，实际 %#v", byName["app"].Models)
	}
	if len(byName["other"].Models) != 0 {
		t.Errorf("不限制那把应当是空（前端显示「全部」），实际 %#v", byName["other"].Models)
	}
}
