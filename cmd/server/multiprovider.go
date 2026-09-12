// multiprovider.go 装配层的多上游接线：账号入池 + 出口层路由实现。
//
// # 这个文件在整条解耦链里的位置
//
// 它是**唯一同时认识核心与所有具体上游的地方**。两个方向在这里汇合：
//
//	核心 → 上游：账号池按 provider 分域，把 codearts 的账号也收进来
//	上游 → 核心：出口层（internal/server）按前缀问"这个上游有没有、模型有哪些"
//
// 两边都靠**适配器**连接，而不是让任何一侧去 import 另一侧的具体类型 ——
// 这样"加第 N 个上游"仍然只是新增一个目录 + 在这里多一段装配。
package main

import (
	"context"
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// syncCodeartsAccounts 把 codearts 凭证目录里的账号并入核心账号池。
//
// # 为什么需要 secret 通道
//
// 池子存的是 *auth.Auth（workbuddy 的凭证类型）—— 那是核心唯一认识的凭证。
// codearts 的凭证是 *codearts.Auth（AK/SK/DPoP 私钥），核心**不得**认识它
// （判据 3：pool 不得依赖任何上游包）。
//
// 于是走 pool 提供的**不透明 any 通道**：
//
//	池子保管：SyncToDirWithSecrets 的 secrets 参数
//	取回使用：pool.SecretOf(uid) → 调用方断言回 *codearts.Auth
//
// 池子全程只搬不读，因此它对 codearts 一无所知，耦合为零。
//
// # 投影规则
//
// 核心只需要 uid（主键）与 nickname（展示），其余字段一律留在 secret 里。
// 这就是"核心不解释上游凭证"的具体体现。
//
// 返回并入的账号数（供启动日志）。
func syncCodeartsAccounts(p *pool.Pool, authDir string) int {
	list, err := codearts.LoadDir(authDir)
	if err != nil {
		log.Printf("codearts: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		// 目录里没有可用凭证不是错误：用户可能刚起网关、还没跑 cmd/login。
		// 这里仍然对齐一次（把已删除的账号剔掉），但不报错、不刷日志。
		p.SyncToDirWithSecrets(codearts.ProviderID, nil, nil)
		return 0
	}

	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, ca := range list {
		if ca == nil || ca.UID == "" {
			continue
		}
		// 上游凭证本身作为 secret 交给池子保管（不透明，池子不读它的字段）。
		secrets[ca.UID] = ca
		auths = append(auths, &auth.Auth{UID: ca.UID, Nickname: ca.Nickname})
	}
	p.SyncToDirWithSecrets(codearts.ProviderID, auths, secrets)
	return len(auths)
}

// registryRouter 把 gateway.Registry 适配成出口层要的 server.ProviderRouter。
//
// # 为什么是适配器而不是直接传 Registry
//
// 出口层声明的接口（Has/Default/Models）与 Registry 的方法集（Get/IDs/First/All）
// 并不重合，且出口层多要一件事：**"该上游现在能不能给出目录"**。
// 这个判断需要 Registry + 账号池两边的信息，只有装配层同时握有它们，
// 所以把它实现在这里是最合适的 —— 出口层因此不必认识 pool。
//
// ⚠ 类型必须是**具名**的，且方法签名必须与出口层声明的接口**逐字一致**：
// 接口断言要求方法集精确匹配，签名差一点就静默失败
// （见 tasks/plan.md 决策 E 的实测教训）。
type registryRouter struct {
	reg *gateway.Registry
	// p 账号池：用来判断某个上游现在有没有可用的号。
	// Models 在没号时直接返回 ok=false，避免发一次注定失败的请求。
	p *pool.Pool
}

// Has 报告该 ID 是否已注册。
func (r registryRouter) Has(id string) bool {
	_, ok := r.reg.Get(id)
	return ok
}

// Default 返回默认上游（注册顺序里的第一个）。
func (r registryRouter) Default() string {
	if id, ok := r.reg.First(); ok {
		return id
	}
	return ""
}

// IDs 返回全部已注册上游（供出口层枚举目录）。
//
// 这是出口层的**可选**能力：它用接口断言发现它
// （见 server.providerIDs）。不需要枚举的实现可以不提供。
func (r registryRouter) IDs() []string { return r.reg.IDs() }

// Models 返回指定上游的模型目录。
//
// ok=false 的三类情形（调用方一律跳过该上游，不把整个 /v1/models 打成失败）：
//   - 该上游未注册；
//   - 该上游现在没有可用账号 —— 目录要靠账号去上游拉，没号必然拉不到，
//     直接返回 ok=false 省掉一次注定失败的往返；
//   - 该上游的 Models() 报错或返回空。
//
// 注意**默认上游**不走向这里：它的目录由出口层自己的动态缓存 + 静态回退表
// 提供（改造前就有的机制，行为要保持）。
func (r registryRouter) Models(ctx context.Context, id string) ([]gateway.ModelInfo, bool) {
	pv, ok := r.reg.Get(id)
	if !ok {
		return nil, false
	}
	if id != r.Default() && r.p != nil && len(r.p.AvailableUIDsFor(id)) == 0 {
		return nil, false
	}
	ms, err := pv.Models(ctx, gateway.Credential{Provider: id})
	if err != nil || len(ms) == 0 {
		return nil, false
	}
	return ms, true
}
