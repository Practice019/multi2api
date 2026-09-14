package codearts

// refresh_selfheal_dpop_test.go —— 自愈必须连 **DPoP 私钥**一起采纳。
//
// # 这条用例防的是什么（评审发现的缺陷）
//
// `adoptDiskRefreshToken` 原先只把 RT/AK/SK/ST/ExpiresAt 抄进内存，**落下了
// DPoP 私钥**。而 DPoP 私钥与 refresh_token 是**配对**的：外部重新登录
// （或手工修好）写进同一个文件的那份新凭证，必然带一把新钥匙。
//
// 后果是一条**会自我固化**的错位 —— 不是"这次失败下次就好了"：
//
//	1. 自愈只抄一半 → 重试仍用自愈**之前**解出的 kp（旧钥匙）签新 token
//	2. 上游按"签名/绑定不匹配"拒绝 → 自愈重试白打一次，账号进冷却
//	3. store.list() 的检测判据 caCredFieldsDiffer 当时也不看钥匙，
//	   而其余字段此时**已经全部相等** → 判 false →
//	   adoptCodeartsCredInPlace（它**会**抄钥匙）再也不跑
//	4. → 该账号每次续期都失败，**直到重启进程**（启动时 byUID 为空才会全新加载）
//
// 第 4 步正是本次交付要根除的那类故障，所以它必须有回归守卫。
//
// # ⚠ 为什么不能用现有的 newRefreshServer 写这条用例（会假绿）
//
// `refreshStub` **完全不看 DPoP 证明** —— 它只校验 refresh_token。
// 于是"重试用了哪把钥匙"对结果毫无影响：自愈哪怕只抄一半，那个桩照常返回 200，
// 用例**绿**。用它写这条断言等于什么都没测。
//
// 真实上游不是这样：证明里的公钥必须与 refresh_token 绑定的一致。
// 本文件的桩复刻这条约束 —— 给每个 refresh_token 登记一把**期望公钥**，
// 证明里的 `jwk` 对不上就 400。
//
// # 为什么拒绝体刻意不含 STS5.1806
//
// "绑定不匹配"与"token 已被消费"是两回事。若拒绝体里带上 `STS5.1806`，
// 客户端会把它误判成"token 已消费"从而**再次**触发自愈分支 ——
// 于是这条用例连"判据是否够窄"都测不准了。这里用另一套 error_code。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// dpopBindingStub 一个会**校验 DPoP 公钥绑定**的假 STS 续期端点。
type dpopBindingStub struct {
	mu     sync.Mutex
	used   map[string]bool              // 已被消费的 refresh_token
	bindTo map[string]map[string]string // refresh_token → 期望的公钥 JWK
	reqs   int                          // 到达端点的请求数
	rejBind int                         // 因绑定不匹配被拒的次数（**必须为 0**）
	rejUsed int                         // 因 token 已消费被拒的次数
}

func (st *dpopBindingStub) counts() (reqs, rejBind int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.reqs, st.rejBind
}

func (st *dpopBindingStub) markUsed(rt string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.used[rt] = true
}

func (st *dpopBindingStub) bind(rt string, pub map[string]string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.bindTo[rt] = pub
}

// jwkOfProof 从 DPoP 证明（JWT）的头部取出内嵌公钥。
// 取不出来时返回 nil —— 调用方按"不匹配"处理（宁可判失败也不放过）。
func jwkOfProof(proof string) map[string]string {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil
	}
	var hdr struct {
		JWK map[string]string `json:"jwk"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return nil
	}
	return hdr.JWK
}

func newDPoPBindingServer(t *testing.T) (*httptest.Server, *dpopBindingStub) {
	t.Helper()
	st := &dpopBindingStub{
		used:   map[string]bool{},
		bindTo: map[string]map[string]string{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readAllLimited(r)
		form, _ := url.ParseQuery(string(raw))
		rt := form.Get("refresh_token")
		jwk := jwkOfProof(r.Header.Get("DPoP"))

		st.mu.Lock()
		st.reqs++

		// ① 公钥绑定校验（真实上游的核心约束，也是本用例的判据所在）。
		if exp, ok := st.bindTo[rt]; ok && !reflect.DeepEqual(exp, jwk) {
			st.rejBind++
			st.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			// ⚠ 刻意不用 STS5.1806 / "has been used"：那是"已消费"的标记，
			// 带上它会让客户端把它误判成可自愈的 token 冲突。
			_, _ = w.Write([]byte(`{"error_code":"STS5.1800",` +
				`"error_msg":"DPoP proof key does not match the key bound to this refresh token"}`))
			return
		}

		// ② 消费校验（与既有桩一致）。
		if rt != "" && st.used[rt] {
			st.rejUsed++
			st.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error_code":"STS5.1806",` +
				`"error_msg":"invalid refresh token: 'the refresh token has been used'"}`))
			return
		}
		if rt != "" {
			st.used[rt] = true
		}
		st.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"credentials":{"access_key_id":"AK-new",`+
			`"secret_access_key":"SK-new","security_token":"ST-new",`+
			`"expiration":"2099-01-01T00:00:00Z"},"refresh_token":"RT-%s"}`, randHex(8))
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

// TestRefreshSelfHealAdoptsDPoPKeyFromDisk
//
// 内存：旧钥匙 + 已消费的 token；磁盘：外部重新登录写进来的**新钥匙 + 新 token**。
// 自愈必须连钥匙一起采纳，重试才可能通过绑定校验。
//
// 删掉 client.go 里"自愈后重新解一次钥匙"或"采纳 DPoP 私钥"任一处，本用例必红。
func TestRefreshSelfHealAdoptsDPoPKeyFromDisk(t *testing.T) {
	srv, st := newDPoPBindingServer(t)
	dir := t.TempDir()
	a := newTestAuth(t, dir) // 内存这把钥匙（K0）

	// ── 夹具自检：两把钥匙必须真的不同，否则本用例恒绿、什么都没测 ──
	memJWK := append([]byte(nil), a.DPoPPrivateKeyJWK...)
	kpMem, err := FromPrivateJWK(memJWK)
	if err != nil {
		t.Fatalf("恢复内存钥匙: %v", err)
	}
	kpDisk, err := NewDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(kpMem.PublicJWK(), kpDisk.PublicJWK()) {
		t.Fatal("夹具无效：内存与磁盘用的是同一把钥匙，这条用例证明不了任何事")
	}

	// 内存那份 token 已被消费（外部重新登录时把它用掉了）。
	consumedRT := "RT-consumed-" + randHex(4)
	st.markUsed(consumedRT)

	// ── 磁盘：外部重新登录写进来的新凭证（新 token + **新钥匙**）──
	diskJWK, err := json.Marshal(kpDisk.PrivateJWK())
	if err != nil {
		t.Fatal(err)
	}
	diskRT := "RT-disk-" + randHex(4)
	a.RefreshToken = diskRT
	a.DPoPPrivateKeyJWK = diskJWK
	a.ExpiresAt = time.Now().Add(time.Hour).Unix()
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	// 服务端登记绑定：这个 refresh_token 只认磁盘那把公钥。
	st.bind(diskRT, kpDisk.PublicJWK())

	// ── 内存退回"旧钥匙 + 已消费的 token"（磁盘保持不动）──
	a.DPoPPrivateKeyJWK = memJWK
	a.RefreshToken = consumedRT
	a.ExpiresAt = time.Now().Add(-time.Hour).Unix()

	c := New()
	c.STSBase = srv.URL

	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("自愈应当连 DPoP 私钥一起采纳并重试成功，实际失败: %v\n"+
			"（若错误是 400 STS5.1800 绑定不匹配，说明重试仍拿自愈前的旧钥匙签名）", err)
	}

	// 断言 1：重试**没有**拿错钥匙。
	reqs, rejBind := st.counts()
	if rejBind != 0 {
		t.Errorf("重试被绑定校验拒绝 %d 次 —— 自愈没有把 DPoP 私钥一起采纳", rejBind)
	}
	// 断言 2：恰好两次请求（首次被拒 + 自愈重试），没有多打。
	if reqs != 2 {
		t.Errorf("应恰好发 2 次请求（首次 + 重试 1 次），实际 %d", reqs)
	}
	// 断言 3：对象上的钥匙确实换成了磁盘那把。
	//
	// ⚠ 按**钥匙语义**比，不按字节：磁盘那份是缩进 JSON，而夹具里的 diskJWK 是
	// json.Marshal 的紧凑形态 —— 同一把钥匙的字节形态并不相同
	// （详见 DPoPKeysDiffer 的注释）。按字节比会误报，把产品缺陷和夹具口径
	// 混在一起，反而看不清。
	gotKP, err := FromPrivateJWK(a.DPoPPrivateKeyJWK)
	if err != nil {
		t.Fatalf("自愈后对象上的 DPoP 私钥解析失败: %v", err)
	}
	if !reflect.DeepEqual(gotKP.PublicJWK(), kpDisk.PublicJWK()) {
		t.Errorf("自愈后 DPoP 私钥未随凭证更新：\n  实际公钥 = %v\n  期望公钥 = %v",
			gotKP.PublicJWK(), kpDisk.PublicJWK())
	}
	// 断言 4：token 也已轮换（不再是任何一份旧值）。
	if a.RefreshToken == consumedRT || a.RefreshToken == diskRT || a.RefreshToken == "" {
		t.Errorf("自愈后 refresh_token 应已轮换为服务端新发的，实际 %q", a.RefreshToken)
	}
}

// TestAdoptDiskRefreshTokenKeepsKeyWhenDiskHasNone
//
// 反向边界：磁盘那份**没有** DPoP 私钥时，不得把内存里可用的钥匙清空 ——
// 清空会让对象从"能自愈"变成"连发请求的资格都没有"（RefreshToken 开头就会
// 因缺 DPoP 私钥直接返回）。
func TestAdoptDiskRefreshTokenKeepsKeyWhenDiskHasNone(t *testing.T) {
	dir := t.TempDir()
	a := newTestAuth(t, dir)
	memJWK := append([]byte(nil), a.DPoPPrivateKeyJWK...)
	if len(memJWK) == 0 {
		t.Fatal("夹具无效：内存凭证没有 DPoP 私钥")
	}

	// 磁盘上写一份 token 不同、但**没有 dpop 段**的凭证。
	a.RefreshToken = "RT-disk-" + randHex(4)
	a.DPoPPrivateKeyJWK = nil
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	// 内存恢复钥匙，token 退回另一份，使自愈分支成立。
	a.DPoPPrivateKeyJWK = memJWK
	a.RefreshToken = "RT-mem-" + randHex(4)

	c := New()
	// 读一下磁盘上真正躺着什么（诊断 + 前置事实）。
	diskRawJWK := diskJWKOf(t, a)
	if !c.adoptDiskRefreshToken(a) {
		t.Fatal("磁盘 token 与内存不同，自愈应当成立")
	}
	if len(a.DPoPPrivateKeyJWK) == 0 {
		t.Error("磁盘那份没有 DPoP 私钥时，不得把内存里可用的钥匙清空")
	}
	// ⚠ 用 bytes.Equal 而不是 reflect.DeepEqual：`a.DPoPPrivateKeyJWK` 是
	// `json.RawMessage`（具名类型），memJWK 是 `[]byte`（匿名类型）——
	// DeepEqual 对**类型不同**的值一律判不等，哪怕字节完全一样。
	// 这里踩过一次，表现是"对象值与被采纳值逐字节相同却报未保持原样"。
	if !bytes.Equal(a.DPoPPrivateKeyJWK, memJWK) {
		t.Errorf("磁盘没有钥匙时，内存那把应当保持原样：\n  磁盘原始字节 = %q\n  内存原始字节 = %q\n  对象当前值   = %q\n  HasUsableDPoPKey(磁盘) = %v",
			diskRawJWK, memJWK, a.DPoPPrivateKeyJWK, HasUsableDPoPKey(diskRawJWK))
	}
}

// diskJWKOf 读出 a.FilePath 上那份凭证的 DPoP 私钥原始字节（诊断用）。
func diskJWKOf(t *testing.T, a *Auth) []byte {
	t.Helper()
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	return d.DPoPPrivateKeyJWK
}
