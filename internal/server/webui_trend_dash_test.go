package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWebUITrendLinesConnectEmptyBucketsWithDash 空桶改**虚线插值连线**（T-419）。
//
// # 用户报障（附截图）
//
//	「这个线很奇怪断开的」「空的我希望也要画，不要断开不然很奇怪」
//
// 实测：24 小时里只有 9 个整点有请求，中间**连续 13 个整点全空** ⇒ 蓝线断成 3 截。
//
// # ★ 但 T-388/T-390 的意图必须保住（这两次改动修的是同一个坑）
//
// 那两次修的是：**在空桶上画 0% ⇒ 一条贴着底部的假线**，用户会读成
// "那段时间有数据但命中率 0"，而真相是"那段时间一次请求都没有"。
// ⇒ 现在要连线，但**空档那一段必须是虚线**（虚线的既有语义就是"推测/插值"），
//
//	且**不许**用 0 作为空桶的值。
//
// 四条判据：
//
//	① 旧的"切段断开"逻辑已删（rateSegs 那套）
//	② 虚线已实现（dash / stroke-dasharray）
//	③ 线性插值已实现（prev.y + (next.y - prev.y) * t 或等价）
//	④ 文案：不再有「线在那里断开」，且出现「虚线连接」
func TestWebUITrendLinesConnectEmptyBucketsWithDash(t *testing.T) {
	jsB, err := os.ReadFile(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("读 ui/app.js 失败：%v", err)
	}
	js := string(jsB)

	// ① 旧断段逻辑已删
	if strings.Contains(js, "const rateSegs = []") {
		t.Error("缓存命中率线仍在用 rateSegs 切段断开 —— 用户要求连线" +
			"（「空的我希望也要画，不要断开不然很奇怪」）")
	}
	if strings.Contains(js, "const tokSegs = []") {
		t.Error("Token 折线仍在用 tokSegs 切段断开 —— 两条线在同一张图上，一条断一条不断更怪")
	}

	// ② 虚线已实现（dcLine 的 opts 要能传出 dash，或直接写 stroke-dasharray）
	if !strings.Contains(js, "stroke-dasharray") && !strings.Contains(js, "dash") {
		t.Error("找不到虚线实现（stroke-dasharray / dash）—— 空档那段必须画成虚线，" +
			"否则用户会把插值当成真实值")
	}

	// ③ 线性插值已实现（判据用正则，容忍空格差异）
	if !regexp.MustCompile(`prev\.y\s*\+\s*\(next\.y\s*-\s*prev\.y\)\s*\*\s*t`).MatchString(js) {
		t.Error("找不到线性插值的痕迹（prev.y + (next.y - prev.y) * t）—— " +
			"空桶不许用 0，也不该用前值保持（那会画成水平线、看起来像真值）")
	}

	// ④ 文案
	html, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	page := string(html)
	if strings.Contains(page, "线在那里断开") {
		t.Error("图例里仍写着「线在那里断开」—— 现在连的是虚线，文案必须跟着改" +
			"（否则用户看不到「为什么这段是虚线」的解释）")
	}
	if !strings.Contains(page, "虚线") {
		t.Error("图例里没有「虚线」字样 —— 需要一句「无请求的整点用虚线连接（那里是插值，不是真实值）」")
	}
	// ④b 空桶不许被当成 0：源码里不得出现"用 0 填空桶"的形态
	if regexp.MustCompile(`empty\s*\?\s*0\s*:`).MatchString(js) {
		t.Error("发现 `empty ? 0 :` 形态 —— 空桶被当成 0 了，那正是 T-388/T-390 修掉的假线")
	}
}
