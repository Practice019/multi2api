// traecreds_test.go 装配层守卫：**后台续期必须落回池子里那个对象**。
//
// # 这份守卫照 codeartscreds_test.go 的 TestBackgroundRefreshLandsOnPoolObject 写
//
// 那个用例是本仓对"同一份凭证在进程里有多个对象"这类缺陷的既有守卫模式，
// 而且它的注释里完整记录了同一个根因（codearts 的 503 no_healthy_account）。
// TRAE 是同类缺陷的第二个实例，所以守卫也照同一形状补。
//
// # 为什么必须在**装配层**测（上游包的测试不够）
//
// `internal/trae` 里的测试只能证明"注入了 credSrc 之后，runRefresh 会用池对象"。
// 而真正出问题的地方是**装配层忘了注入** —— 那是 main.go 的一行，
// 上游包内部的测试永远覆盖不到。
//
// 所以这里的判据是：用**真实装配**（wireTraeCreds + 真实池子）跑一遍
// **真正的后台任务**，然后断言池子里那个对象的字段变了。
// 只要 `main.go` 里那行 `tr.SetCredentialSource(...)` 被删掉/写错位置，
// 本测试就红 —— 而它红了才说明"忘了接线"这类失误不会再静默通过。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/trae"
)

// writeTraeCred 造一份 trae 凭证落盘（形状照 Parse/SaveAtomic 的 doc）。
func writeTraeCred(t *testing.T, dir, uid, access, refresh string, expiresAt int64) {
	t.Helper()
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  access,
			"refreshToken": refresh,
			"expiresAt":    expiresAt,
			// 消费型 refreshToken 自身的寿命（RefreshTokenDead 用它）。
			"refreshExpiresAt": time.Now().Add(30 * 24 * time.Hour).Unix(),
		},
		"account": map[string]any{
			"uid":      uid,
			"nickname": "trae-assembly",
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trae-"+uid+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ⚠ 核心：走**真实装配**，跑**真正的后台任务**，断言**池子里那个对象**被更新。
//
// 判据分三层，任一层失败都能定位到具体环节：
//
//	① 池里有该 uid（前置条件 —— 否则后面全是空转）
//	② Jobs() 真的注册了任务（缺这个就是"根本不会续期"，本次报障的另一半）
//	③ 跑完之后**池里那个指针**的 accessToken/refreshToken 是新值
//
// 第 ③ 层是决定性的：接线断了的话，续期会写到 LoadDir 新建的副本上，
// 池里那份仍是旧的 —— 而**磁盘会是新的**，所以只看磁盘会误判为成功。
func TestTraeBackgroundRefreshLandsOnPoolObject(t *testing.T) {
	const uid = "uid-trae-assembly"
	dir := t.TempDir()

	// 凭证必须**临近过期**，后台任务才会真的续期（生产故障的现场条件）。
	// trae 的后台窗口是 30 分钟（refreshScanSkew），所以造"剩 5 分钟"。
	writeTraeCred(t, dir, uid, "OLD_ACCESS", "OLD_REFRESH", time.Now().Add(5*time.Minute).Unix())

	var exchanges int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&exchanges, 1)
		// ⚠ 回执形状：trae 的 exchange 是 `result.token` / `result.refreshToken`。
		// 我第一版写成了 OpenAI 的 `access_token`，于是"续期失败"，
		// 测试红在一个与待测性质无关的理由上 —— 那种红不能作为证据。
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"token":"NEW_ACCESS","refreshToken":"NEW_REFRESH"}}`))
	}))
	t.Cleanup(srv.Close)

	tr := trae.NewWithConfig(trae.Config{
		AuthDir:         dir,
		AgentBase:       srv.URL,
		UgBase:          srv.URL,
		OAuthBase:       srv.URL,
		RefreshInterval: time.Minute,
		CheckinEnabled:  false, // 本用例只验续期
	})

	p := newTestPool(t)
	// ①+② 走真实装配：并池 → 接线
	syncTraeAccounts(p, dir)
	tr.SetCredentialSource(traeCredSource(p))

	before, ok := p.SecretOf(uid)
	if !ok {
		t.Fatal("前置条件不成立：池子里没有这个 uid（syncTraeAccounts 没装上）")
	}
	poolObj, ok := before.(*trae.Auth)
	if !ok {
		t.Fatalf("池 secret 类型 = %T，期望 *trae.Auth", before)
	}
	if poolObj.AccessToken != "OLD_ACCESS" {
		t.Fatalf("前置条件不成立：池里 accessToken=%q", poolObj.AccessToken)
	}

	// ③ 跑真正的后台任务
	var refreshJob *traeJob
	for _, j := range tr.Jobs() {
		if j.Name == trae.JobRefresh {
			jj := j
			refreshJob = &traeJob{run: jj.Run, interval: jj.Interval}
		}
	}
	if refreshJob == nil {
		t.Fatal("trae 没有注册后台续期任务（trae.JobRefresh）——\n" +
			"没有它，没人拿 trae 号发请求时 token 会静静过期（这正是用户报障的形态）")
	}
	if refreshJob.interval <= 0 {
		t.Errorf("后台续期任务的 Interval = %v，必须为正", refreshJob.interval)
	}
	if err := refreshJob.run(context.Background()); err != nil {
		t.Fatalf("后台续期任务报错: %v", err)
	}
	if n := atomic.LoadInt64(&exchanges); n == 0 {
		t.Fatal("一轮都没打上游 —— 任务没真的续期，本测试无意义")
	}

	// ④ 池子里**那个对象**必须拿到新凭证
	after, _ := p.SecretOf(uid)
	poolObjAfter, ok := after.(*trae.Auth)
	if !ok {
		t.Fatalf("池 secret 类型 = %T，期望 *trae.Auth", after)
	}
	if poolObjAfter != poolObj {
		t.Fatalf("★ 池 secret 换成了新对象（%p → %p）—— 续期结果写不回请求路径用的那份",
			poolObj, poolObjAfter)
	}
	if poolObj.AccessToken != "NEW_ACCESS" {
		t.Errorf("★ 后台续期成功了，但**池子里那份对象没变**（accessToken=%q，期望 NEW_ACCESS）——\n"+
			"\n"+
			"这是本仓栽过多次的形态（codearts 的 503 no_healthy_account、raccoon 的\n"+
			"「已过期」一直显示，见 codeartscreds_test.go 的同款守卫）：\n"+
			"续期刷的是 `localAccounts()` 里 `LoadDir` 新建的副本，而池子持有的是\n"+
			"启动时 syncTraeAccounts 放进去的**另一个**指针。\n"+
			"\n"+
			"对 TRAE 尤其致命：它的 RefreshSkew 返回 (0,true)（请求路径不预检续期），\n"+
			"所以**后台是唯一续期路径** —— 这个状态会一直持续到进程重启。",
			poolObj.AccessToken)
	}
	// refreshToken 是消费型的：轮换后不写回池里，下轮会拿作废的值去换。
	if poolObj.RefreshToken != "NEW_REFRESH" {
		t.Errorf("轮换后的 refreshToken 没写回池里那份: %q（期望 NEW_REFRESH）——\n"+
			"下一个续期周期会拿已作废的旧 refreshToken 去 ExchangeToken，必然失败。",
			poolObj.RefreshToken)
	}
}

// traeJob 只为把 gateway.Job 的两个字段带出来（避免在断言里反复解构）。
type traeJob struct {
	run      func(context.Context) error
	interval time.Duration
}
