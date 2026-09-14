// pool_reconcile.go 装配层对账：把「本次启动没有注册的上游」的账号从池中逐出。
//
// # 这个文件在修什么（用户实测的事故）
//
// 账号池是**持久化**的：data/state.json 里记着账号，另有 Redis 快照。
// 用户先用一份启用了 codearts 的 config 跑过（auths/codearts/ 下 2 份凭证
// 并入池子并落了盘），随后改用一份**没有 codearts 段**的 config 启动 ——
// codearts.enabled 缺省 false，上游根本不注册。
//
// 但恢复路径（Pool.RestoreFromSnapshot / Pool.load）只认 state.json：
// 它照样把那 2 个 codearts 账号装回池子。后果有两条：
//
//	表面：/admin/accounts 返回 5 个账号（其中 2 个 provider="codearts"），
//	      而 /admin/ui/manifest 的 providers 只有 workbuddy ——
//	      前端账号池里冒出一个 manifest 里没注册的上游分组，
//	      标题只能写「按默认上游推断（未在 manifest 里注册）」。
//	实害：这 2 个号永远选得到，却没有任何上游能服务它们、也没有续期任务
//	      （codearts 的 STS 只有约 2 小时寿命）—— 请求必然失败，
//	      而且用户完全看不出"为什么号在界面里却不能用"。
//
// # 修法：装配层做一次对账，而不是让 pool 认识上游
//
// pool 不得知道任何具体上游名（判据 1/3），"本次注册了谁"是**装配层的
// 事实**（registry.IDs()）。所以对账放在 cmd/server：装配层把这份事实
// 交给下面这个纯函数，由它逐出"池里出现过、但本次没注册"的上游。
//
// # 不变量：凭证文件不动
//
// SyncToDirWithSecrets(prov, nil, nil) 的语义正是"这个上游在池中应为空集"，
// 它只删 prov 这一个上游的账号并落盘，**不碰磁盘上的凭证文件**。
// auths/codearts/ 下那 2 份凭证原样留着，重新启用该上游后会被重新并入。
package main

import "workbuddy2api/internal/pool"

// pruneUnregisteredProviders 把池中「本次启动没有注册的上游」的账号逐出。
// 返回被逐出的上游数与账号数（供调用方打日志）。
//
// known 是本次启动**真正注册**的上游 id 列表（调用方传 registry.IDs()）。
// logf 只用来报告"为什么号不见了"，判断逻辑不依赖它。
//
// # 调用点必须在 SetDefaultProvider 之后（理由见下，与空串守卫是一对）
//
// Pool.Providers() 返回的是**生效**标识（pool.providerOf）：没打 provider
// 标签的历史账号会回落成默认上游。默认上游还没设时它们解析成空串 ""，
// 而空串必然不在 known 里 → 下面会把"所有没有标签的号"当成未注册上游整批删掉，
// 且 SyncToDirWithSecrets("", nil, nil) 归一后的 want 也正好是 ""，
// 一个不漏地命中删除。所以顺序是先 SetDefaultProvider 再对账。
func pruneUnregisteredProviders(p *pool.Pool, known []string, logf func(format string, args ...any)) (providers, accounts int) {
	// 防御性：拿不到注册表时**宁可不动池子**。
	//
	// 若这里继续走下去，"所有上游都不在 known 里"会把整个账号池清空 ——
	// 一次装配失误（例如 registry 提前构造失败、IDs() 返回空）就能让用户
	// 的所有账号消失，而这是完全不可逆的观感事故。空 known 直接返回。
	if len(known) == 0 {
		return 0, 0
	}
	allow := make(map[string]bool, len(known))
	for _, id := range known {
		allow[id] = true
	}

	// Providers() 已按字典序稳定输出，遍历顺序可预期、日志可复现。
	for _, prov := range p.Providers() {
		// 必须跳过空串（与调用顺序那条注释是一对，这里是**双保险**）。
		//
		// 空串不是任何上游的 id，它只会出现在一种情况：池里存在"没打 provider
		// 标签、且默认上游尚未注入"的账号。那批号是**合法资产**，
		// 绝不能因为"空串不在 known 里"就被当成未注册上游删掉 ——
		// 而且它一旦被删就是静默的：用户只看到"重启后账号池空了"。
		// 调用方保证顺序，这里再守一道，两道都破不了才会出事。
		if prov == "" {
			continue
		}
		if allow[prov] {
			continue // 本次注册过的上游：池子归它自己管（SyncToDir* 已对齐）
		}

		// 先数后删：SyncToDirWithSecrets 调用后 ListFor 就空了，
		// 逐出前的账号数只能在这之前拿，它同时也是"要不要打日志"的依据。
		n := len(p.ListFor(prov))
		// 语义正是"这个上游在池中应为空集"：只删 prov 一家、并落盘。
		p.SyncToDirWithSecrets(prov, nil, nil)
		providers++

		// 只有真的删掉了账号才记账、才打日志：
		// 池里出现过一个空上游（例如凭证目录刚被清空）时不该刷屏。
		if n > 0 {
			accounts += n
			if logf != nil {
				logf("账号池：上游 %q 本次启动未注册，已从池中移除 %d 个账号（凭证文件未改动，重新启用该上游后会自动并入）", prov, n)
			}
		}
	}
	return providers, accounts
}
