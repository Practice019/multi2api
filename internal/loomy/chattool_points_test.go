// chattool_points_test.go 工具路径的免费额度扣减守卫。
//
// # 守的是什么（用户报的真缺陷）
//
// 免费额度优先的机制靠 noteConsumed 递减本地估计（见 freepool.go）。
// 但**只有一条路径**在喂它：provider.go 的 ChatStream 挂了 gateway.NewUsageTap。
// 而工具回执（生图 generate_image / 搜索 web_search）同样返回 points_consumed，
// 却只把数值塞进 Artifact 给客户端看 —— **没有**告诉池子。
//
// 后果：工具消耗了免费额度，池子不知道 ⇒ 那个号仍被当作"还有免费额度"优先选中
// ⇒ 继续烧**永久**额度。这正是用户说的"免费额度优先好像还是做不到"。
//
// # 为什么加源码级守卫
//
// 这一族坑已经踩过：新增一条计费路径、忘了喂池子，行为测试测不到（它只测已知路径），
// 而线上表现是"额度悄悄烧永久"。守卫枚举所有解析 points_consumed 的地方，
// 断言每一处都有配套的 noteConsumed（或显式白名单 + 理由）。
package loomy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── 行为测试 ────────────────────────────────────────────────────────────

// TestPointsOfParsesAllShapes 上游这个字段实测有三种形态：数字 / 浮点 / 字符串。
// 漏一种就等于静默不扣（扣减方向还可能反过来）。
func TestPointsOfParsesAllShapes(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{float64(1), 1, true},   // encoding/json 的默认数值类型
		{float64(2.0), 2, true}, // 1.0 这种
		{"3", 3, true},          // 字符串形态
		{"4.0", 4, true},
		{int64(5), 5, true},
		{int(6), 6, true},
		{nil, 0, false},   // 没这个字段 ⇒ 不知道，不是 0
		{"", 0, false},    // 空串 ⇒ 不知道
		{"abc", 0, false}, // 解析不了 ⇒ 不知道
	}
	for _, c := range cases {
		got, ok := pointsOf(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("pointsOf(%#v) = (%d, %v)，want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestToolConsumptionRemovesFromFreePool 工具消耗后，账号必须离开"还有免费额度"档。
//
// 这条直接复现用户报的场景：号在池子里 → 工具扣了 1 积分 → 池子该知道。
func TestToolConsumptionRemovesFromFreePool(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()
	po := p.pool()
	po.noteQuota("UID1", 1, true, now)   // 只剩 1 分免费额度
	po.noteQuota("UID2", 500, true, now) // ⚠ 必须有另一个"还有额度"的号
	// 否则 UID1 用完时**池子变空**，会命中 CandidateTiers 的 empty 分支
	//（「池子空了 = 全体放开」—— 那是用户明确要的规则），于是 UID1 仍返回
	// tierFree，看起来像"扣减没生效"。这不是 bug，是测试没造对场景。

	if got := p.CandidateTiers("", []string{"UID1", "UID2"})["UID1"]; got != tierFree {
		t.Fatalf("前置：UID1 应在 tierFree(%d)，实际 %d", tierFree, got)
	}

	// 模拟工具回执消耗 1 分（这就是 execGenerateImage / execWebSearch 现在做的事）
	po.noteConsumed("UID1", 1, now)

	if got := p.CandidateTiers("", []string{"UID1", "UID2"})["UID1"]; got != tierPaid {
		t.Errorf("★ 工具消耗掉最后的免费额度后，UID1 应落到 tierPaid(%d)，实际 %d ——\n"+
			"\n"+
			"说明扣减没生效：那个号会继续被当作\"还有免费额度\"优先选中，\n"+
			"继续烧永久额度（用户报的就是这个）。", tierPaid, got)
	}
}

// TestToolConsumptionPartialKeepsInPool 扣了但没扣完 ⇒ 仍在池子里（别误清）。
func TestToolConsumptionPartialKeepsInPool(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()
	p.pool().noteQuota("UID2", 100, true, now)
	p.pool().noteConsumed("UID2", 1, now)

	if got := p.CandidateTiers("", []string{"UID2"})["UID2"]; got != tierFree {
		t.Errorf("只扣 1/100 应仍在 tierFree(%d)，实际 %d", tierFree, got)
	}
}

// ─── 源码级守卫 ──────────────────────────────────────────────────────────

// toolPointsSitesAllowlist 允许"解析了 points_consumed 但不喂池子"的位置。
//
// ⚠ 这里是**空**的，这是刻意的：loomy 里每一个读 points_consumed 的地方
// 都是一次真实扣费，都必须反映到池子里。将来若出现"只读不扣"的正当理由
// （例如纯诊断），必须在本表写清理由 —— 而不是悄悄加个例外。
var toolPointsSitesAllowlist = map[string]string{}

// TestEveryPointsConsumedSiteFeedsPool 枚举 internal/loomy 下所有解析
// points_consumed 的位置，断言每处都有配套的 noteConsumed。
//
// 判据：
//  1. 每一处 `points_consumed` 的 json tag / map 键，都落在一个函数里；
//  2. 该函数（或它调用的、同文件内的紧邻上游函数）里必须出现 noteConsumed；
//  3. 特例必须登记在 toolPointsSitesAllowlist 里并写明理由。
//
// 这样将来新增一条计费路径却忘了喂池子时，**测试变红**而不是静默漏扣。
func TestEveryPointsConsumedSiteFeedsPool(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列文件失败: %v", err)
	}

	// 统计每个文件里 points_consumed 的出现次数与 noteConsumed 的出现次数。
	//
	// 为什么按**文件**而不是按函数：解析点与喂池点在同一个文件里是常态
	// （本包的工具实现都在 chattool.go）。按函数需要完整 Go 语法树，
	// 而这条守卫要守的是"这个文件里有没有把解析到的值用起来"——
	// 按文件判定更稳、也更容易看懂失败原因。
	type fileStat struct {
		pointsSites int
		feeds       int
	}
	stats := map[string]fileStat{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		if _, allowed := toolPointsSitesAllowlist[file]; allowed {
			continue
		}
		src, rerr := os.ReadFile(file)
		if rerr != nil {
			t.Fatalf("读 %s 失败: %v", file, rerr)
		}
		body := string(src)

		// 只数**结构体字段声明**与 map 键这两种真实解析点，
		// 不数注释与文档里的提及（注释里写 points_consumed 是在解释它）。
		sites := countCodeOccurrences(body, "points_consumed")
		feeds := countCodeOccurrences(body, "noteConsumed")
		if sites > 0 {
			stats[file] = fileStat{pointsSites: sites, feeds: feeds}
		}
	}

	for file, st := range stats {
		if st.feeds == 0 {
			t.Errorf("★ %s 解析了 points_consumed（%d 处）但**从不**调用 noteConsumed ——\n"+
				"\n"+
				"这意味着这条路径消耗的免费额度不会反映到池子里：\n"+
				"号会继续被当作\"还有免费额度\"优先选中，继续烧永久额度。\n"+
				"\n"+
				"修法：拿到数值后调 p.pool().noteConsumed(uid, points, time.Now())；\n"+
				"若确实只读不扣（纯诊断），把它登记到 toolPointsSitesAllowlist 并写明理由。\n"+
				"\n"+
				"⚠ 这是用户报的真缺陷（工具路径漏扣），不是风格问题。",
				file, st.pointsSites)
		}
	}
}

// countCodeOccurrences 数目标串在**代码**里出现的次数（排除行注释与整行注释）。
//
// ⚠ 必须先剥注释再数：本包里 points_consumed 在注释里出现得比代码里还多
// （每处都在解释这个字段的语义）。直接 strings.Count 会把注释算成解析点，
// 于是守卫永远红 —— 或者更糟：为了让守卫变绿而去删注释。
//
// 这条与仓库里 T-19 那条教训同源（守卫用 strings.Contains 时被注释骗成假绿）。
func countCodeOccurrences(body, needle string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		// 剥行尾注释（够用：本包没有把 // 放进字符串字面量里的写法）
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		n += strings.Count(line, needle)
	}
	return n
}
