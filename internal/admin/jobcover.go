package admin

// jobcover.go 任务覆盖检查：哪些上游**有能力**做每日签到 / 凭据续期，
// 但**没有注册**对应的定时任务。
//
// # 为什么需要它（真实故障）
//
// 2026-10-25 用户报「monkeycode 渠道没有设置任务调度，定时每日对每个账号签到」。
//
// 排查发现签到驱动 internal/dailycheckin **早就实现好了** —— 按钮能点、
// 历史会写、手动签到成功。但调度器发现任务的方式是构造期遍历注册表、
// 对每个 Provider 做 gateway.ExtOf[gateway.JobExt] 类型断言，**没实现就静默跳过**
// （scheduler/job.go 里那行是 continue，不是 error）。于是形态是：
//
//	编译通过 · 按钮可用 · 手动能签 · 但调度器根本不知道有这个任务
//
// 补上 Jobs() 就好了。但**下一个上游还会踩**：这是契约的一环没人守。
// cmd/server/jobs_pairing_test.go 有一条测试期守卫（实现了 CredentialRefresher
// 就必须实现 JobExt），但那要跑测试才看得见 —— 用户不会跑。
//
// 本文件把它变成**运行期可视化**：任务调度页直接列出每个上游的覆盖情况，
// 缺的标红。
//
// # 判据必须分三层（不能只看「有没有注册任务」）
//
//  ① 有能力吗 —— 编译期事实：
//       签到：Caps() 里有 CapCheckin
//       续期：实现了 gateway.CredentialRefresher
//  ② 注册了吗 —— 运行时事实：manifest.jobs 里有没有该上游的对应任务
//  ③ 配对正确吗 —— ①真 && ②假 ⇒ 报缺
//
// 只看 ② 是错的：绝大多数上游**本来就没有**签到能力，把它们都报成「缺」
// 会让页面全是红字，真正的缺口反而被噪声埋掉。

import (
	"strings"

	"workbuddy2api/internal/gateway"
)

// jobCoverage 一个上游的任务覆盖情况。
type jobCoverage struct {
	Provider string `json:"provider"`
	// CanCheckin 该上游**有能力**做每日签到（Caps 里有 CapCheckin）。
	CanCheckin bool `json:"can_checkin"`
	// HasCheckin 该上游**已注册**签到任务。
	HasCheckin bool `json:"has_checkin"`
	// CanRefresh 该上游**有能力**续期凭据（实现了 CredentialRefresher）。
	CanRefresh bool `json:"can_refresh"`
	// HasRefresh 该上游**已注册**续期任务。
	HasRefresh bool `json:"has_refresh"`
	// MissingCheckin 有能力但没注册 —— **这是要报警的**。
	MissingCheckin bool `json:"missing_checkin"`
	// MissingRefresh 有能力但没注册 —— **这是要报警的**。
	MissingRefresh bool `json:"missing_refresh"`
}

// jobKindOf 判断一个任务名属于哪一类。
//
// ⚠ 只能按**名字后缀**判断：gateway.Job 里没有类型字段
// （Name / Interval / Run / Due），manifest 的 uiJob 也没有。
//
// 这个判据是脆的 —— 上游可以给任务起任意名字。所以：
//
//	· 只认两种明确后缀，其余一律归为「其它」不参与覆盖检查；
//	· 返回 unknown 时**不报缺**（宁可漏报，也不要把正常的上游标成红字）；
//	· 这条脆弱性写在注释里，将来 Job 加了类型字段就该换掉这里。
func jobKindOf(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.HasSuffix(n, "-checkin"):
		return "checkin"
	case strings.HasSuffix(n, "-refresh"):
		return "refresh"
	}
	return ""
}

// jobCoverageOf 算出每个上游的任务覆盖情况。
//
// jobs 传 manifest 里已经收集好的任务（含 Provider 字段），
// registry 用来取能力位与接口断言。
func jobCoverageOf(providers []providerInfo, jobs []uiJob, registry *gateway.Registry) []jobCoverage {
	// 先把已注册任务按 provider 归组，避免对每个上游重复扫一遍任务表。
	has := map[string]map[string]bool{}
	for _, j := range jobs {
		k := jobKindOf(j.Name)
		if k == "" {
			continue
		}
		if has[j.Provider] == nil {
			has[j.Provider] = map[string]bool{}
		}
		has[j.Provider][k] = true
	}

	out := make([]jobCoverage, 0, len(providers))
	for _, p := range providers {
		c := jobCoverage{Provider: p.ID}
		// ① 能力：签到看能力位（providerInfo.Capabilities 已经是名字数组）。
		for _, n := range p.Capabilities {
			if n == gateway.String(gateway.CapCheckin) {
				c.CanCheckin = true
				break
			}
		}
		// ① 能力：续期看接口实现（编译期事实，与配置无关）。
		// ⚠ 这里必须走 registry 拿 Provider 本体做类型断言 ——
		//   providerInfo 里没有这个信息。
		if registry != nil {
			if raw, ok := registry.Get(p.ID); ok {
				if _, ok := gateway.ExtOf[gateway.CredentialRefresher](raw); ok {
					c.CanRefresh = true
				}
			}
		}
		// ② 注册：从任务表查。
		c.HasCheckin = has[p.ID]["checkin"]
		c.HasRefresh = has[p.ID]["refresh"]
		// ③ 配对：有能力但没注册才报缺。
		c.MissingCheckin = c.CanCheckin && !c.HasCheckin
		c.MissingRefresh = c.CanRefresh && !c.HasRefresh
		out = append(out, c)
	}
	return out
}
