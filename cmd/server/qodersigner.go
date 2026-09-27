// qodersigner.go 把 internal/qoderwasm 的签名器适配成 qoder.RequestSigner。
//
// # 为什么需要这层适配（而不是让 qoderwasm 直接实现那个接口）
//
// 两个包的类型不同，且**不能合并**：
//
//	qoder.SignIdentity  {UID, AccessToken, MachineID}      ← 上游包的类型
//	qoderwasm.Identity  {UID, AccessToken, MachineID}      ← WASM 包的类型
//
// 形状一样，但 qoderwasm **不能 import internal/qoder** —— 后者依赖
// internal/gateway，而 arch_test 的 discoverUpstreams 用 `go list -deps`
//（**传递**依赖）判定"谁消费契约"：一旦 qoderwasm 依赖它，
// 就会被判成一个没有 Provider 方法的上游 → 判据红。
//
// 所以适配放在装配层（这里可以同时 import 两边）。这与
// workbuddy.WrapOAuth / poolAdapter 是同一手法：
// **上游类型 ≠ 核心类型，方法集精确匹配，必须显式转换**。
package main

import (
	"context"
	"log"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/qoderwasm"
)

// qoderSigner 把 WASM 签名器接到 qoder 的接缝上。
type qoderSigner struct {
	s *qoderwasm.Signer
}

// BuildInferRequest 实现 qoder.RequestSigner。
func (a qoderSigner) BuildInferRequest(id qoder.SignIdentity, model string, body []byte) (string, []byte, map[string]string, error) {
	return a.s.BuildInferRequest(qoderwasm.Identity{
		UID:         id.UID,
		AccessToken: id.AccessToken,
		MachineID:   id.MachineID,
	}, model, body)
}

// 编译期断言：适配器确实满足上游的接缝。
var _ qoder.RequestSigner = qoderSigner{}

// newQoderSigner 按配置建签名器（失败返回 nil = 加密推理不可用）。
//
// # 为什么失败只记日志、不 Fatalf
//
// 与其它上游不同，**qoder 的其余能力（模型目录、账号管理、登录）都不依赖
// WASM**。签名器建不起来（比如 WASM 与桥的实现不匹配）时，
// 让整个网关起不来是过度的：应该只让"对话"这一项不可用，
// 而把原因明确记在日志里。
//
// 反过来，如果**静默**降级（不记日志、或记成 Debug），
// 用户会看到"qoder 账号能加、模型能列、但一问就报错"，
// 而日志里找不到任何线索 —— 那正是本项目反复避免的"静默失效"。
func newQoderSigner(host string) *qoderwasm.Signer {
	s, err := qoderwasm.NewSigner(qoderwasm.Options{
		Host: host,
		// 与设备码授权用的元数据保持一致（见 internal/qoder 的 clientMetadata）。
		// 身份不一致会让服务端把我们当成两个不同的客户端。
		ClientType:      "5",
		BusinessProduct: "cli",
		BusinessType:    "agent",
		Scene:           "assistant",
	})
	if err != nil {
		log.Printf("qoder: ⚠ WASM 签名器创建失败，**加密推理不可用**（对话会明确报错）：%v", err)
		return nil
	}
	return s
}

// closeQoderSigner 释放签名器（幂等）。
func closeQoderSigner(s *qoderwasm.Signer) {
	if s == nil {
		return
	}
	if err := s.Close(context.Background()); err != nil {
		log.Printf("qoder: 释放 WASM 签名器时出错: %v", err)
	}
}

// qoderCredSource 造一个"按 uid 取活凭证"的访问器（签到与余额端点用）。
//
// # 为什么不能用账号池的 AuthByUID
//
// `AuthByUID` 返回的是**核心账号投影**（只有 uid / nickname / filePath），
// 而签到与余额要的是 `*qoder.Auth`（含 access_token）——
// 那个东西在 `pool.SecretOf` 里（凭证并池时由 SyncToDirWithSecrets 装进去）。
//
// # 为什么 providerID 必须按实例传
//
// 两个产品（qoder / qodercn）的凭证在**同一个目录**，靠凭证里的
// product_id 区分，并池时也按 provider 标签分域。所以投影必须带对标签：
// 传错会让 qodercn 的端点取到 qoder 的账号（或取不到，
// 表现为"账号不在池里"这种误导性错误）。
func qoderCredSource(p *pool.Pool, providerID string) func(string) (gateway.Credential, bool) {
	return func(uid string) (gateway.Credential, bool) {
		secret, ok := p.SecretOf(uid)
		if !ok {
			return gateway.Credential{}, false
		}
		a, ok := secret.(*qoder.Auth)
		if !ok || a == nil {
			return gateway.Credential{}, false
		}
		return gateway.Credential{
			Provider: providerID,
			UID:      uid,
			Nickname: a.Nickname,
			Secret:   a,
		}, true
	}
}
