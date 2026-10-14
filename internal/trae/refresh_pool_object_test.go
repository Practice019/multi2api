// refresh_pool_object_test.go 后台续期必须刷**池里持有的那个对象**。
//
// # 这条守卫是实测确认的真实缺陷
//
// 用户点名的两个上游里，TRAE 有这个问题（MiniMax 是另一个：压根没有后台任务）。
//
// 缺陷形态：`runRefresh` 走 `localAccounts()` = `LoadDir(authDir)`，
// 而 `LoadDir` 用 `Parse(raw)` **每次新建对象**；池子持有的是启动时
// `syncTraeAccounts` 放进 `secrets[uid]` 的**另一个** `*Auth`。于是：
//
//	续期成功 → 磁盘更新了（SaveAtomic）
//	池里那份纹丝不动 → TokenExpiry 读池 ⇒ 界面一直「已过期」
//
// # 为什么必须先观测到它（而不是直接读代码改）
//
// "读起来像 bug"不等于"是 bug"。我第一版假上游的响应字段写错了
// （用了 OpenAI 的 `access_token`，而 trae 的 exchange 回执是 `result.token`），
// 于是测试红在"续期失败"上 —— 那个红**不能**证明对象分叉。
// 修好响应格式之后才拿到干净证据：
//
//	上游交换次数=1
//	池里那份 AccessToken = "old-access"   ← 没变
//	磁盘那份 AccessToken = "new-access"   ← 变了
//
// 同理，本文件最后那条"未接线时仍分叉"的用例是**故意的**：它把
// "必须接线"这件事写成可执行的断言，而不是靠注释提醒。
//
// # 为什么这对 TRAE 尤其致命
//
// TRAE 的 `RefreshSkew` 返回 **(0, true)** —— 显式声明"请求路径不预检续期"
// （见 extensions.go，理由是 refreshToken 消费型，预检会让每次请求都轮换）。
// 所以**后台是唯一的续期路径**：一旦它刷错对象，界面状态就永久停在过期上，
// 直到进程重启。
package trae

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

	"workbuddy2api/internal/gateway"
)

// poolObjectFixture 造"池里那份"与"磁盘那份"分叉的场景。
type poolObjectFixture struct {
	p        *Provider
	dir      string
	exch     *int64
	poolCopy *Auth
}

func newPoolObjectFixture(t *testing.T) *poolObjectFixture {
	t.Helper()
	var exch int64
	// ⚠ 回执形状必须对：trae 的 exchange 是 `result.token` / `result.refreshToken`，
	// **不是** OpenAI 的 `access_token`。写错的话续期会"失败"，
	// 测试就红了 —— 但红在一个与待测性质无关的理由上，等于没测。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&exch, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"token":        "new-access",
				"refreshToken": "new-refresh",
			},
		})
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	expiring := &Auth{
		UID:              "u-pool",
		AccessToken:      "old-access",
		RefreshToken:     "old-refresh",
		ExpiresAt:        time.Now().Add(5 * time.Minute).Unix(),
		RefreshExpiresAt: time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
	raw, err := json.Marshal(expiring)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠ 必须设 FilePath：生产路径上池里那份是 `LoadDir` 建出来的，
	// 而 LoadDir 会 `a.FilePath = fp`；`SaveAtomic` 依赖它（没有就报
	// "没有 FilePath（凭证未从目录加载？）"）。夹具漏设的话，
	// "落盘失败"会被误读成实现缺陷 —— 我第一版就是这么红了一次。
	fp := filepath.Join(dir, FileName(expiring))
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	expiring.FilePath = fp

	return &poolObjectFixture{
		p: NewWithConfig(Config{
			AuthDir:   dir,
			AgentBase: srv.URL,
			UgBase:    srv.URL,
			OAuthBase: srv.URL,
		}),
		dir:  dir,
		exch: &exch,
		// 「池里那份」：启动时 LoadDir 出来、被放进 secrets 的那个指针。
		poolCopy: expiring,
	}
}

// wirePoolObject 模拟装配层接线：credSrc 返回**池里那个**指针。
func (f *poolObjectFixture) wirePoolObject() {
	f.p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		if uid != f.poolCopy.UID {
			return gateway.Credential{}, false
		}
		return gateway.Credential{
			Provider: ProviderID,
			UID:      uid,
			Secret:   f.poolCopy, // 同一个指针 —— 这正是要保证的
		}, true
	})
}

func (f *poolObjectFixture) runOnce(t *testing.T) {
	t.Helper()
	if err := f.p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh: %v", err)
	}
}

// ⚠ 核心断言：接线之后，**池里那份**必须被续期更新。
//
// 红的形态就是缺陷本身：磁盘新了、池里还是旧的 ⇒ 界面永久显示「已过期」。
func TestRunRefreshUpdatesPoolObject(t *testing.T) {
	f := newPoolObjectFixture(t)
	f.wirePoolObject()
	f.runOnce(t)

	if n := atomic.LoadInt64(f.exch); n == 0 {
		t.Fatal("一轮都没打上游 —— 续期根本没发生，本测试无意义")
	}
	if f.poolCopy.AccessToken != "new-access" {
		t.Errorf("**池里那份没被更新**（AccessToken 仍是 %q，期望 \"new-access\"）——\n"+
			"后台续期刷的是 LoadDir 新建的副本，而池子持有的是另一个对象。\n"+
			"后果：`TokenExpiry` 读池里那份 ⇒ 界面一直显示「已过期」，"+
			"而磁盘上的 token 其实是新的。\n"+
			"（TRAE 的 RefreshSkew 返回 (0,true) ⇒ 请求路径永不预检，"+
			"后台是唯一续期路径，所以这个状态是永久的。）", f.poolCopy.AccessToken)
	}
	// refresh_token 是消费型的，轮换后也必须写回池里那份。
	if f.poolCopy.RefreshToken != "new-refresh" {
		t.Errorf("轮换后的 refresh_token 没写回池里那份: %q（期望 \"new-refresh\"）——\n"+
			"下一个续期周期会拿作废的旧 refresh_token 去换，必然失败。", f.poolCopy.RefreshToken)
	}
}

// 磁盘那份**也要**更新（落盘不能因为改用池对象而丢掉）。
//
// 只守"池里更新"会让一个"只改内存不落盘"的实现在重启后回到旧 token 上，
// 而那条路径不会报错。
func TestRunRefreshAlsoPersistsToDisk(t *testing.T) {
	f := newPoolObjectFixture(t)
	f.wirePoolObject()
	f.runOnce(t)

	list, err := LoadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("磁盘上应有 1 份凭证，实际 %d", len(list))
	}
	if list[0].AccessToken != "new-access" {
		t.Errorf("磁盘那份没被更新: %q（期望 \"new-access\"）——\n"+
			"重启后会读回旧 token，表现为「刚重启就又过期了」。", list[0].AccessToken)
	}
}

// ⚠ 未接线时必须**如实分叉**（而不是假装正常）。
//
// 这条守的是"接线是必需的"这件事本身：装配层若忘了调 SetCredentialSource，
// 行为就回到缺陷形态。把它写成断言而不是靠注释，是因为
// "忘了接线"正是本仓反复出现的失误（TRAE 就是唯一漏掉这条接线的上游）。
//
// 同时它也防止有人把 credSrc 的回落写成"静默用磁盘副本也能对" ——
// 那是做不到的（磁盘副本与池对象本来就是两个指针）。
func TestRunRefreshWithoutWiringDiverges(t *testing.T) {
	f := newPoolObjectFixture(t)
	// 故意不接线。
	f.runOnce(t)

	if n := atomic.LoadInt64(f.exch); n == 0 {
		t.Fatal("一轮都没打上游 —— 续期根本没发生，本测试无意义")
	}
	if f.poolCopy.AccessToken == "new-access" {
		t.Error("未接线时池里那份竟然被更新了 —— 说明测试夹具没有真实模拟" +
			"「池对象与磁盘副本是两个指针」，这条守卫失去了意义")
	}
	// 磁盘仍然应该被更新（磁盘副本是它自己刷的），这是旧行为的真实形状。
	list, _ := LoadDir(f.dir)
	if len(list) != 1 || list[0].AccessToken != "new-access" {
		t.Error("未接线时磁盘那份应当仍被更新（旧行为）—— 夹具前提不成立")
	}
}
