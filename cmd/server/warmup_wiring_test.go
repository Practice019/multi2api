// warmup_wiring_test.go Loomy 后台档位预热的**接线守卫**。
//
// # 为什么需要这条（变异验证抓出来的）
//
// 我把启动预热写进 cmd/server/main.go 之后做变异验证，其中一条变异是
// **把 `lm.StartTierWarmup()` 整个删掉** —— 结果**全绿**。
//
// 也就是说：整条预热可以在无人察觉的情况下从生产路径上消失，
// 而所有单测照常通过（它们直接调 warmupPass）。这正是用户报障的那种形态：
// 功能看起来在、代码看起来对、**只有线上表现不对**。
//
// 这条守卫不做运行时验证，只钉"装配层确实调了它"。它不证明预热有效
// （那是 candidate_tier_warmup_test.go 的事），它只证明**它被接上了**——
// 两件事要分开守，缺一个都会漏。
package main

import (
	"os"
	"strings"
	"testing"
)

func TestLoomyTierWarmupIsWiredInAssembly(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	src := string(raw)

	// 判据一：调用必须**存在**。
	if !strings.Contains(src, "StartTierWarmup()") {
		t.Fatal("cmd/server/main.go 没有调用 lm.StartTierWarmup() ——\n" +
			"档位缓存是纯内存的，重启后是空的；空缓存里所有账号都是「不确定」档，" +
			"它们之间没有优先级，选号退化成加权抽签。\n" +
			"实测后果：重启后 22 小时里唯一一次计费记在了那唯一一个免费额度为 0 的号上" +
			"（永久积分 -30），而另外 21 个号都还有免费额度。\n" +
			"删掉这行不会有任何测试失败 —— 这条守卫就是为此存在的。")
	}

	// 判据二：调用点必须在「loomy 已启用」日志**之后**
	//（那之前它在 `if cfg.LoomyEnabled` 之外/之前 —— 挪进 else 分支就等于没接，
	// 因为 else 分支里 lm 是 nil）。
	iLog := strings.Index(src, "loomy: 已启用")
	iCall := strings.Index(src, "lm.StartTierWarmup()")

	// ⚠ 锚点不存在时必须**报错**，不能静默跳过。
	//
	// 我写这版时用的是 `if iLog >= 0 && ...` —— 那样只要有人改了那句日志文案，
	// 判据二就**悄悄不再检查任何东西**，而测试依然全绿。
	// 那与"守卫失效"是同一件事，只是更隐蔽：它看起来还在守。
	// 所以锚点缺失本身就是失败 —— "判据跑不了" 等同 "判据不在"。
	if iLog < 0 {
		t.Fatal("找不到锚点「loomy: 已启用」—— 判据二（调用点在 if cfg.LoomyEnabled 内）" +
			"无法执行。\n改日志文案时请同步更新这里：一条**跑不了**的判据等于没有判据。")
	}
	if iCall < 0 {
		t.Fatal("找不到 `lm.StartTierWarmup()` —— 判据一已经会报，这里给出更具体的线索")
	}
	if iCall < iLog {
		t.Error("StartTierWarmup 的调用点在「loomy 已启用」日志**之前** ——\n" +
			"那意味着它可能落在 cfg.LoomyEnabled=false 的分支里（此时 lm 为 nil），" +
			"预热会静默失效。它必须在 `if cfg.LoomyEnabled` 分支内。")
	}

	// 判据三：那一行必须**恰好两层缩进** —— 即直接躺在 `if cfg.LoomyEnabled { ... }`
	// 块里，没有被任何条件包起来。
	//
	// # 为什么连缩进都判（变异验证抓出来的）
	//
	// 我把调用整段用 `if false { ... }` 包起来（"有人给它加了个开关但写死了"）
	// 之后，前两条判据**全绿** —— 字符串还在、位置还在。
	// 而那正是最危险的一种退化：代码看着在、条件永不成立、功能静默消失。
	//
	// ⚠ 代价如实标注：将来有人**合理地**给它加一个开关（比如
	// `if cfg.LoomyWarmup {`），这条会红。那种情况应该红 ——
	// "什么时候不预热"是个需要被看见的决定，不该偷偷发生。
	indentOK := false
	for _, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "lm.StartTierWarmup()" {
			if line == "\t\tlm.StartTierWarmup()" {
				indentOK = true
				break
			}
		}
	}
	if !indentOK {
		t.Error("`lm.StartTierWarmup()` 必须恰好两层缩进（直接躺在 `if cfg.LoomyEnabled {` 块里）——\n" +
			"多一层缩进意味着它被某个条件包住了，而那个条件一旦写死成假值，" +
			"预热就静默消失：代码看着在、所有单测照常通过、只有线上表现为" +
			"「重启后头几次请求仍会烧永久积分」。")
	}
}
