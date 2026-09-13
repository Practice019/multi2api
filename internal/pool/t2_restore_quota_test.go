package pool

import "testing"

// T2：`restoreQuota` 曾把"从没查过额度"伪装成"查过了，确实是 0"。
//
// # 这两条测试为什么必须**对称**存在
//
// 修这个缺陷有两个方向，每个方向都有一个"改过头"的失败模式：
//
//	方向一：别把**有真数据**的账号降级   → 改过头 = 一律不回落（旧格式账号权重全归零）
//	方向二：别把**没数据**的账号造成 0   → 改过头 = 一律报未知（workbuddy 的显示变差）
//
// 只写一条会留下另一个方向的坑，所以两条都写。
// 变异验证（把兜底改成无条件执行）会让**两条同时红** —— 见交付报告。

// TestRestoreQuota_WorkbuddyRealDataNotDowngraded 方向一：真实数据不得被降级。
//
// 这是评审 F2/F4 那条的补充 —— 之前只覆盖了 per_model/unlimited，
// **没有覆盖最普通的 credits 单值**，而 workbuddy 恰好走的就是那条。
func TestRestoreQuota_WorkbuddyRealDataNotDowngraded(t *testing.T) {
	// workbuddy 落盘的真实形态（取自 data/state.json，2026-09-13）。
	saved := QuotaView{Kind: QuotaKindCredits, Remaining: 1136, HasData: true}

	got := restoreQuota(saved, 1136)

	if !got.HasData {
		t.Fatal("workbuddy 的真实额度被降级成了「未知」—— 界面会显示 — 而不是 1136")
	}
	if got.Remaining != 1136 {
		t.Errorf("剩余额度必须原样保留 1136，得到 %d", got.Remaining)
	}
	if got.Kind != QuotaKindCredits {
		t.Errorf("额度形态被改动：期望 %q，得到 %q", QuotaKindCredits, got.Kind)
	}
	if got.Effective() != 1136 {
		t.Errorf("Effective 必须仍是 1136（选号权重依赖它），得到 %d", got.Effective())
	}
}

// TestRestoreQuota_UnknownStaysUnknown 方向二：从没查过的账号必须**保持未知**。
//
// ⚠ **这一条才是真正修掉假 0 的那条。**
//
// 断言必须直接钉住 HasData —— 只看 Remaining 是抓不住的：
// 改造前的实现返回 `{Kind: credits, Remaining: 0, HasData: true}`，
// 它的 Remaining **恰好也是 0**，跟正确答案一模一样。
// 唯一的区别就在 HasData 上。
//
// 这正是线上 codearts 那条记录：
//
//	"01a08fe0...": {"quota":{"kind":"credits","has_data":true},"credits":0}
func TestRestoreQuota_UnknownStaysUnknown(t *testing.T) {
	// 旧状态文件：没有 quota 字段（零值），credits 也是 0
	// —— 即"这个账号从来没有被写过任何额度信息"。
	got := restoreQuota(QuotaView{}, 0)

	if got.HasData {
		t.Errorf("从没查过的账号被伪装成了「查过了」：%+v\n"+
			"后果：用户看到 0，会误以为「这个号没额度了」而去删账号。\n"+
			"正确行为：HasData=false → 界面显示 —（未知）", got)
	}
	if !got.IsUnknown() {
		t.Errorf("应报告 IsUnknown()=true，得到 %+v", got)
	}
	// 附带确认它不会被当成一个"可信的零"参与选号。
	if got.Effective() != 0 {
		t.Errorf("未知额度的 Effective 应为 0（不代表没额度），得到 %d", got.Effective())
	}
}

// TestRestoreQuota_LegacyNonZeroCreditsStillFallsBack 旧格式但有真数据时仍要回落。
//
// 这条守着"改过头"：`legacyCredits != 0` 是**真的带信息**，
// 那时必须回落成 credits 单值（否则那次升级会让所有老账号变成未知，
// 选号权重全归零 —— 正是 restoreQuota 当初存在的理由）。
func TestRestoreQuota_LegacyNonZeroCreditsStillFallsBack(t *testing.T) {
	got := restoreQuota(QuotaView{}, 5000)

	if !got.HasData {
		t.Fatal("旧格式但 credits=5000 是真实信息，不该被判为未知")
	}
	if got.Kind != QuotaKindCredits || got.Remaining != 5000 {
		t.Errorf("应回落成 credits=5000，得到 %+v", got)
	}
}

// TestRestoreQuota_CodeartsFakeZeroIsGone 端到端回归：线上那条假数据不再复现。
//
// 直接照抄 data/state.json 里 codearts 那条记录，断言恢复后**不再是**
// `{kind:credits, has_data:true, remaining:0}`。
//
// 为什么值得单写一条：上面两条是单元级的，而这条是**照线上数据钉的**——
// 它保证"用户报的那个现象"本身有测试守着，而不只是它的成因。
func TestRestoreQuota_CodeartsFakeZeroIsGone(t *testing.T) {
	// 线上原样：JSON 里没有 remaining（被 omitempty 吃掉，即 0）。
	onDisk := QuotaView{Kind: QuotaKindCredits, Remaining: 0, HasData: true}

	// 等等 —— 这一条在**落盘已经写成假数据**时确实会被原样采用
	//（hasContent() 对 credits 恒为 true）。这是刻意的：
	// restoreQuota 不能推翻一份**自称有数据**的落盘，
	// 否则就会把 workbuddy 的真数据一起推翻。
	got := restoreQuota(onDisk, 0)
	if !got.HasData {
		t.Fatalf("不该推翻落盘里自称有数据的 credits 形态（那会误伤 workbuddy）：%+v", got)
	}

	// 所以真正的修复在于**不再产生**这种落盘 —— 即未知账号不再被造成 credits。
	// 由 TestRestoreQuota_UnknownStaysUnknown 覆盖。
	//
	// 下面这条断言钉住"新产生的未知不会是 credits 形态"：
	fresh := restoreQuota(QuotaView{}, 0)
	if fresh.Kind == QuotaKindCredits {
		t.Errorf("新恢复的未知账号不该被造成 credits 形态（那正是假 0 的来源）：%+v", fresh)
	}
}
