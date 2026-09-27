package main

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/qoder"
)

// TestQoderSignerAdapterProducesRealEncryptedRequest 装配层的适配器必须真的
// 产出加密请求（走真 WASM，不碰网络）。
//
// # 这条测试补的是什么洞
//
// internal/qoderwasm 的测试验证的是**签名器本身**；
// internal/qoder 的测试用的是**桩签名器**（它不加密）。
// 两者之间的**适配器**（qoderSigner）此前没有任何测试 ——
// 而它正是"字段名写错/漏传一个"最容易发生的地方：
//
//	qoder.SignIdentity{UID, AccessToken, MachineID}
//	              ↓ 手写映射，逐个字段
//	qoderwasm.Identity{UID, AccessToken, MachineID}
//
// 三个同名字段手抄一遍，抄错一个不会有编译错误（类型都一样是 string），
// 而后果是"签名与身份不符" → 上游 403，排查时看起来像凭据问题。
//
// 判据落在**产物**上而不是"调了哪个函数"：产物里必须出现该 uid
//（`Cosy-User`），且必须是一份带 COSY 签名的真加密请求。
func TestQoderSignerAdapterProducesRealEncryptedRequest(t *testing.T) {
	s := newQoderSigner("api2.qoder.sh")
	if s == nil {
		t.Fatal("签名器创建失败 —— 真 WASM 应该能实例化")
	}
	defer closeQoderSigner(s)

	adapter := qoderSigner{s: s}
	body, _ := json.Marshal(map[string]any{
		"model":    "qfmodel",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   true,
	})

	path, payload, headers, err := adapter.BuildInferRequest(qoder.SignIdentity{
		UID:         "adapter-probe-uid",
		AccessToken: "adapter-probe-token",
		MachineID:   "adapter-probe-machine",
	}, "qfmodel", body)
	if err != nil {
		t.Fatalf("BuildInferRequest: %v", err)
	}

	// 1. URL query（加密端点的固定参数）
	if !strings.Contains(path, "FetchKeys=llm_model_result") ||
		!strings.Contains(path, "AgentId=agent_common") {
		t.Errorf("URL query 不对: %q", path)
	}

	// 2. 身份必须**逐字段**穿过适配器
	//
	// 这是本测试的核心：三个同名字段手抄，抄错不会编译失败。
	if got := headers["Cosy-User"]; got != "adapter-probe-uid" {
		t.Errorf("Cosy-User = %q，want adapter-probe-uid —— "+
			"适配器把 UID 传丢了或传错了", got)
	}
	if got := headers["Cosy-MachineId"]; got != "adapter-probe-machine" {
		t.Errorf("Cosy-MachineId = %q，want adapter-probe-machine —— "+
			"适配器把 MachineID 传丢了或传错了", got)
	}
	// AccessToken 不直接出现在头里（它进 encrypt_user_info），
	// 但它的缺失会让 Cosy-Key 为空 —— 那条在下面一起判。

	// 3. 必须是**真加密**请求（不是桩签名器的形态）
	auth := headers["Authorization"]
	if !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("Authorization 不是 COSY 签名形态: %.60q", auth)
	}
	if headers["Cosy-Key"] == "" {
		t.Error("Cosy-Key 为空 —— AccessToken 没传到 WASM（它由 token 派生）")
	}
	if len(payload) < 500 {
		t.Errorf("加密体只有 %d 字节 —— 不像是真加密的产物", len(payload))
	}
	// 加密体不该是明文 JSON
	if json.Valid(payload) {
		t.Error("加密体是合法 JSON —— 说明根本没加密")
	}
}

// TestQoderSignerAdapterSeparatesAccounts 不同账号必须得到不同的签名头。
//
// 判据落在**身份隔离**上：若适配器/签名器把账号身份搞混（比如共用一份
// context），两个账号会得到相同的 Cosy-User 或相同的 Cosy-Key ——
// 那意味着请求可能被发到错误的账号上，是最严重的一类缺陷。
func TestQoderSignerAdapterSeparatesAccounts(t *testing.T) {
	s := newQoderSigner("api2.qoder.sh")
	if s == nil {
		t.Fatal("签名器创建失败")
	}
	defer closeQoderSigner(s)
	adapter := qoderSigner{s: s}

	body, _ := json.Marshal(map[string]any{
		"model":    "qfmodel",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	_, _, h1, err := adapter.BuildInferRequest(qoder.SignIdentity{
		UID: "acct-A", AccessToken: "tok-A", MachineID: "m-A",
	}, "qfmodel", body)
	if err != nil {
		t.Fatal(err)
	}
	_, _, h2, err := adapter.BuildInferRequest(qoder.SignIdentity{
		UID: "acct-B", AccessToken: "tok-B", MachineID: "m-B",
	}, "qfmodel", body)
	if err != nil {
		t.Fatal(err)
	}

	if h1["Cosy-User"] != "acct-A" || h2["Cosy-User"] != "acct-B" {
		t.Errorf("身份串了：h1.Cosy-User=%q h2.Cosy-User=%q",
			h1["Cosy-User"], h2["Cosy-User"])
	}
	if h1["Cosy-MachineId"] != "m-A" || h2["Cosy-MachineId"] != "m-B" {
		t.Errorf("机器标识串了：%q / %q", h1["Cosy-MachineId"], h2["Cosy-MachineId"])
	}
	// 两个账号的鉴权密钥必须不同（各自 token 派生）
	if h1["Cosy-Key"] == h2["Cosy-Key"] {
		t.Error("两个账号的 Cosy-Key 相同 —— 身份没隔离，请求可能发到错的账号上")
	}
}
