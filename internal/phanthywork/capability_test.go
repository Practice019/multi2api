// capability_test.go 导入 / 账号列 / 健康探活三块的断言。
//
// 对齐本轮"把 phanthywork 补齐成标准上游"的目标：CapImport + AccountColumnsExt + HealthProbeExt。
package phanthywork

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// ── 批量导入（CapImport + AccountImportExt）─────────────────────────────

// TestImportAcceptsThreeShapes 三种粘贴形态都要认（这是本上游的特殊之处）。
//
//	① 网关嵌套形（别的部署拷过来的文件内容）
//	② **官方客户端的**凭据文件（用户最可能的动作：把 .credentials.json 整段粘进来）
//	③ 本包扁平形（手写）
func TestImportAcceptsThreeShapes(t *testing.T) {
	p := NewProvider(Config{})
	a := newTestAuth("u-import", "rt-import")
	_, nested, err := a.MarshalAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	official := []byte(`{"claudeAiOauth":{"accessToken":"` + fakeJWT("u-official", 1794446024) +
		`","refreshToken":"rt-official","expiresAt":1794446024000,"scopes":["user:profile"]}}`)
	flat := []byte(`{"account_scope":"u-flat","oauth":{"access_token":"` + fakeJWT("u-flat", 1794446024) + `"}}`)

	cases := []struct {
		name string
		raw  string
		uid  string
	}{
		{"网关嵌套形", string(nested), "u-import"},
		{"官方客户端形", string(official), "u-official"},
		{"本包扁平形", string(flat), "u-flat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ierr := p.ImportCredentials(c.raw)
			if ierr != nil {
				t.Fatalf("导入失败: %v", ierr)
			}
			if len(got) != 1 {
				t.Fatalf("应当导入 1 份，实际 %d", len(got))
			}
			item := got[0]
			if item.UID != c.uid {
				t.Errorf("UID = %q, want %q", item.UID, c.uid)
			}
			if item.FileName == "" || len(item.Raw) == 0 {
				t.Errorf("FileName/Raw 不能为空（它们决定落盘）：%q / %d 字节", item.FileName, len(item.Raw))
			}
			if strings.ContainsAny(item.FileName, `/\\`) {
				t.Errorf("FileName 不能含路径分隔符（它会与 AuthDir 拼在一起）：%q", item.FileName)
			}
			// 产物必须能被回读（与页内登录产的凭证同形 ⇒ 下游不必区分来源）
			back, perr := ParseCredential(item.Raw)
			if perr != nil {
				t.Fatalf("导入产物回读失败: %v", perr)
			}
			if back.UID() != c.uid {
				t.Errorf("回读 UID = %q, want %q", back.UID(), c.uid)
			}
		})
	}
}

// TestImportSplitsMultipleItems 一次粘贴多份也要能导。
func TestImportSplitsMultipleItems(t *testing.T) {
	p := NewProvider(Config{})
	a1 := newTestAuth("u-1", "rt-1")
	a2 := newTestAuth("u-2", "rt-2")
	_, r1, _ := a1.MarshalAuthFile()
	_, r2, _ := a2.MarshalAuthFile()
	got, err := p.ImportCredentials(string(r1) + "\n" + string(r2))
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应当导入 2 份，实际 %d", len(got))
	}
	if got[0].UID == got[1].UID {
		t.Error("两份应当是两个不同的账号")
	}
}

// TestImportFailureNamesItemAndReason ★ 失败要能看出**是哪一份、为什么**。
//
// 契约要求"解析失败 → 返回带原因的错误"；本实现进一步把**序号与逐条原因**
// 都放进错误里（共享骨架的语义是"任一条坏即整批失败"，但"整批失败"本身
// 只说了一句"有一条坏了"，用户看不出是哪条）。
func TestImportFailureNamesItemAndReason(t *testing.T) {
	p := NewProvider(Config{})
	good := newTestAuth("u-good", "rt")
	_, rawGood, _ := good.MarshalAuthFile()
	pasted := string(rawGood) + "\n" + `{"account_scope":"u-bad"}`
	got, err := p.ImportCredentials(pasted)
	if err == nil {
		t.Fatal("有一条坏凭证时必须整批失败（契约：不能静默返回空切片）")
	}
	if got != nil {
		t.Errorf("失败时不该返回部分结果（那会让调用方落盘一半）：%+v", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "第 2 份") {
		t.Errorf("错误里必须指出是**哪一份**，实际: %s", msg)
	}
	if !strings.Contains(msg, "access_token") {
		t.Errorf("错误里必须带上**原因**，实际: %s", msg)
	}
	if !strings.Contains(msg, "1/2") {
		t.Errorf("错误里应当给出计数（1/2 份有问题），实际: %s", msg)
	}
}

// TestImportListsEveryBadItem 多份坏的时候要逐条列出（不是只报第一条）。
func TestImportListsEveryBadItem(t *testing.T) {
	p := NewProvider(Config{})
	good := newTestAuth("u-ok", "rt")
	_, rawGood, _ := good.MarshalAuthFile()
	// 三份都**能被切分**（各自是合法 JSON 对象），但其中两份解析不出凭证
	//（第 2 份缺 access_token；第 3 份只有无关字段）⇒ 走的是逐条诊断那条路。
	pasted := string(rawGood) + "\n" + `{"account_scope":"u-a"}` + "\n" + `{"nope":1}`
	_, err := p.ImportCredentials(pasted)
	if err == nil {
		t.Fatal("有坏条目时应当报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "2/3") {
		t.Errorf("应当说清 2/3 份有问题，实际: %s", msg)
	}
	if !strings.Contains(msg, "第 2 份") || !strings.Contains(msg, "第 3 份") {
		t.Errorf("两份坏条目都要被逐条列出，实际: %s", msg)
	}
}

// TestImportUnparseablePasteGetsReadableError 连"切成条目"都做不到时，错误要可读。
//
// 那时没有"第几份"可言（整段不是对象序列）—— 但错误必须说明这一点，
// 而不是把 JSON 词法错误原样丢给用户。
func TestImportUnparseablePasteGetsReadableError(t *testing.T) {
	p := NewProvider(Config{})
	_, err := p.ImportCredentials("this is not json at all")
	if err == nil {
		t.Fatal("非 JSON 输入应当报错")
	}
	if !strings.Contains(err.Error(), "切分") {
		t.Errorf("错误里应当说明「无法按 JSON 对象切分」，实际: %s", err)
	}
}

// TestImportRejectsEmpty 空输入要报错（不是"导入了 0 个"）。
func TestImportRejectsEmpty(t *testing.T) {
	p := NewProvider(Config{})
	for _, in := range []string{"", "   ", "\n\n"} {
		if _, err := p.ImportCredentials(in); err == nil {
			t.Errorf("空输入 %q 应当报错", in)
		}
	}
}

// ── 账号列（**不实现** AccountColumnsExt）────────────────────────────────

// TestAccountColumnsUsesDefaults ★ 本上游**不声明** AccountColumnsExt ⇒ 账号池用默认列。
//
// # 为什么（2026-11-13 用户原话：「phanthywork 2 个账号 · 6 项能力的**今日额度**列不需要」）
//
//	本包原先加了 `free_quota` 这一列（「今日额度」）。用户明确不要它 ⇒
//	**直接不声明 AccountColumnsExt**，而不是"声明了但返回默认列" ——
//	后者会留下一个"声称实现了扩展点、行为却与默认一致"的假声明（本仓"声明即承诺"纪律的反面）。
//
// ⚠ **只撤列**：`QuotaExt` 那三维（Remaining / Free / FreeKnown）**原样保留** ——
//   「刷新本上游额度」面板还在用它，且"今日已领 ⇒ Free=0 且 FreeKnown=true"那条三态纪律不许连带删掉。
func TestAccountColumnsUsesDefaults(t *testing.T) {
	p := NewProvider(Config{})
	if _, ok := gateway.ExtOf[gateway.AccountColumnsExt](gateway.Provider(p)); ok {
		t.Error("本上游声明了 AccountColumnsExt —— 用户要求去掉「今日额度」(free_quota) 列 ⇒ 应当**不声明**、走默认列")
	}
	// 默认列集本身必须没被动过（顺序也一样）：核心的回落就是它。
	got := gateway.EffectiveAccountColumns(gateway.Provider(p))
	def := gateway.DefaultAccountColumns()
	if strings.Join(got, ",") != strings.Join(def, ",") {
		t.Errorf("账号列应当**恰好**是默认列集（顺序也一样）:\n got %v\nwant %v", got, def)
	}
	if strings.Contains(strings.Join(got, ","), gateway.AccountColFreeQuota) {
		t.Error("默认列里不该出现 free_quota（「今日额度」是用户要求撤掉的那一列）")
	}
	// ⚠ 但额度那三维（QuotaExt）必须还在 —— 只撤列，不撤能力。
	if _, ok := gateway.ExtOf[gateway.QuotaExt](gateway.Provider(p)); !ok {
		t.Error("QuotaExt 必须保留（「刷新本上游额度」面板靠它）—— 本轮只撤账号列")
	}
}

// ── 健康探活（HealthProbeExt）───────────────────────────────────────────

// TestProbeHealthIsReadOnly ★ 探活必须只读、且只打一个便宜的端点。
//
// 契约：不得用完整对话；不得触发写操作。这里断言"只打了 GET /api/oauth/profile"。
func TestProbeHealthIsReadOnly(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	a := newTestAuth("u-probe", "rt")
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}
	if err := p.ProbeHealth(context.Background(), cred); err != nil {
		t.Fatalf("健康账号的探活应当成功: %v", err)
	}
	paths := fake.pathsSnapshot()
	if len(paths) != 1 || paths[0] != pathProfile {
		t.Errorf("探活只该打 %s（只读端点），实际 %v", pathProfile, paths)
	}
	// 更狠一点：注册/签到/token 这些**写操作**端点一个都不许被碰
	for _, write := range []string{pathRegister, pathDailyLoginClaim, pathOAuthToken} {
		if fake.pathCount(write) != 0 {
			t.Errorf("探活触发了写操作 %s（契约禁止）", write)
		}
	}
}

// TestProbeHealthSurfacesFailure 失败要如实返回 error（调用方只记日志、不惩罚）。
func TestProbeHealthSurfacesFailure(t *testing.T) {
	p := NewProvider(Config{})
	if err := p.ProbeHealth(context.Background(), gateway.Credential{Provider: providerID, UID: "x"}); err == nil {
		t.Error("没有凭证时应当返回错误（而不是 nil = 健康）")
	}
	// 上游 5xx：探活要报错，且**不**改任何状态
	mux := http.NewServeMux()
	mux.HandleFunc(pathProfile, func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, http.StatusInternalServerError, map[string]any{"error": "boom"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p2 := newFakeProvider(t, srv)
	a := newTestAuth("u-probe", "rt")
	if err := p2.ProbeHealth(context.Background(), gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}); err == nil {
		t.Error("上游 5xx 时探活应当返回错误")
	}
	if r, ok := p2.RefreshQuota(a.UID()); ok && r.HasData {
		t.Error("探活不该改变额度状态")
	}
}

// TestImportRawIsGatewayShape 导入产物是**网关嵌套形**（与页内登录同形）。
func TestImportRawIsGatewayShape(t *testing.T) {
	p := NewProvider(Config{})
	a := newTestAuth("u-shape", "rt")
	_, raw, _ := a.MarshalAuthFile()
	got, err := p.ImportCredentials(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(got[0].Raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["auth"]; !ok {
		t.Error("落盘形态必须是嵌套形（根键 auth）—— 下游会按同一形状读")
	}
	if got[0].FileName != FileName(a) {
		t.Errorf("FileName = %q, want %q（文件名由本包的 FileName 决定）", got[0].FileName, FileName(a))
	}
}