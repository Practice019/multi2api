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
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// syncCodeartsAccounts 已迁到 codeartscreds.go。
//
// 它现在从 **codeartsCredStore** 取对象（而不是自己 LoadDir），
// 因为池 secret 必须与后台续期任务、管理端点共享**同一个** *codearts.Auth：
// 否则对象级 refreshMu 跨对象失效，续期写回打不到池子，503 必然复发。
// 详见 codeartscreds.go 的文件头。

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
	picked, uids := codeartsWinners(list)
	auths := make([]*auth.Auth, 0, len(picked))
	secrets := make(map[string]any, len(picked))

	for i, ca := range picked {
		// 上游凭证本身作为 secret 交给池子保管（不透明，池子不读它的字段）。
		secrets[uids[i]] = ca
		// 核心只需要 uid（主键）与 nickname（展示），其余字段一律留在 secret 里。
		auths = append(auths, &auth.Auth{UID: uids[i], Nickname: ca.Nickname})
	}
	return auths, secrets
}

// codeartsWinners 按 uid 聚合**一次目录扫描**的结果，每个 uid 选一份胜出凭证。
//
// 返回的 picked 与 uids 一一对应，顺序 = uid 在 list 里**首次出现**的顺序
// （不直接遍历 winner map —— map 遍历顺序随机，会让池子内容与日志不可复现）。
//
// # 为什么要从 dedupeCodeartsByUID 里抽出来
//
// 因为**胜出判据只允许有一份实现**。codeartsCredStore 也必须按 uid 裁决
// "哪份凭证胜出"（同一 uid 可能有多份文件），若它另写一套判据，
// store 里的对象与并池时选中的 secret 迟早分叉 —— 那是同一类 bug 的另一个入口。
//
// 这里只负责遍历、计数与记日志；"哪份胜出"的全部规则就是 betterCodeartsCred。
func codeartsWinners(list []*codearts.Auth) (picked []*codearts.Auth, uids []string) {
	// winner 记录每个 uid 当前胜出的那份凭证，供后续同 uid 的候选比较。
	winner := make(map[string]*codearts.Auth, len(list))
	// 各 uid 的候选文件数，用于冲突日志里说明"有 N 份"。
	count := make(map[string]int, len(list))
	// uids 按 uid **首次出现**的顺序保存，保证输出顺序与输入顺序一致
	// （不直接遍历 winner map —— map 遍历顺序随机，会让日志和池子内容不可复现）。
	uids = make([]string, 0, len(list))

	for _, ca := range list {
		if ca == nil || ca.UID == "" {
			continue
		}
		count[ca.UID]++
		cur, exists := winner[ca.UID]
		if !exists {
			winner[ca.UID] = ca
			uids = append(uids, ca.UID)
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

	picked = make([]*codearts.Auth, 0, len(uids))
	for _, uid := range uids {
		picked = append(picked, winner[uid])
	}
	return picked, uids
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
//
// # 为什么必须从池里取凭证再传进去（T1 修的就是这里）
//
// 早先这里是 `pv.Models(ctx, gateway.Credential{Provider: id})` ——
// **只填 Provider，Secret 是 nil**。
//
// 对 workbuddy 那类「Models() 不需要凭证」的实现这是巧合成立的；
// 但 codearts.Provider.Models() 第一件事是 authOf(cred)，它对 nil Secret
// 直接返回错误：
//
//	codearts: 凭证为空（Credential.Secret 未设置）
//
// 于是 router 拿到 err != nil → ok=false → 出口层 `continue` →
// **codearts 的模型被静默跳过**。而它的 Models() 是静态表、根本不需要网络，
// 实测能给出 7 条 —— 只是从来没人给它一份凭证。
//
// 现象因此极度难查：账号在池里、CapModels 已声明、RefreshModels 也返回成功，
// 只有 /v1/models 里少了一批 id，且**一个错误都不报**。
//
// 修法：按上游从池里取一个可用账号，把它的**不透明 secret** 装进 Credential
// （pool.SecretOf → any → Credential.Secret）。池子只搬不读，所以这里
// 仍然不认识任何具体上游 —— 装配层本来就是「唯一同时认识核心与上游的地方」，
// 凭证装配放在这一层正是它的职责。
//
// 取号失败（没号 / 没 secret）时回落到旧的裸 Credential 形态：
// 那让「需要凭证的上游跳过」与「不需要凭证的上游照常工作」两者都成立，
// 不会因为取不到 secret 就把 workbuddy 的目录一起弄丢。
func (r registryRouter) Models(ctx context.Context, id string) ([]gateway.ModelInfo, bool) {
	pv, ok := r.reg.Get(id)
	if !ok {
		return nil, false
	}
	if id != r.Default() && r.p != nil && len(r.p.AvailableUIDsFor(id)) == 0 {
		return nil, false
	}
	ms, err := pv.Models(ctx, r.credentialFor(id))
	if err != nil || len(ms) == 0 {
		return nil, false
	}
	return ms, true
}

// credentialFor 为该上游组装一份**带凭证**的 Credential。
//
// 凭证来源是账号池上那份不透明 secret（pool.SecretOf）—— 它由
// SyncToDirWithSecrets 在启动时装载（codearts 的真实 AK/SK/DPoP 就在里面）。
//
// # 为什么按 UID 排序取第一个，而不是问池子"给我一个号"
//
// AvailableUIDsFor 已经按 UID 排序（稳定输出）。这里只需要**任一个**可用账号
// 来把凭证带过去 —— 静态目录对所有账号一致，不需要也不应该走 pick 的
// 加权随机（那会记录 lastUsed、影响真实流量的选号分布）。
//
// # 为什么取不到就回落
//
// 裸 Credential 对「不需要凭证的实现」是正确输入。回落保证这条路
// 在池里没有 secret 时也不会把别的上游一起拖坏 —— 出口层对每个上游
// 独立调用本方法，任何一家的失败都只影响它自己。
func (r registryRouter) credentialFor(id string) gateway.Credential {
	cred := gateway.Credential{Provider: id}
	if r.p == nil {
		return cred
	}
	for _, uid := range r.p.AvailableUIDsFor(id) {
		if secret, ok := r.p.SecretOf(uid); ok && secret != nil {
			cred.UID = uid
			cred.Secret = secret
			return cred
		}
	}
	return cred
}

// Credential 为**指定的那个账号**组装一份带凭证的 Credential（出站路径专用）。
//
// # 与 credentialFor 的关键区别
//
//	credentialFor(id)          —— 目录路径：任取一个号即可（静态目录人人一样）
//	Credential(id, uid)        —— 出站路径：**必须是调用方选中的那个号**
//
// 出站循环已经用 `Pool.PickFor(provider, model)` 选好了号并 `Acquire` 了
// 在途名额。这里若自作主张另选一个号，会让「被选中的号」与「真正发请求的号」
// 分叉：名额占在 A 上、请求发在 B 上、失败惩罚记在 A 上 ——
// 三者对不上会让整套选号/额度/熔断统计全部失真。
//
// ok=false 的情形（出口层一律视为「服务端暂时发不出这个号」→ 换号）：
//   - 该上游未注册；
//   - 账号不在池里（状态文件与 auths 目录不同步）；
//   - **该账号不属于这个上游**（见下，P1 真漏网）；
//   - **默认上游没有私有 secret**（见下，这是绝大多数部署的实际形态）。
//
// # ⚠ 归属校验：uid 必须真的属于 id 那个上游（P1 真漏网）
//
// 改造前这里只校验两件事：上游已注册、uid 在池里。**它不校验 uid 是否属于
// 该上游** —— 而这是"粘性把别家的号塞进来"能一路走到底的最后一环：
//
//  1. 客户端发 {"model":"codearts/GLM-5.2","metadata":{"conversation_id":"K"}}
//     → Session.Bind("K", <codearts uid>)（并镜像到 Redis）
//  2. 同一 conversation_id=K 下次改发 {"model":"glm-5.2"}（裸名 → 默认上游）
//  3. 粘性命中那个 codearts uid（handler 侧已被 PickByUIDFor 拦住，
//     但**任何别的调用方**拿着这个 uid 问 workbuddy 要凭证都到这里）
//  4. Credential("workbuddy", <codearts uid>) 交出 Secret = *codearts.Auth
//  5. workbuddy 的 authOf 断言 *auth.Auth 失败 → 报错
//     → **惩罚一个无辜的 codearts 账号**（记错/冷却），真正的错在路由
//
// 池子本来就按 provider 分域（byUID 里的每个 entry 都带 provider 标签，
// 见 providerOf），所以"这个号属于谁"在池内是**已知事实**，只是没有只读
// 出口让它被问出来 —— 现在用 pool.ProviderOf。
//
// 判据与 PickByUIDFor / AvailableUIDsFor / PickFor 用的是同一个
// providerOf + normalizeProvider 组合，四处的归属口径不可能分叉。
//
// **效果**：把"粘性把别家号塞进来"降级成**一次换号**
// （ok=false → 出口层 errNoProviderCredential → 换号重试），
// 而不是静默的凭证类型错误 + 一个无辜账号被罚。
// 这是**独立于粘性域对齐的第二道防线**：即使将来再有人写一条新的调用路径
// 绕过 PickByUIDFor，凭证装配点自己也拦得住。
//
// # ⚠ 默认上游为什么必须回落（这是端到端实测抓出来的，不是预防性设计）
//
// 池子里有一个**部署形态上的不对称**：
//
//	codearts  用 SyncToDirWithSecrets 入池 → e.secret = *codearts.Auth
//	workbuddy 用 SyncToDir 入池（main.go:70，改造前就有的调用）→ e.secret = nil
//
// 后者的"私有凭证"就是 `*auth.Auth` **本身** —— 池子在 main.go 的
// loadOrRestore 路径上已经把明文凭证（AccessToken/RefreshToken/FilePath）
// 直接装进了 e.a，所以**不需要**再走 secret 通道。
// 换句话说：默认上游的 secret 通道**本来就是空的**，那是正常形态。
//
// 第一版这里写的是 `if secret == nil { return ok=false }`，于是：
//
//	裸模型名 "glm-5.3" → 选中 workbuddy 的号 → Credential 返回 ok=false
//	→ 出口层报 errNoProviderCredential → 换号 → 三个号全换完 → 503
//
// 实测：**改造前能用的 workbuddy 路径被整个打挂**
// （`503 ... 多上游模式下取不到该账号的凭证`）。这是"修好新的、弄坏旧的"的典型。
//
// 修法：secret 为 nil 时回落到 **e.a（*auth.Auth 本体）** ——
// 它正是 workbuddy.Provider 的 authOf 期望的类型（断言 `*auth.Auth`）。
// 这样两类上游拿到的东西都对了：
//
//	codearts  拿到 *codearts.Auth（它自己的类型）
//	workbuddy 拿到 *auth.Auth（它自己的类型）
//
// 而且这个回落**不引入任何新耦合**：*auth.Auth 是核心自己的通用凭证类型，
// 装配层把它交给上游本来就是既有机制（改造前 `cfg.Upstream.ChatStream(acct)`
// 传的就是它）。出口层仍然不读 Secret。
func (r registryRouter) Credential(id, uid string) (gateway.Credential, bool) {
	if _, ok := r.reg.Get(id); !ok {
		return gateway.Credential{}, false
	}
	if r.p == nil {
		return gateway.Credential{}, false
	}
	a := r.p.AuthByUID(uid)
	if a == nil {
		return gateway.Credential{}, false
	}
	// 归属校验：uid 必须真的属于 id 这个上游。
	//
	// ⚠ 默认上游的归一：池子里 uid 可能没打 provider 标签，此时 providerOf
	// 返回 p.defaultProvider —— 正是 normalizeProvider("") 的语义，因此
	// `id == ""`（调用方没有上游上下文）与显式默认上游都走得通。
	// 这里刻意**不做** id == "" 的短路放行：那会让单上游部署退回无校验，
	// 而单上游部署里 defaultProvider 与唯一的 provider 相等，校验自然为真。
	if got, ok := r.p.ProviderOf(uid); !ok || got != id {
		return gateway.Credential{}, false
	}

	cred := gateway.Credential{Provider: id, UID: uid, Nickname: a.Nickname}
	if a.ExpiresAt > 0 {
		cred.ExpiresAt = time.Unix(a.ExpiresAt, 0)
	}
	// 上游私有凭证优先；没有就回落到通用凭证本身（默认上游的正常形态）。
	if secret, ok := r.p.SecretOf(uid); ok && secret != nil {
		cred.Secret = secret
	} else {
		cred.Secret = a
	}
	return cred, true
}

// RefreshCredential 用**该上游自己的**实现续期一份凭证。
//
// # 为什么分派在装配层而不是出口层
//
// 出口层不认识任何具体上游（判据 1），它只会按 ID 问。而「这个 ID 对应哪个
// 实例」以及「那个实例会不会续期」是装配层的事实 —— 这里正是
// 「唯一同时认识核心与所有具体上游的地方」。
//
// # 为什么用 ExtOf 而不是让 Provider 接口多一个方法
//
// 续期不是每个上游都有的事实：纯 API Key 的上游没有凭证生命周期。
// 塞进 Provider 会逼所有人写一个空实现（本仓已验证的模式：
// AdminExt / JobExt / LoginFlow / AuthDirExt / CredentialLoader 都是
// `gateway.ExtOf[T]` 类型断言）。
//
// # ok=false 且 err=nil 的语义（⚠ 出口层依赖它）
//
// 「这个上游没有续期实现」= **它的凭证不需要刷新**，出口层据此**跳过刷新**
// 直接用现有凭证发请求。把它当成失败会让这类上游每次请求都白换一次号。
func (r registryRouter) RefreshCredential(_ context.Context, id string, cred gateway.Credential) (bool, error) {
	pv, ok := r.reg.Get(id)
	if !ok {
		return false, nil
	}
	fr, ok := gateway.ExtOf[gateway.CredentialRefresher](pv)
	if !ok {
		return false, nil
	}
	if err := fr.RefreshCredential(cred); err != nil {
		return true, err
	}
	return true, nil
}

// Classify 用**该上游自己的**错误分类器判定 (status, body) 的错误类别。
//
// # 为什么分类也必须由装配层分派（P2 缺口的落点）
//
// 出站循环原先在两条上游的响应上**都**调 `upstream.Classify`
// —— 那是 workbuddy 的分类器。它的判据里有两批上游专有的事实：
//
//	hardMarkers        = ["insufficient credit", "no credit", "quota exceeded", ...]
//	sessionDeadMarkers = ["Offline user session not found", "12153"]
//
// 拿它判 codearts 的响应体 → 两条真危害：
//
//	① 额度漏判：无一条匹配 codearts 的 "insufficient quota" → 额度耗尽被忽略
//	② 反向误伤：body 含裸数字 "12153" → session_dead → **永久禁用健康的号**
//
// 与 `CredentialRefresher` / `RefreshSkewExt` 用同一个模式（ExtOf 类型断言）：
// 没有实现 `gateway.ErrorClassifier` 的上游**不算错**，
// 返回 ok=false，核心回落到 `upstream.Classify`（默认上游的判据，
// 也正是改造前的行为 —— 不是通用猜测）。
//
// # 类型边界
//
// 出口层不认识任何具体上游，只认识 `gateway.ErrorKind`（中立类型）。
// 各上游在自己的包里把自己的 ErrKind 翻译过来：
//
//	internal/workbuddy  → toGatewayKind(upstream.Classify(...))
//	internal/codearts   → 先 DetectQuotaExhausted，再 toGatewayKind(Classify(...))
//
// 装配层只做"取出该上游的实现并调用"，不做任何翻译 ——
// 翻译是上游的事实（每家的错误码表不同）。
func (r registryRouter) Classify(id string, status int, body string) (gateway.ErrorKind, bool) {
	pv, ok := r.reg.Get(id)
	if !ok {
		return gateway.ErrKindNone, false
	}
	ext, ok := gateway.ExtOf[gateway.ErrorClassifier](pv)
	if !ok {
		return gateway.ErrKindNone, false
	}
	return ext.Classify(status, body), true
}

// RefreshSkew 问**该上游自己的**提前续期窗口。
//
// # 为什么"要不要刷"也必须在装配层分派
//
// `RefreshCredential` 把"**怎么**刷"分派出去了，但"**要不要**刷"原先留在核心：
// 出站循环用 `cfg.RefreshSkew`（默认 10m）替所有上游回答。而这是上游的事实：
//
//	workbuddy → token 寿命以小时计 → 10m
//	codearts  → STS 仅约 30m     → 3m
//
// 对 codearts，10m 的后果是**太早**：剩 8m 就被核心判为该刷，而 codearts 的
// CredentialRefresher 内部没有自己的 skew 检查 → 真的去消费那个**一次性**的
// refresh_token。为省一次 401 往返烧掉一个凭证。
//
// 与 CredentialRefresher 用同一个模式（ExtOf 类型断言）：没有实现该扩展点的
// 上游**不算错**，返回 ok=false，核心回落到自己的通用兜底。
func (r registryRouter) RefreshSkew(id string, cred gateway.Credential) (time.Duration, bool) {
	pv, ok := r.reg.Get(id)
	if !ok {
		return 0, false
	}
	ext, ok := gateway.ExtOf[gateway.RefreshSkewExt](pv)
	if !ok {
		return 0, false
	}
	return ext.RefreshSkew(cred)
}

// ResetAt 问**该上游自己的**"额度耗尽的号什么时候能再用"。
//
// # 为什么这个能力也必须在装配层分派（P2 设计缺口）
//
// 与 RefreshSkew 同一形状的缺口，但断在**类型**上：出口层的
// `Config.NextResetAt` 早先是 `func() time.Time` —— 没有参数。
// 签名不允许按上游分派，于是它只能返回装配层注入的那**一个**时刻
// （workbuddy 的次日 04:00），所有上游共用。
//
// 后果对 codearts 是实打实的：它**没有签到恢复机制**，
// 「次日 04:00」不是它的任何事实 ——
//
//	明明已恢复却还冷到次日凌晨 → 白白闲置近 24h
//	按 04:00 解冻而实际未恢复   → 又撞一次硬错误
//
// 现在出口层按 ID 问（ProviderRouter.ResetAt），本函数把
// 「ID → 那个实例的排程」翻译出来。出口层仍然不认识任何具体上游。
//
// 与 CredentialRefresher / RefreshSkewExt / ErrorClassifier 用同一个模式
// （ExtOf 类型断言）：没有实现 gateway.ResetPolicyExt 的上游**不算错**，
// 返回 ok=false，核心回落到自己的通用保守值（now+1h）。
//
// ⚠ 刻意**不**回落到 `r.reg.First()`（默认上游）的排程：那正是本 bug 的形态 ——
// 拿默认上游的事实去回答另一个上游的问题。ok=false 是唯一正确的回答。
func (r registryRouter) ResetAt(id string, cred gateway.Credential) (time.Time, bool) {
	pv, ok := r.reg.Get(id)
	if !ok {
		return time.Time{}, false
	}
	ext, ok := gateway.ExtOf[gateway.ResetPolicyExt](pv)
	if !ok {
		return time.Time{}, false
	}
	return ext.ResetAt(cred)
}

// Chat 用**指定上游自己的 Provider** 发一次对话。
//
// # 为什么必须由装配层做（本次修的 bug 的落点）
//
// 出口层只有 ID。把 ID 换成「能发请求的那个实例」需要 Registry，
// 而 Registry 就在本层。出口层因此永远不需要 import 任何具体上游。
//
// ok=false 表示该上游未注册 / 实例缺失 —— 出口层按「服务端暂时发不出去」
// 处理（换号、最终 503），而不是当成上游返回的业务错误。
func (r registryRouter) Chat(ctx context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error) {
	pv, ok := r.reg.Get(id)
	if !ok {
		return gateway.ChatStream{}, false, nil
	}
	cs, err := pv.Chat(ctx, cred, body)
	return cs, true, err
}
