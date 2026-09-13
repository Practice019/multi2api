// quota_ext.go workbuddy 实现 gateway.QuotaExt —— "我的额度怎么取"。
//
// # 这个文件补的是哪个洞（本次要修的 bug）
//
// 账号池面板上的「刷新全部额度」按钮**刷不到 codearts**：前端把它绑在
// `/admin/credits/refresh`（workbuddy 私有 api）上，那是个全局按钮，
// 却只走本上游一条路。
//
// 核心的通用端点 `/admin/accounts/quota/refresh` → `refreshQuotas()`
// **按账号自己的 provider 去 registry 里找 `gateway.QuotaExt`**，
// 找到了才问额度。codearts 早已实现（internal/codearts/quota_ext.go），
// **workbuddy 没实现** —— 于是它落进 quota_refresh.go 的
// "该上游不报额度 → 保持未知，不写池" 分支，被**静默跳过**。
//
// 于是同一批账号在两个按钮下的表现完全不同，而界面上没有任何报错说
// "这个上游没接线"。本文件把这条线接上。
//
// # 与 codearts 那份的关系：同形，必须对称
//
// 两个上游的 QuotaExt 是对同一个契约的回答，形状必须一致
// （见 internal/codearts/quota_ext.go）。任何一处判据漂移，
// 表现为"某一家上游的额度忽然全是 `—`"或"忽然全是 0"，且不报错。
//
// # 为什么不新写一个"取余额"的函数，而是转调 RefreshCredits
//
// 因为 workbuddy 的余额取值路径**只有一条**：RefreshCredits
// （history.go:44）。它比本适配层多做两件事，而两件都不能省：
//
//	history.go:72  Pool.SetCredits —— 把余额写回池
//	history.go:74  Pool.ReenableIfUsable —— 按"查到了且 > 0"解冻
//
// 另起一条"只读余额"的路会让两条路各自演化：一条写池、一条不写，
// 于是"用哪个按钮刷的"决定了池里有没有值。转调则天然只有一份语义。
//
// # ⚠ 已知行为差异（本适配层比 codearts 那条路**更强**）
//
// 因为 RefreshCredits 内含 `ReenableIfUsable`，本适配层会**顺带解冻**账号。
// 而它挂载的端点 `/admin/accounts/quota/refresh` 的文档
// （quota_refresh.go:176）明说"不碰解冻（那需要上游回答能不能用，是另一件事）"。
//
// 也就是说：前端改接这个端点之后，「刷新全部额度」对 workbuddy 账号的
// 行为比 codearts 账号**多一步解冻**。
//
// 这是**可接受的已知差异**，理由是它的来源不是本文件 ——
// 旧按钮（/admin/credits/refresh）背后是**同一个** RefreshCredits，
// 所以这一步解冻在改造前后完全相同，用户看不到任何新行为。
// 反过来，若为了"对齐文档"而在适配层里绕开解冻，就等于给同一个业务
// 造出第二套语义（见上一条），那才是真正的坑。
package workbuddy

import (
	"log"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// quotaRefreshTrigger 本适配层刷新额度时写进任务历史的 trigger。
//
// # 为什么是 "manual"
//
// 这是**用户点按钮**触发的（/admin/accounts/quota/refresh 或
// /admin/credits/refresh），与定时任务不是一回事。
//
// # 为什么复用 triggerManual 而不是新造一个值
//
// 取值域由 checkinlog.Record.Trigger 的消费方决定。实测**没有任何消费方
// 按值过滤** —— webui.html:4380 只有二元显示：
//
//	trigger == "manual" → "手动"，其余一律 → "定时"
//
// 所以新造一个值（比如 "quota-ext"）不会更精确，只会落进 else 分支，
// 把一次人工点击显示成"定时" —— 那是在误导用户。
// 旧端点用的就是 "manual"（adminendpoints.go:228），保持一致。
// 测试里有 TestQuotaRefreshTriggerMatchesLegacyEndpoint 钉住这一点。
const quotaRefreshTrigger = triggerManual

// RefreshQuota 取该账号的当前额度（gateway.QuotaExt）。
//
// 返回 (额度视图, ok)：
//
//	ok=false          这条 uid 我服务不了（账号池未接线 / 号不存在）→ 调用方跳过
//	ok=true + 有数据  写回池
//	ok=true + 取不到  HasData=false → 调用方**不写池**，界面显示 `—`
//
// # 判定必须走三元组，不能只看 RefreshCredits 的第二个返回值
//
// 这是本文件最容易写错的一处，所以单独展开。
//
// RefreshCredits 的签名是 (CreditsResult, bool)（history.go:44），
// 而它第二个返回值的语义是 **"这条 uid 我认领了"**（能不能服务），
// **不是 "成功没成功"**。四类分支里两者恰好分叉：
//
//	history.go:46  Pool == nil     → (Fail, false)  我服务不了
//	history.go:49  !Pool.Has(uid)  → (Fail, false)  我服务不了
//	history.go:53  AuthByUID==nil  → (Fail, **true**)  ← 认领了，但失败
//	history.go:69  上游报错         → (Fail, **true**)   认领了，但失败
//	history.go:76  成功             → (OK,   **true**)
//
// 若直接把第二个返回值当 ok 用（`return UnknownQuota(), claimed`），
// 上面第 3、4 行就变成 `ok=false` —— 调用方（quota_refresh.go:105）
// 会把它记进 **Failed** 计数。那是错的分类：这个账号存在、也属于本上游，
// 只是**当下拿不到额度**，正确的归类是 Unknown（界面 `—`），不是错误。
//
// 所以这里按三个正交的问题依次判定：
//
//	claimed  —— 这条 uid 是不是我该服务的？（契约的 ok）
//	Status   —— 这一次取值成功了吗？（checkinlog.StatusOK）
//	HasQuota —— 上游真的给了额度数据吗？（对应 gateway.QuotaView.HasData）
//
// # 判据 a：HasQuota 必须来自 res.HasQuota，**不能**写成 res.Credits != 0
//
// 因为 `remain == 0` 时 Credits 是 0 而 HasQuota 是 true（history.go:71）。
// 那是"**查到了，确实是 0**" —— 界面必须显示 `0`。
//
// 若用 `Credits != 0` 判，这一种会被误判成"取不到"，界面显示 `—`，
// 于是用户以为额度未知而继续用这个号 —— **比显示 0 更糟**。
//
// 这与 codearts 的 quota_ext.go:89 用 `sub.Status == ""` 判"上游没给数据"
// 是**同一个不变式**：**"上游没给数据 ≠ 数据是 0"**。两边必须一致。
//
// # ⚠ 这里**不**重复写池
//
// RefreshCredits 内部已经写池（history.go:72 SetCredits + :74 ReenableIfUsable），
// 本适配层**只读结果**。调用方 core 会在 quota_refresh.go:113 再 SetQuota 一次 ——
// 我核实过两者落点一致（都是 e.quota + e.credits），**幂等**：
//
//	pool.SetCredits  → e.credits + e.quota{kind:credits, remaining, has_data:true}
//	pool.SetQuota    → e.quota{kind:credits, remaining, has_data:true}
//
// 所以在适配层里再写一次没有意义（写的是同一组值），只会让"谁负责写池"
// 这个问题多出一个答案。
//
// # 为什么失败**只记日志**、不返回 error
//
// 与 codearts 同理由：额度刷新是**展示性**操作，不是业务路径。
// 它失败不该让 /admin/accounts 报错，也不该刷错误日志把真正的故障淹掉。
// 契约把 error 留给"调用方需要知道并处理"的情况，这里不需要
// （RefreshCredits 连 Detail 都替我们写好了，见 history.go:58）。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	res, claimed := p.RefreshCredits(uid, quotaRefreshTrigger)

	// 契约里的 ok=false = "这条 uid 服务不了"（池未接线 / 号不存在）。
	// 调用方据此跳过或回 404，而不是把一个"问不到"当成"额度是 0"。
	if !claimed {
		return gateway.UnknownQuota(), false
	}

	// 认领了但没成功（上游报错、账号没有可用凭证）。
	//
	// ⚠ **必须 ok=true**：我服务这个号，只是当下拿不到额度。
	// 返回 false 会被调用方记成 Failed —— 那是把它当成了"不属于我的号"。
	//
	// 返回未知而不是 0：见 package 注释里那条不变式。
	if res.Status != checkinlog.StatusOK {
		log.Printf("workbuddy: 额度刷新未成功 uid=%s status=%s: %s（显示为未知，不填 0）",
			uid, res.Status, res.Detail)
		return gateway.UnknownQuota(), true
	}

	// 判据 a：HasQuota 是"上游给没给数据"的权威字段。
	// remain=0 时它是 true —— 那必须走下面一条，显示成 0。
	if !res.HasQuota {
		// 防御性分支，理论不可达：Status==OK 的那条 return 上
		// Credits 与 HasQuota 总是一起被赋值（history.go:71）。
		// 留在这里是因为"不可达"是今天的事实，不是契约 ——
		// 万一将来那条路径改成"OK 但没有套餐数据"，
		// 这里会安静地降级成未知，而不是发出一个假的 0。
		return gateway.UnknownQuota(), true
	}

	// 查到了，数额就是 res.Credits —— **哪怕是 0**。
	return gateway.CreditsQuota(res.Credits), true
}

// 编译期断言：Provider 实现额度扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"界面上额度一直空着"。
// 与 provider.go:311 那条 Provider 断言同一理由；
// codearts 也有一条逐字相同的（internal/codearts/quota_ext.go:112）。
var _ gateway.QuotaExt = (*Provider)(nil)
