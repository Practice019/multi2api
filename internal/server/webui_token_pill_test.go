package server

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// tokenPillBody 截出 tokenPillClass 的函数体，供下面的守卫解析。
//
// 为什么要截体而不是全文扫：全文里 `n <` 这种形态到处都是（fmtCountdown、
// 分页、速率限制……），全文扫会把别的函数的阈值当成这一列的阈值 —— 那种
// 守卫会在别人改别处时莫名其妙地红，然后被人删掉。
func tokenPillBody(t *testing.T, src string) string {
	t.Helper()
	i := strings.Index(src, "function tokenPillClass(")
	if i < 0 {
		t.Fatal("webui.html 里找不到 tokenPillClass —— 守卫失效（fail-open）")
	}
	rest := src[i:]
	// 函数体到第一个顶格的 `  }` 为止（本文件用两空格缩进的函数体风格）。
	j := strings.Index(rest, "\n  }")
	if j < 0 {
		t.Fatal("tokenPillClass 找不到函数体结尾")
	}
	return rest[:j]
}

// TestWebUITokenPillThresholdsMatchCodeartsLifetime 钉住「Token 到期」胶囊的
// 分档阈值。
//
// # 这条守卫防的是什么
//
// 第一版写的是 `n < 3600 → err`，即"剩余不到一小时就标红"。这在 workbuddy
// 那种长效 token 上没问题，但 codearts 的 STS 实测寿命约 **2 小时**
// （本机实测剩余 6036s / 4134s；见 internal/codearts/accountview.go 的注释
// 与 tasks/plan.md 的实测表），于是每个 2 小时周期里有 57 分钟常亮红色。
//
// 红色常态化 = 信号消失：用户分不清「续期正常、只是快到期」和「续期坏了、
// 这个凭证真的救不回来」。而后者恰恰是他最初来问的那个问题。所以判据是
// **红色必须稀到只可能由"续期失败"产生**：codearts 的续期窗口只有 3 分钟
// （internal/codearts/refreshskew.go），正常跑就掉不到 15 分钟以下。
//
// # 为什么阈值写成"≤ 900"而不是"== 900"
//
// 允许往更严的方向调（把窗口收得更窄），只禁止放宽到"健康态也标红"。
// 一条只在别人想收紧时变红的守卫是不招人烦的；反之则会。
func TestWebUITokenPillThresholdsMatchCodeartsLifetime(t *testing.T) {
	body := tokenPillBody(t, string(webuiHTML))

	// 只取 `n < 正数` 形态：`n < 0` 是"已过期"这条**定点**判断，不是分档阈值，
	// 混进来会让下面的 nums[0]/nums[1] 全部错位。
	re := regexp.MustCompile(`n\s*<\s*([1-9]\d*)`)
	ms := re.FindAllStringSubmatch(body, -1)
	if len(ms) != 2 {
		t.Fatalf("tokenPillClass 里应当恰好有 2 个正数阈值（err 带、warn 带），实际 %d 个：%q", len(ms), ms)
	}
	nums := make([]int, 0, len(ms))
	for _, m := range ms {
		v, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("解析阈值失败: %q", m[1])
		}
		nums = append(nums, v)
	}
	errBand, warnBand := nums[0], nums[1]

	// 1) 单调：越少越严。反了的话会出现"剩 10 分钟是绿的、剩 40 分钟是红的"。
	if errBand >= warnBand {
		t.Errorf("阈值必须单调（err 带 < warn 带），实际 err=%d warn=%d", errBand, warnBand)
	}

	// 2) 核心判据：健康态不得标红。
	//
	//    codearts 正常剩余约 2 小时；把它当成"健康"的最保守取值就是 1 小时。
	//    只要有任何一个 ≥ 1 小时的值会被判成 err，这一列对 codearts 就废了。
	if errBand > 3600 {
		t.Errorf("err 带阈值 %ds > 3600s —— 健康的 codearts token（实测约 2 小时）会常亮红色，信号失效", errBand)
	}

	// 3) 但也不能小到没用：至少要比 3 分钟的续期窗口宽出一个反应余量，
	//    否则用户看到红色时已经没有时间处理了。
	if errBand < 600 {
		t.Errorf("err 带阈值 %ds 太小 —— 距续期窗口（3 分钟）不足 10 分钟余量，红色出现时已来不及反应", errBand)
	}

	// 4) warn 带别超过一天：超过就落进 fmtTokenRemain 的"天"档，
	//    会出现"文字写 3 天、颜色却是黄色"的怪胶囊。
	if warnBand > 86400 {
		t.Errorf("warn 带阈值 %ds 超过一天 —— 与 fmtTokenRemain 的「天」档错位", warnBand)
	}
}

// TestWebUITokenPillKeepsUnknownDistinctFromExpired 钉住「未知」与「已过期」
// 在配色上仍可区分（都在 err 侧的话，用户无法判断是"上游没给"还是"真过期"）。
func TestWebUITokenPillKeepsUnknownDistinctFromExpired(t *testing.T) {
	body := tokenPillBody(t, string(webuiHTML))

	// 未知（非数值）走 dim，且必须排在任何数值判断**之前** ——
	// Number(undefined) 是 NaN，NaN 参与比较恒为 false，顺序反了会漏到 'ok'。
	dimAt := strings.Index(body, "return 'dim'")
	errAt := strings.Index(body, "return 'err'")
	if dimAt < 0 {
		t.Fatal("tokenPillClass 没有 dim 分支 —— 未知态与已过期态无法区分")
	}
	if errAt >= 0 && dimAt > errAt {
		t.Error("dim 分支排在 err 分支之后 —— 未知值可能先被判成别的档")
	}

	if !strings.Contains(body, "n < 0") {
		t.Error("缺少 `n < 0` 的已过期分支 —— 负数会被 warn 档吞掉")
	}
}
