// dailycheckin_ext_test.go 跨上游全量签到扩展点（gateway.DailyCheckinExt）的契约测试。
//
// # 本文件要守住的三条性质
//
//  1. **每个有签到的上游都实现了它** —— 顶部那个「全部签到」的语义是
//     "触发**所有**上游"。漏一个上游不会报错：按钮照常启动、进度条照常
//     走完，只是那个上游的号一个都没签。这种"静默少做"正是本项目反复
//     吃过的形态（trae/lobsterai/qoder 的签到都曾这样整片缺失）。
//
//  2. **它与 DailyAction.AllURL 是同一段业务** —— 两个入口（卡片上的按钮、
//     顶部那个跨上游按钮）若各写一份遍历，差异没有任何测试会发现。
//     这里钉不住"是不是同一段代码"（那要读实现），但钉得住
//     "有没有资格被视为同一个动作"：报了 AllURL 就必须实现扩展点，
//     实现了扩展点就必须报 AllURL。
//
//  3. **报出来的 AllURL 真的挂着** —— 假按钮守卫的另一面（与
//     dailyaction_test.go 的 TestWorkbuddyActionURLsReallyExist 同一条）。
package gateway_test

import (
	"context"
	"testing"

	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/lobsterai"
	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/trae"
	"workbuddy2api/internal/workbuddy"
)

// checkinProviders 本轮**必须有**全量签到入口的上游。
//
// # 为什么是白名单而不是"凡是报了 checkin 动作的都算"
//
// 动作 ID 各不相同（checkin / trae-checkin / lobsterai-checkin /
// qoder-checkin / qodercn-checkin / welfare），靠 ID 匹配 "checkin"
// 子串去推断"这个上游该有全量签到"，会把 workbuddy 的 keepalive
// 之类的动作也卷进来 —— 那正是本项目反复出现的"用字符串猜语义"。
// 白名单贵在**显式**：加新上游时这里红一次，是提醒而不是障碍。
//
// ⚠ qoder 与 qodercn 是**两个 Provider 实例**（不同域名与 clientId），
// 两者都要在列的理由见 internal/qoder/checkin.go 的文件头注释。
func checkinProviders(t *testing.T) []gateway.Provider {
	t.Helper()
	return []gateway.Provider{
		workbuddy.NewWithConfig(workbuddy.Config{}),
		codearts.NewWithConfig(codearts.Config{}),
		trae.NewWithConfig(trae.Config{}),
		lobsterai.NewWithConfig(lobsterai.Config{}),
		qoder.NewWithConfig(qoder.Config{}),                       // 国际版
		qoder.NewWithConfig(qoder.Config{Product: qoder.QoderCN}), // 中国版（另一个服务）
	}
}

// TestCheckinExtImplemented 每个有签到的上游都实现了 DailyCheckinExt。
//
// # 漏了它的表现（这是本条要挡的）
//
// 顶部「全部签到」会照常回 202 {started:true}，任务跑完显示
// "成功 N · 已签到 M · 失败 0"，而漏掉的那一整个上游**一个号都没动** ——
// 没有任何错误，也没有任何计数能暴露它。
func TestCheckinExtImplemented(t *testing.T) {
	for _, p := range checkinProviders(t) {
		ext, ok := gateway.ExtOf[gateway.DailyCheckinExt](p)
		if !ok || ext == nil {
			t.Errorf("%s 没有实现 gateway.DailyCheckinExt —— "+
				"顶部「全部签到」会**跳过**它（不报错、不计失败），"+
				"表现为「点了全部签到，这个上游的号一个都没签」", p.ID())
		}
	}
}

// TestBulkActionImpliesCheckinExt 报了全量端点就必须有跨上游入口（反之亦然）。
//
// # 两个方向各自挡什么
//
//	有 AllURL 但没实现扩展点 → 卡片上的按钮能用、顶部那个扫不到它
//	                            （两级入口行为不一致，最难查的一类）
//	实现了扩展点但没报 AllURL → 顶部能签到、卡片上却没有「全部签到」按钮
//	                            （用户本轮要求的"每个上游都有"落空）
//
// 两者都是"一半能用"，而界面的两处入口各自都绿 —— 只有这条断言会红。
func TestBulkActionImpliesCheckinExt(t *testing.T) {
	for _, p := range checkinProviders(t) {
		_, hasExt := gateway.ExtOf[gateway.DailyCheckinExt](p)
		hasBulk := false
		if dae, ok := gateway.ExtOf[gateway.DailyActionExt](p); ok {
			for _, a := range gateway.SanitizeDailyActions(dae.DailyActions()) {
				if a.Batch && a.AllURL != "" {
					hasBulk = true
				}
			}
		}
		switch {
		case hasBulk && !hasExt:
			t.Errorf("%s 报了全量端点（Batch+AllURL）却没有实现 DailyCheckinExt —— "+
				"卡片上的「全部签到」能用，顶部那个跨上游的却扫不到它", p.ID())
		case hasExt && !hasBulk:
			t.Errorf("%s 实现了 DailyCheckinExt 却没有报全量端点 —— "+
				"顶部「全部签到」能签它，但它那张卡片上没有「全部签到」按钮", p.ID())
		}
	}
}

// TestCheckinExtNoSecrets 回执不得夹带凭证。
//
// 与 dailyaction_test.go 的 TestDailyActionsHaveNoSecretKeys 同一条判据：
// 跨上游签到的结果会汇总进 /admin/task 的响应体（前端直接展示），
// Detail 里若带上游原文就可能泄凭证 —— 而上游错误文案是**不受控**的。
func TestCheckinExtNoSecrets(t *testing.T) {
	secretish := []string{"eyJ", "Bearer ", "ak=", "sk="}
	for _, p := range checkinProviders(t) {
		ext, ok := gateway.ExtOf[gateway.DailyCheckinExt](p)
		if !ok || ext == nil {
			continue
		}
		rep := ext.CheckinAll(context.Background())
		for _, r := range rep.Results {
			for _, s := range secretish {
				if contains(r.Detail, s) {
					t.Errorf("%s 的签到回执 Detail 里出现 %q —— 它会进 /admin/task 展示给前端",
						p.ID(), s)
				}
			}
		}
	}
}

// TestCheckinExtEmptyProviderIsNotError 未接线时返回空结果而不是 panic。
//
// # 为什么这条重要
//
// 核心的 runCheckinAll 会**串行**遍历所有上游。一个上游 panic
// （池没接、目录不存在、客户端为 nil）会让后面几个上游全部拿不到结果 ——
// 而这发生在一个后台 goroutine 里，界面上只表现为"任务很快结束了"。
//
// 本条用**零值配置**的上游（不配任何凭证目录）验证这条路径：
// 那是所有上游在"刚装好还没加号"状态下的真实形态。
func TestCheckinExtEmptyProviderIsNotError(t *testing.T) {
	for _, p := range checkinProviders(t) {
		ext, ok := gateway.ExtOf[gateway.DailyCheckinExt](p)
		if !ok || ext == nil {
			continue
		}
		rep := ext.CheckinAll(context.Background())
		if len(rep.Results) != 0 {
			t.Errorf("%s 在零配置下返回了 %d 条结果 —— 它不该凭空造账号",
				p.ID(), len(rep.Results))
		}
	}
}

// contains 子串判断（避免为一行引入 strings 依赖到本表的可读性）。
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
