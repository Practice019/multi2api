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
	"path/filepath"

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
// 返回值是**实际生效的账号数**（按 uid 去重后），见 syncCodeartsAccounts 注释 S3。
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

	auths, secrets := dedupeCodeartsByUID(list)
	p.SyncToDirWithSecrets(codearts.ProviderID, auths, secrets)
	return len(auths)
}

// dedupeCodeartsByUID 把 LoadDir 的原始凭证列表按 uid 聚合，每个 uid 选**一份**凭证。
//
// # 为什么需要这个函数（S2）
//
// LoadDir 返回的是**文件维度**的列表：auths/ 里放 N 个文件就返回 N 条。
// 但账号池的主键是 **uid**（pool.byUID），它天然按 uid 去重。
//
// 这两者平时一致，但在**同一个华为云账号被授权两次**时会分叉：
// ParseCredential 的 uid 三级回落链优先取 refresh_token JWT 里的 account_id，
// 而那是**服务端给的权威账号 ID** —— 两次浏览器授权拿到的是同一个账号 ID，
// 于是两个不同 AK 的文件解析出**同一个 uid**。
//
// 这是**正确行为**，不是缺陷（判据：uid 表示"这是哪个账号"，不是"这是哪份凭证"）。
// 但旧的实现没有处理这个分叉，于是产生两个真问题：
//
//  1. auths 里有 2 条同 uid、secrets 里只有 1 份（map 键冲突，后写的胜出）。
//     两者长度不一致，池子按 2 条做 upsert，实际只生效 1 条 —— 白做一次。
//  2. **"哪份胜出"完全由 filepath.Glob 的字母序决定**。
//     两份都是好凭证时这无害；但当**一份已过期、一份有效**时，
//     胜出的可能是**过期那份** —— 而界面上一切正常（号在池里、昵称也对），
//     只有请求全部失败。这是最难查的一类故障：现象与原因之间没有任何提示。
//
// 所以冲突必须按**可用性判据**裁决，而不是按文件名的偶然顺序。
//
// # 判据（逐级短路，全平则保留先出现的以保证稳定可预期）
//
//  1. expiresAt 更大     —— 更晚过期 = 更可能还能用；这是最直接的可用性信号
//  2. 有 refresh_token   —— 能自动续期，过期只是暂时的
//  3. 有 DPoPPrivateKeyJWK —— codearts 续期**必需**私钥，缺它则连续期都发不出去
//  4. 全平 → 保留先出现的 —— 结果不随 map 遍历顺序漂移，可复现
//
// ⚠ 不用文件 mtime：复制/解压/同步文件都会改 mtime，它与凭证是否有效无关。
// 判据必须来自凭证**内容**里的字段，那才是真正影响"能不能发出请求"的东西。
//
// 返回的 auths 与 secrets 的 **uid 集合严格一致**（secrets 的每个键都在 auths 里出现一次，
// 且 auths 无重复 uid）—— 这是池子能正确 upsert 的前提。
func dedupeCodeartsByUID(list []*codearts.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))

	// winner 记录每个 uid 当前胜出的那份凭证，供后续同 uid 的候选比较。
	winner := make(map[string]*codearts.Auth, len(list))
	// 各 uid 的候选文件数，用于冲突日志里说明"有 N 份"。
	count := make(map[string]int, len(list))
	// winners 按 uid **首次出现**的顺序保存，保证输出顺序与输入顺序一致
	// （不直接遍历 winner map —— map 遍历顺序随机，会让日志和池子内容不可复现）。
	order := make([]string, 0, len(list))

	for _, ca := range list {
		if ca == nil || ca.UID == "" {
			continue
		}
		count[ca.UID]++
		cur, exists := winner[ca.UID]
		if !exists {
			winner[ca.UID] = ca
			order = append(order, ca.UID)
			continue
		}
		if betterCodeartsCred(ca, cur) {
			// 挑战者更好：它成为新的胜出者，旧的被丢弃。
			log.Printf("codearts: 账号 %s 有 %d 份凭证，选用 %s（更新/更完整），丢弃 %s",
				shortUID(ca.UID), count[ca.UID], baseOrPath(ca.FilePath), baseOrPath(cur.FilePath))
			winner[ca.UID] = ca
		} else {
			// 挑战者不如当前胜出者：丢弃挑战者。
			log.Printf("codearts: 账号 %s 有 %d 份凭证，选用 %s（更新/更完整），丢弃 %s",
				shortUID(ca.UID), count[ca.UID], baseOrPath(cur.FilePath), baseOrPath(ca.FilePath))
		}
	}

	for _, uid := range order {
		ca := winner[uid]
		// 上游凭证本身作为 secret 交给池子保管（不透明，池子不读它的字段）。
		secrets[uid] = ca
		// 核心只需要 uid（主键）与 nickname（展示），其余字段一律留在 secret 里。
		auths = append(auths, &auth.Auth{UID: uid, Nickname: ca.Nickname})
	}
	return auths, secrets
}

// betterCodeartsCred 报告 cand 是否**优于** cur（按上面的四级判据）。
//
// 独立成函数是为了让判据可以被单测直接钉住：它就是"哪份胜出"的全部规则。
func betterCodeartsCred(cand, cur *codearts.Auth) bool {
	// 判据 1：更晚过期。STS 凭证只有约 30 分钟寿命，"哪份更晚过期"
	// 几乎等价于"哪份还没被用过/刚续期过"。
	if cand.ExpiresAt != cur.ExpiresAt {
		return cand.ExpiresAt > cur.ExpiresAt
	}
	// 判据 2：有 refresh_token 的能自动续期，比没有的活得久。
	if (cand.RefreshToken != "") != (cur.RefreshToken != "") {
		return cand.RefreshToken != ""
	}
	// 判据 3：DPoP 私钥是 codearts 续期的**必需**项 ——
	// 没有它，即使有 refresh_token 也换不出新凭证。
	if len(cand.DPoPPrivateKeyJWK) != len(cur.DPoPPrivateKeyJWK) {
		return len(cand.DPoPPrivateKeyJWK) > 0
	}
	// 判据 4：全平 → 不当选。保留**先出现**的那份，
	// 使结果只取决于输入顺序（filepath.Glob 的字母序），不取决于 map 遍历的随机性。
	return false
}

// shortUID 把 uid 截成前 8 位用于日志。
//
// 华为云 account_id 是 32 位十六进制，整串打进日志会挤掉真正可读的信息；
// 前 8 位在同一台机器上足以区分不同账号。
func shortUID(uid string) string {
	const n = 8
	if len(uid) <= n {
		return uid
	}
	return uid[:n]
}

// baseOrPath 取文件名的 Base（日志里不打完整路径）。
//
// 取不到 FilePath 时（单测直接构造的内存凭证）不留空串，
// 否则日志会变成"丢弃 "后面什么都没有，反而看不出发生了什么。
func baseOrPath(fp string) string {
	if fp == "" {
		return "(内存凭证)"
	}
	return filepath.Base(fp)
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
