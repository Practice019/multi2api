package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// quotaRecord 记录写回池的额度调用。
type quotaRecord struct {
	mu   sync.Mutex
	got  map[string]gateway.QuotaView
	call int
}

func (r *quotaRecord) sink(uid string, q gateway.QuotaView) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = map[string]gateway.QuotaView{}
	}
	r.got[uid] = q
	r.call++
}

// usageBody 造一份真实的 usage 响应（取自 2026-09-30 实测）。
//
// ⚠ 关键是 `userQuota.remaining = 0` 而 `addOnQuota.remaining = 100` ——
// 用户报「额度是 0」的那个号正是这个形状：套餐额度确实为 0，
// 但**资源包里有 100**。只读 userQuota 就会显示 0。
const usageBody = `{"displayMode":"qoder","qoderUsage":{` +
	`"userId":"u1","userType":"personal_standard","usageType":"credits",` +
	`"userQuota":{"total":0,"used":0,"remaining":0,"percentage":0,"unit":"credits"},` +
	`"addOnQuota":{"total":100,"used":0,"remaining":100,"percentage":0,"unit":"credits"}}}`

// TestWriteBackQuotaSumsAddOnQuota 额度写回必须**累加资源包**，不是只读套餐额度。
//
// # 用户报「额度是 0」的直接原因（实测的响应）
//
//	{"userQuota":{"total":0,"used":0,"remaining":0},
//	 "addOnQuota":{"total":100,"used":0,"remaining":100}}
//
// 套餐额度确实是 0，但资源包里有 100 —— 用户实际能用的就是那 100。
// 只读 `userQuota` 会把"有 100 可用"渲染成"0"。
func TestWriteBackQuotaSumsAddOnQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(usageBody))
	}))
	defer srv.Close()

	rec := &quotaRecord{}
	c := NewWithBase(srv.URL)
	p := NewWithConfig(Config{Client: c, QuotaSink: rec.sink})
	// RefreshQuota 走 p.creds 取凭证 → 注入一条
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{Provider: providerID, UID: uid,
			Secret: &Auth{AccessToken: "dt-x", UID: uid, ProductID: providerID}}, true
	})

	p.writeBackQuota(context.Background(), &Auth{AccessToken: "dt-x", UID: "u1", ProductID: providerID})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.call == 0 {
		t.Fatal("额度没有写回池 —— 界面会一直显示空（用户报的「额度是 0」）")
	}
	q := rec.got["u1"]
	if !q.HasData {
		t.Error("HasData 必须是 true（真查到了）")
	}
	if q.Remaining != 100 {
		t.Errorf("写回的额度 = %d，want 100（套餐 0 + 资源包 100）—— "+
			"只读 userQuota 会把「有 100 可用」显示成 0", q.Remaining)
	}
}

// TestWriteBackQuotaSkipsWhenNoData 查不到时**不写池**（界面保持"未知"）。
//
// # 为什么这条重要
//
// `RefreshQuota` 在"上游没给出可用余额"时返回 `HasData=false`。
// 若把它写成 0，界面会显示 0 —— 那比"未知"更糟，因为它看起来像
// "额度被用光了"，会误导用户去充值。
func TestWriteBackQuotaSkipsWhenNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 上游挂了
	}))
	defer srv.Close()

	rec := &quotaRecord{}
	c := NewWithBase(srv.URL)
	p := NewWithConfig(Config{Client: c, QuotaSink: rec.sink})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{Provider: providerID, UID: uid,
			Secret: &Auth{AccessToken: "dt-x", UID: uid, ProductID: providerID}}, true
	})

	p.writeBackQuota(context.Background(), &Auth{AccessToken: "dt-x", UID: "u1", ProductID: providerID})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.call != 0 {
		t.Errorf("查不到额度时不该写池（会把「未知」显示成 0），实际写了 %+v", rec.got)
	}
}

// TestWriteBackQuotaNoSinkIsNoop 没有注入写回通道时**不 panic**。
//
// 反向守卫：`quotaSink == nil`（未接线的部署 / 纯单测）不能崩 ——
// 那会让整个后台任务挂掉，连带续期也不做了。
func TestWriteBackQuotaNoSinkIsNoop(t *testing.T) {
	p := NewWithConfig(Config{})
	p.writeBackQuota(context.Background(), &Auth{AccessToken: "dt-x", UID: "u1"})
	// 能走到这里就是没 panic
}

// TestRunRefreshAlsoWritesQuota 续期那一轮**顺带**把额度写回。
//
// # 为什么额度挂在续期任务里（而不是新起一个任务）
//
// 两者打同一个 host，已有的一轮扫描顺手多一次 GET 即可；另起任务要再配
// 一次间隔与去重（任务名又得按产品区分 —— 上一轮刚在那上面栽过）。
//
// ⚠ 而且额度与 token 寿命是**两件事**：额度随时可能被消耗，
// 所以写回不看 `shouldRefresh`（那个判据只管 token）。
func TestRunRefreshAlsoWritesQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "campaigns") {
			_, _ = w.Write([]byte(`{"campaigns":[]}`))
			return
		}
		if strings.Contains(r.URL.Path, "usage") {
			_, _ = w.Write([]byte(usageBody))
			return
		}
		// 续期端点
		_, _ = w.Write([]byte(`{"device_token":"dt-NEW","refresh_token":"drt-NEW",` +
			`"expires_at":"2026-10-30T06:56:55Z"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	writeAuthFile(t, dir, "qoder-u1.json", "u1", "dt-OLD", "drt-OLD", providerID)

	rec := &quotaRecord{}
	c := NewWithBase(srv.URL)
	p := NewWithConfig(Config{Client: c, AuthDir: dir, QuotaSink: rec.sink})
	// RefreshQuota 需要 creds 才能取到凭证
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{Provider: providerID, UID: uid,
			Secret: &Auth{AccessToken: "dt-OLD", UID: uid, ProductID: providerID}}, true
	})

	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh 报错: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.call == 0 {
		t.Fatal("续期那一轮没有写回额度 —— 重启后界面额度列会一直是空的（用户报障）")
	}
	if q := rec.got["u1"]; q.Remaining != 100 {
		t.Errorf("写回额度 = %d，want 100", q.Remaining)
	}
}

// TestFreshTokenStillUpdatesQuota 即使 token 不需要续期，额度**也要**更新。
//
// 这是"两件事"的证明：token 刚签发（不需要刷）时，
// 额度仍可能因为消耗而变化 —— 不更新就会显示过期数字。
func TestFreshTokenStillUpdatesQuota(t *testing.T) {
	var sawUsage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "usage") {
			sawUsage = true
			_, _ = w.Write([]byte(usageBody))
			return
		}
		_, _ = w.Write([]byte(`{"device_token":"dt-NEW"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	// 写一份**刚签发**的凭证：expires_at 在 30 天后（远超 30 分钟扫描窗口）
	raw := `{"auth":{"access_token":"dt-fresh","refresh_token":"drt-x",` +
		`"expires_at":` + itoa64(time.Now().Add(30*24*time.Hour).UnixMilli()) + `,` +
		`"machine_id":"mid","uid":"u1","product_id":"qoder"},` +
		`"account":{"uid":"u1","nickname":"n"}}`
	writeRaw(t, dir, "qoder-u1.json", raw)

	rec := &quotaRecord{}
	c := NewWithBase(srv.URL)
	p := NewWithConfig(Config{Client: c, AuthDir: dir, QuotaSink: rec.sink})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{Provider: providerID, UID: uid,
			Secret: &Auth{AccessToken: "dt-fresh", UID: uid, ProductID: providerID}}, true
	})

	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh 报错: %v", err)
	}
	if !sawUsage {
		t.Error("token 不需要续期时**仍应**查额度 —— " +
			"额度会随消耗变化，不更新就会显示过期数字")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.call == 0 {
		t.Error("额度没有写回")
	}
}
