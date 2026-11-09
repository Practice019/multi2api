package codearts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// T-297：去重语义 + 「localAccounts() 出口已去重」这两件事的钉子。
//
// 背景（真跑实证）：同一账号重复登录会留下多份凭证（文件名按 AK 命名）。
// 遍历凭证目录时若不过滤，过期那份会让上游返回 **403** APIGW.0301
// （过期 STS 是 403 而**不是** 401），界面看起来像签名 bug，其实是脏数据。
// ─────────────────────────────────────────────────────────────────────────────

func authFixture(uid, ak string, exp int64) *Auth {
	return &Auth{UID: uid, AccessKey: ak, ExpiresAt: exp}
}

// TestDedupeAuthByUID 钉住纯函数语义（含"完全保序"这条 T-297 新增的性质）。
func TestDedupeAuthByUID(t *testing.T) {
	now := time.Now().Unix()
	old := authFixture("u1", "AK_OLD", now-3600*24*18) // 过期 18 天
	fresh := authFixture("u1", "AK_NEW", now+3600)
	other := authFixture("u2", "AK_U2", now+1800)
	noUID := authFixture("", "AK_NOUID", now+600)

	// ① 同 UID 留到期最晚的那份；输入顺序不影响结果
	for _, in := range [][]*Auth{{old, fresh, other}, {fresh, old, other}, {other, old, fresh}} {
		out := dedupeAuthByUID(in)
		if len(out) != 2 {
			t.Fatalf("去重后应有 2 份，得到 %d: %+v", len(out), out)
		}
		byUID := map[string]*Auth{}
		for _, a := range out {
			byUID[a.UID] = a
		}
		if got := byUID["u1"]; got == nil || got.AccessKey != "AK_NEW" {
			t.Errorf("u1 应保留到期最晚的 AK_NEW，得到 %+v", got)
		}
		if got := byUID["u2"]; got == nil || got.AccessKey != "AK_U2" {
			t.Errorf("u2 应保留，得到 %+v", got)
		}
	}

	// ② ★ 完全保序：优胜者落在该 UID **首次出现**的位置
	out := dedupeAuthByUID([]*Auth{other, old, fresh})
	if out[0].UID != "u2" || out[1].AccessKey != "AK_NEW" {
		t.Errorf("应按首次出现排序且 u1 位置留给优胜者，得到 %s/%s", out[0].UID, out[1].AccessKey)
	}

	// ③ ★ 对"已经每位一个 UID"的列表必须是**纯 no-op**（连顺序都不变）
	//     —— 这是"在出口加一层去重不会改变既有行为"的依据（装配层注入分支靠它）。
	uniq := []*Auth{authFixture("a", "AK_A", 3), authFixture("b", "AK_B", 2), authFixture("c", "AK_C", 1)}
	got := dedupeAuthByUID(uniq)
	if len(got) != len(uniq) {
		t.Fatalf("no-op 长度应相同，得到 %d", len(got))
	}
	for i := range uniq {
		if got[i] != uniq[i] {
			t.Errorf("no-op 第 %d 位被改动：%+v → %+v", i, uniq[i], got[i])
		}
	}

	// ④ 空 UID **原位置保留**（早先版本会把它挪到末尾，那不是 no-op）
	mixed := []*Auth{noUID, fresh, authFixture("", "AK_NOUID2", now+700)}
	got = dedupeAuthByUID(mixed)
	if len(got) != 3 || got[0].AccessKey != "AK_NOUID" || got[2].AccessKey != "AK_NOUID2" {
		t.Errorf("空 UID 应原位保留，得到 %+v", got)
	}

	// ⑤ nil 跳过不 panic（契约测试会喂零值）
	if got := dedupeAuthByUID([]*Auth{nil, fresh, nil}); len(got) != 1 || got[0].AccessKey != "AK_NEW" {
		t.Errorf("nil 应被跳过，得到 %+v", got)
	}
	if got := dedupeAuthByUID(nil); len(got) != 0 {
		t.Errorf("nil 输入应返回空，得到 %+v", got)
	}
}

// writeCredFile 落一份能被 LoadDir 读到的凭证（文件名前缀必须是 codearts-）。
func writeCredFile(t *testing.T, dir, ak string, a *Auth) {
	t.Helper()
	doc := map[string]any{
		"auth": map[string]any{
			"accessKeyId":     ak,
			"secretAccessKey": "SK_" + ak,
			"securityToken":   "ST_" + ak,
			"expiresAt":       a.ExpiresAt,
			"clientId":        "vscode-codebot",
			"refresh_token":   "RT_" + ak,
		},
		"account": map[string]any{"uid": a.UID, "nickname": "nick-" + a.UID},
		"dpop":    map[string]any{"privateKeyJwk": map[string]any{"kty": "EC", "crv": "P-256", "x": "x", "y": "y", "d": "d"}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codearts-"+ak+".json"), raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", ak, err)
	}
}

// TestLocalAccountsDedupes 钉住"**出口**已去重"：目录里同 UID 两份（一份过期）⇒ 只回 1 份且是有效那份。
//
// 这是"一处修、六个调用方全受益"的核心证据：调用方什么都不用做，
// 拿到手的就是收敛后的列表（admin / dailycheckin / jobs 全都走这里）。
func TestLocalAccountsDedupes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()
	writeCredFile(t, dir, "HSTA_EXPIRED", authFixture("UID_DUP", "HSTA_EXPIRED", now-3600*24*18))
	writeCredFile(t, dir, "HSTA_FRESH", authFixture("UID_DUP", "HSTA_FRESH", now+3600))
	writeCredFile(t, dir, "HSTA_SOLO", authFixture("UID_SOLO", "HSTA_SOLO", now+600))

	p := &Provider{authDir: dir}
	list, err := p.localAccounts()
	if err != nil {
		t.Fatalf("localAccounts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应去重成 2 个账号（3 份文件 → 2 个 UID），得到 %d: %+v", len(list), list)
	}
	for _, a := range list {
		if a.UID == "UID_DUP" && a.AccessKey != "HSTA_FRESH" {
			t.Errorf("UID_DUP 应保留未过期的那份（HSTA_FRESH），得到 %s", a.AccessKey)
		}
	}
	if list[0].UID != "UID_DUP" || list[1].UID != "UID_SOLO" {
		t.Errorf("顺序应按首次出现（UID_DUP, UID_SOLO），得到 %s, %s", list[0].UID, list[1].UID)
	}
}

// TestLocalAccountsDedupesInjectedBranch 钉住**注入分支**也去重，且对"已唯一"的列表是 no-op。
func TestLocalAccountsDedupesInjectedBranch(t *testing.T) {
	now := time.Now().Unix()
	expired := authFixture("UID_X", "AK_OLD", now-100)
	fresh := authFixture("UID_X", "AK_NEW", now+100)

	// ① 注入了一份重复的列表 ⇒ 出口收敛
	p := &Provider{accounts: func() []*Auth { return []*Auth{expired, fresh} }}
	list, err := p.localAccounts()
	if err != nil {
		t.Fatalf("localAccounts: %v", err)
	}
	if len(list) != 1 || list[0].AccessKey != "AK_NEW" {
		t.Errorf("注入分支应去重并保留 AK_NEW，得到 %+v", list)
	}

	// ② 注入的列表本来就唯一 ⇒ 必须是**纯 no-op**（含顺序），证明加这层不改变既有行为
	uniq := []*Auth{authFixture("A", "AK_A", 3), authFixture("B", "AK_B", 2)}
	p2 := &Provider{accounts: func() []*Auth { return uniq }}
	got, err := p2.localAccounts()
	if err != nil {
		t.Fatalf("localAccounts: %v", err)
	}
	if len(got) != 2 || got[0] != uniq[0] || got[1] != uniq[1] {
		t.Errorf("已唯一的注入列表应原样返回，得到 %+v", got)
	}
}
