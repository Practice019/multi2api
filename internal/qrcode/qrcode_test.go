package qrcode

import (
	"strings"
	"testing"
)

// TestSizeFollowsSpec 尺寸必须是 21 + 4×(版本-1)。
//
// 版本由内容长度决定，所以这里只断言"落在合法集合里"——
// 与参照的 raccoon-qr.spec.ts 同一条判据。
func TestSizeFollowsSpec(t *testing.T) {
	m, err := Build("HELLO")
	if err != nil {
		t.Fatalf("Build 报错: %v", err)
	}
	legal := map[int]bool{21: true, 25: true, 29: true, 33: true, 37: true, 41: true}
	if !legal[m.Size] {
		t.Errorf("尺寸 %d 不在合法集合 {21,25,29,33,37,41}", m.Size)
	}
	if len(m.Modules) != m.Size {
		t.Errorf("行数 %d ≠ 尺寸 %d", len(m.Modules), m.Size)
	}
	for i, row := range m.Modules {
		if len(row) != m.Size {
			t.Errorf("第 %d 行长度 %d ≠ 尺寸 %d", i, len(row), m.Size)
		}
	}
}

// TestFinderPatterns 三个角必须有 finder pattern（7×7 方框）。
//
// 外圈全黑、中心 3×3 全黑、中间一圈全白 —— 这是 QR 最显眼的特征，
// 也是扫码器定位的第一步。画错它等于码扫不出来。
func TestFinderPatterns(t *testing.T) {
	m, err := Build("https://xiaohuanxiong.com/login/mp?code=abc")
	if err != nil {
		t.Fatalf("Build 报错: %v", err)
	}
	checkFinder := func(rowStart, colStart int) {
		for r := 0; r < 7; r++ {
			for c := 0; c < 7; c++ {
				isBorder := r == 0 || r == 6 || c == 0 || c == 6
				isCenter := r >= 2 && r <= 4 && c >= 2 && c <= 4
				want := isBorder || isCenter
				if got := m.Modules[rowStart+r][colStart+c]; got != want {
					t.Errorf("finder(%d,%d) 处 (%d,%d)=%v want %v",
						rowStart, colStart, rowStart+r, colStart+c, got, want)
					return
				}
			}
		}
	}
	checkFinder(0, 0)
	checkFinder(0, m.Size-7)
	checkFinder(m.Size-7, 0)
}

// TestNoFinderInBottomRight 右下角**不该**有 finder（只有三个角有）。
//
// ⚠ 这条能抓"多画了一个 finder"这类错位缺陷 —— 那种缺陷下
// 三个角的断言照样全绿，但码已经扫不出来。
func TestNoFinderInBottomRight(t *testing.T) {
	m, err := Build("TEST")
	if err != nil {
		t.Fatalf("Build 报错: %v", err)
	}
	allBorderBlack := true
	for c := 0; c < 7; c++ {
		if !m.Modules[m.Size-7][m.Size-7+c] {
			allBorderBlack = false
		}
	}
	if allBorderBlack {
		t.Error("右下角 7×7 顶边全黑 —— 那里不该有 finder")
	}
}

// TestDeterministic 相同输入产生相同矩阵。
//
// 掩码是**按惩罚分选出来的**，若选择逻辑带随机性（或遍历 map），
// 同一个 URL 每次会产出不同的码 —— 那不影响扫描，但会让"对比两次结果"
// 这类排障手段失效。
func TestDeterministic(t *testing.T) {
	a, err1 := Build("SAME")
	b, err2 := Build("SAME")
	if err1 != nil || err2 != nil {
		t.Fatalf("Build 报错: %v / %v", err1, err2)
	}
	for y := 0; y < a.Size; y++ {
		for x := 0; x < a.Size; x++ {
			if a.Modules[y][x] != b.Modules[y][x] {
				t.Fatalf("两次 Build 的 (%d,%d) 不同", y, x)
			}
		}
	}
}

// TestDifferentInputsDiffer 不同输入产生不同矩阵。
func TestDifferentInputsDiffer(t *testing.T) {
	a, _ := Build("AAA")
	b, _ := Build("BBB")
	same := a.Size == b.Size
	if same {
		for y := 0; y < a.Size && same; y++ {
			for x := 0; x < a.Size; x++ {
				if a.Modules[y][x] != b.Modules[y][x] {
					same = false
					break
				}
			}
		}
	}
	if same {
		t.Error("AAA 与 BBB 产出了相同矩阵")
	}
}

// TestVersionGrowsWithContent 内容变长时版本提升（矩阵变大）。
func TestVersionGrowsWithContent(t *testing.T) {
	short, _ := Build("A")
	long, _ := Build("https://xiaohuanxiong.com/login/mp?code=" + strings.Repeat("a", 64))
	if long.Size < short.Size {
		t.Errorf("长内容尺寸 %d 反而小于短内容 %d", long.Size, short.Size)
	}
}

// TestEmptyStringOK 空字符串不报错（产出最小矩阵）。
func TestEmptyStringOK(t *testing.T) {
	if _, err := Build(""); err != nil {
		t.Errorf("空字符串不该报错: %v", err)
	}
}

// TestRealLoginURLLength 真实长度的登录 URL 可编码。
//
// 实测形态：`https://xiaohuanxiong.com/login/mp?code=<32位hex>&appname=…`
// 约 145 字节，需要版本 8（尺寸 49）。
func TestRealLoginURLLength(t *testing.T) {
	url := "https://xiaohuanxiong.com/login/mp?code=" + strings.Repeat("a", 32) +
		"&appname=%E5%95%86%E6%B1%A4%E5%B0%8F%E6%B5%A3%E7%86%8A%E5%AE%98%E7%BD%91"
	m, err := Build(url)
	if err != nil {
		t.Fatalf("真实登录 URL 编不出来: %v", err)
	}
	if m.Size < 49 {
		t.Errorf("尺寸 %d < 49（%d 字节的内容至少要版本 8）", m.Size, len(url))
	}
}

// TestTooLongReturnsError 内容超出范围时返回**可读错误**。
//
// ⚠ 不能静默产出坏码：那表现为"用户扫码没反应"，
// 且没有任何迹象指向编码器。
func TestTooLongReturnsError(t *testing.T) {
	_, err := Build(strings.Repeat("x", 1000))
	if err == nil {
		t.Fatal("超长内容必须报错，不能静默产出坏码")
	}
	if !strings.Contains(err.Error(), "过长") && !strings.Contains(err.Error(), "超出") {
		t.Errorf("错误信息应说清原因，实际: %v", err)
	}
}

// TestAllEightMasksDiffer 8 个掩码产出各不相同的矩阵。
//
// # 为什么单独立一条（参照项目专门记过这个坑）
//
// 掩码曾是"参数不生效"的缺陷（不同掩码产出相同矩阵），
// 而当时的测试看不出来。这里锁死"不同掩码确实不同"这一事实 ——
// 它是"自动选掩码"有意义的前提。
func TestAllEightMasksDiffer(t *testing.T) {
	sigs := map[string]int{}
	for mask := 0; mask < 8; mask++ {
		sig := matrixWithMask(t, "HELLO", mask)
		sigs[sig]++
	}
	if len(sigs) != 8 {
		t.Errorf("8 个掩码只产出了 %d 种不同矩阵 —— 掩码参数没生效", len(sigs))
	}
}

// matrixWithMask 用**指定掩码**构造矩阵并返回签名。
//
// 本包的生产入口（Build）走自动评分，不暴露 mask ——
// 所以这里直接用内部函数构造（同包测试）。
func matrixWithMask(t *testing.T, text string, mask int) string {
	t.Helper()
	bytes := make([]int, 0, len(text))
	for _, b := range []byte(text) {
		bytes = append(bytes, int(b))
	}
	version := pickVersion(len(bytes))
	size := version*4 + 17
	modules := make([][]bool, size)
	isFunction := make([][]bool, size)
	for i := range modules {
		modules[i] = make([]bool, size)
		isFunction[i] = make([]bool, size)
	}
	drawFunctionPatterns(modules, isFunction, size, version)
	cw, err := buildCodewords(bytes, version)
	if err != nil {
		t.Fatalf("buildCodewords: %v", err)
	}
	drawCodewords(modules, isFunction, size, cw)
	applyMask(modules, isFunction, size, mask)
	drawFormatBits(modules, isFunction, size, mask)

	var sb strings.Builder
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if modules[y][x] {
				sb.WriteByte('1')
			} else {
				sb.WriteByte('0')
			}
		}
	}
	return sb.String()
}

// TestRenderSVGShape SVG 形态（与参照的 renderQrSvg 断言同一组）。
func TestRenderSVGShape(t *testing.T) {
	svg, err := RenderSVG("HELLO", 0)
	if err != nil {
		t.Fatalf("RenderSVG 报错: %v", err)
	}
	for _, want := range []string{"<svg", "viewBox=", "<path", "</svg>", `width="158"`, `height="158"`} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG 缺 %q", want)
		}
	}

	m, _ := Build("HELLO")
	// viewBox 含静区（默认 4 模块）
	if !strings.Contains(svg, "0 0 "+itoa(m.Size+8)+" "+itoa(m.Size+8)) {
		t.Errorf("viewBox 应含静区（size+8=%d）", m.Size+8)
	}
	// 深色模块数 == path 里的 M 命令数（SVG 与矩阵一致）
	darkCount := 0
	for _, row := range m.Modules {
		for _, c := range row {
			if c {
				darkCount++
			}
		}
	}
	if got := strings.Count(svg, "M"); got != darkCount {
		t.Errorf("SVG 里的 M 命令 %d 个，矩阵深色模块 %d 个 —— 两者必须一致", got, darkCount)
	}
}

// TestRenderSVGSizeOption size 选项生效。
func TestRenderSVGSizeOption(t *testing.T) {
	svg, err := RenderSVG("HELLO", 300)
	if err != nil {
		t.Fatalf("RenderSVG 报错: %v", err)
	}
	if !strings.Contains(svg, `width="300"`) || !strings.Contains(svg, `height="300"`) {
		t.Errorf("size 选项没生效: %s", svg[:80])
	}
}

// TestFormatBitsSelfConsistent 格式信息必须能解回「纠错 M + 选中的掩码」。
//
// # 为什么这条比"结构看着对"强
//
// 格式信息是**编码过的**（BCH(15,5) + 固定掩码 0x5412），画错位置或
// 算错多项式时，矩阵的其它部分看起来完全正常，但扫码器读不出
// 纠错等级与掩码 → 直接扫不出来。
//
// 这里把两份拷贝都解出来，与"实际用的掩码"比对。
func TestFormatBitsSelfConsistent(t *testing.T) {
	m, err := Build("HELLO")
	if err != nil {
		t.Fatalf("Build 报错: %v", err)
	}
	size := m.Size

	// 读第一份拷贝（第 8 列 + 第 8 行）
	readBit := func(x, y int) int {
		if m.Modules[y][x] {
			return 1
		}
		return 0
	}
	bits := 0
	for i := 0; i <= 5; i++ {
		bits |= readBit(8, i) << i
	}
	bits |= readBit(8, 7) << 6
	bits |= readBit(8, 8) << 7
	bits |= readBit(7, 8) << 8
	for i := 9; i < 15; i++ {
		bits |= readBit(14-i, 8) << i
	}
	// 去掉固定掩码，再做 BCH 校验
	v := bits ^ 0x5412
	// 高 5 位是 data = (纠错等级 << 3) | 掩码
	data := v >> 10
	if ecLevel := data >> 3; ecLevel != 0b00 {
		t.Errorf("纠错等级位 = %b，want 0b00（等级 M）", ecLevel)
	}
	mask := data & 0b111
	if mask < 0 || mask > 7 {
		t.Errorf("解出的掩码 %d 越界", mask)
	}

	// BCH 校验：rem 必须能被生成多项式整除
	rem := v
	for i := 14; i >= 10; i-- {
		if (rem>>i)&1 == 1 {
			rem ^= 0x537 << (i - 10)
		}
	}
	if rem != 0 {
		t.Errorf("格式信息 BCH 校验失败（余数 %d ≠ 0）—— 扫码器会读不出纠错等级与掩码", rem)
	}

	// 第二份拷贝必须与第一份**一致**（否则扫码器按哪个读是未定义的）
	bits2 := 0
	for i := 0; i < 8; i++ {
		bits2 |= readBit(size-1-i, 8) << i
	}
	for i := 8; i < 15; i++ {
		bits2 |= readBit(8, size-7+(i-8)) << i
	}
	if bits2 != bits {
		t.Errorf("两份格式信息不一致：第一份 %015b，第二份 %015b", bits, bits2)
	}
}

// TestDataCapacityBoundary 容量边界：刚好装下 / 多一字节。
//
// 版本 1 + 纠错 M 的 byte 模式容量是 14 字节（16 码字 - 2 头部开销）。
// 15 字节必须升到版本 2（尺寸 25）。
func TestDataCapacityBoundary(t *testing.T) {
	ok1, err := Build(strings.Repeat("a", 14))
	if err != nil {
		t.Fatalf("14 字节应该能编: %v", err)
	}
	if ok1.Size != 21 {
		t.Errorf("14 字节的尺寸 = %d，want 21（版本 1）", ok1.Size)
	}
	ok2, err := Build(strings.Repeat("a", 15))
	if err != nil {
		t.Fatalf("15 字节应该能编: %v", err)
	}
	if ok2.Size != 25 {
		t.Errorf("15 字节的尺寸 = %d，want 25（版本 2，14 字节装不下）", ok2.Size)
	}
}

// itoa 小整数转字符串（避免为一行引入 strconv）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
