// Package qrcode 最小二维码生成器（byte 模式 + 纠错等级 M + 版本 1–10）。
//
// # 为什么需要它（本轮修的缺陷）
//
// `raccoon` 的登录是**微信扫码**：`Start()` 返回的是一个**要扫的 URL**
// （`https://xiaohuanxiong.com/login/mp?code=<32位hex>`），而不是一个
// "点开就能授权"的链接。而前端此前对所有上游都只渲染
// 「打开链接 / 复制链接」—— 用户拿到一条链接，无法扫码，
// 「添加账号」等于不可用。
//
// 参照项目（dsh-codearts-auth 的 raccoon-qr.ts）的做法是**宿主侧**
// 生成 SVG 二维码内联进它自己的登录页。本网关是纯后端、没有那个页面，
// 所以这里生成 SVG 字符串，由核心随登录回执下发，前端插进 DOM 即可。
//
// # 为什么自己实现而不引入依赖
//
// 本仓的硬约束是「Release 下载即用」：go.mod 直接依赖只有 go-redis
// （见 AGENTS.md 的依赖铁律）。为一个固定形态的短 URL 引入 QR 库
// 会破坏它。参照项目也做了同样选择（它连 node_modules 里没有 QR 依赖
// 这件事都专门记了一笔）。
//
// # 实现范围（刻意最小，与参照逐字对齐）
//
//   - **byte 模式**（UTF-8 字节）
//   - **纠错等级 M**
//   - **版本 1–10**（内容上限 213 字节，够编码登录 URL）
//
// 不做数字/字母数字模式、不做更高纠错等级、不做版本 11+。
// 超出容量时**返回错误**（由调用方缩短内容），而不是静默产出扫不出来的坏码
// —— 后者会表现为"用户扫码没反应"，且没有任何迹象指向编码器。
//
// 算法依据 ISO/IEC 18004，结构与参照实现一致（两者都源自 Nayuki 的
// 参考实现思路）。**逐位对齐**由 qrcode_test.go 的已知向量守住。
package qrcode

import (
	"errors"
	"fmt"
	"strings"
)

// dataCodewords 各版本（1–10）纠错等级 M 的**数据码字**总数。
//
// 下标 0 占位（没有版本 0），与参照的 `DATA_CODEWORDS` 逐字一致。
var dataCodewords = [11]int{0, 16, 28, 44, 64, 86, 108, 124, 154, 182, 216}

// ecBlocks 各版本的纠错块结构（纠错等级 M）。
//
//	ecPerBlock 每块的纠错码字数
//	groups     [块数, 每块数据码字数] 的列表
type ecBlocks struct {
	ecPerBlock int
	groups     [][2]int
}

var ecBlocksM = [11]*ecBlocks{
	nil,
	{10, [][2]int{{1, 16}}},
	{16, [][2]int{{1, 28}}},
	{26, [][2]int{{1, 44}}},
	{18, [][2]int{{2, 32}}},
	{24, [][2]int{{2, 43}}},
	{16, [][2]int{{4, 27}}},
	{18, [][2]int{{4, 31}}},
	{22, [][2]int{{2, 38}, {2, 39}}},
	{22, [][2]int{{3, 36}, {2, 37}}},
	{26, [][2]int{{4, 43}, {1, 44}}},
}

// maxInputBytes 版本 10 + 纠错 M 的 byte 模式容量（213 字节）。
const maxInputBytes = 213

// ── GF(256) 运算（本原多项式 0x11D）──────────────────────────────────

var (
	gfExp [512]int
	gfLog [256]int
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = x
		gfLog[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11d
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[gfLog[a]+gfLog[b]]
}

// polyMul 多项式乘法（最高次在前）。
func polyMul(a, b []int) []int {
	out := make([]int, len(a)+len(b)-1)
	for i := range a {
		for j := range b {
			out[i+j] ^= gfMul(a[i], b[j])
		}
	}
	return out
}

// rsGeneratorPoly 生成 degree 次 Reed-Solomon 生成多项式（最高次在前）。
func rsGeneratorPoly(degree int) []int {
	poly := []int{1}
	for i := 0; i < degree; i++ {
		poly = polyMul(poly, []int{1, gfExp[i]})
	}
	return poly
}

// rsEncode 计算 Reed-Solomon 纠错码字（综合除法取余）。
func rsEncode(data []int, ecCount int) []int {
	gen := rsGeneratorPoly(ecCount)
	buf := make([]int, len(data)+ecCount)
	copy(buf, data)
	for i := range data {
		coef := buf[i]
		if coef == 0 {
			continue
		}
		for j := range gen {
			buf[i+j] ^= gfMul(gen[j], coef)
		}
	}
	return buf[len(data):]
}

// ── 数据编码 ─────────────────────────────────────────────────────────

// pickVersion 选能容纳 byteLength 字节的最小版本；都装不下返回 0。
func pickVersion(byteLength int) int {
	for version := 1; version <= 10; version++ {
		capacityBits := dataCodewords[version] * 8
		// 模式指示符 4 位 + 字符计数（版本 1–9 是 8 位，10 起是 16 位）
		overhead := 4
		if version <= 9 {
			overhead += 8
		} else {
			overhead += 16
		}
		if overhead+byteLength*8 <= capacityBits {
			return version
		}
	}
	return 0
}

// buildCodewords 把字节编码成完整码字序列（数据 + 纠错，按块交织）。
func buildCodewords(bytes []int, version int) ([]int, error) {
	blocks := ecBlocksM[version]
	if blocks == nil {
		return nil, fmt.Errorf("qrcode: 不支持的版本 %d", version)
	}
	totalData := dataCodewords[version]
	capacityBits := totalData * 8

	// 1) 位流：模式 + 计数 + 数据 + 终止符 + 补齐
	bits := make([]int, 0, capacityBits)
	pushBits := func(value, length int) {
		for i := length - 1; i >= 0; i-- {
			bits = append(bits, (value>>i)&1)
		}
	}
	pushBits(0b0100, 4) // byte 模式
	if version <= 9 {
		pushBits(len(bytes), 8)
	} else {
		pushBits(len(bytes), 16)
	}
	for _, b := range bytes {
		pushBits(b, 8)
	}

	// 终止符最多 4 位（容量刚好时可以为 0 位）
	terminator := 4
	if rem := capacityBits - len(bits); rem < terminator {
		terminator = rem
	}
	pushBits(0, terminator)
	// 补齐到字节边界
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}
	// 交替填充字节
	padBytes := [2]int{0xec, 0x11}
	for i := 0; len(bits) < capacityBits; i++ {
		pushBits(padBytes[i%2], 8)
	}

	// 2) 位流 → 数据码字
	dataCW := make([]int, 0, totalData)
	for i := 0; i < len(bits); i += 8 {
		b := 0
		for j := 0; j < 8; j++ {
			b = (b << 1) | bits[i+j]
		}
		dataCW = append(dataCW, b)
	}

	// 3) 切块 + 逐块算纠错
	var dataBlocks, ecBlockList [][]int
	offset := 0
	for _, g := range blocks.groups {
		count, perBlock := g[0], g[1]
		for b := 0; b < count; b++ {
			block := dataCW[offset : offset+perBlock]
			offset += perBlock
			dataBlocks = append(dataBlocks, block)
			ecBlockList = append(ecBlockList, rsEncode(block, blocks.ecPerBlock))
		}
	}

	// 4) 交织：先按序取各块的数据码字，再按序取各块的纠错码字
	result := make([]int, 0, totalData+blocks.ecPerBlock*len(dataBlocks))
	maxDataLen := 0
	for _, b := range dataBlocks {
		if len(b) > maxDataLen {
			maxDataLen = len(b)
		}
	}
	for i := 0; i < maxDataLen; i++ {
		for _, block := range dataBlocks {
			if i < len(block) {
				result = append(result, block[i])
			}
		}
	}
	for i := 0; i < blocks.ecPerBlock; i++ {
		for _, block := range ecBlockList {
			result = append(result, block[i])
		}
	}
	return result, nil
}

// ── 矩阵构造 ─────────────────────────────────────────────────────────

// alignmentPositions 对齐图案中心坐标（版本 1 无）。
func alignmentPositions(version, size int) []int {
	if version == 1 {
		return nil
	}
	numAlign := version/7 + 2
	step := ((version*4 + 4) + (numAlign*2 - 2) - 1) / (numAlign*2 - 2) * 2
	result := []int{6}
	for pos := size - 7; len(result) < numAlign; pos -= step {
		result = append(result[:1], append([]int{pos}, result[1:]...)...)
	}
	return result
}

// drawFinderPattern 画 finder pattern（含分隔符，x/y 是中心）。
func drawFinderPattern(modules, isFunction [][]bool, size, x, y int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			xx, yy := x+dx, y+dy
			if xx < 0 || xx >= size || yy < 0 || yy >= size {
				continue
			}
			dist := abs(dx)
			if abs(dy) > dist {
				dist = abs(dy)
			}
			// dist 4 = 分隔符（浅色），2 = 内圈（浅色），其余深色
			modules[yy][xx] = dist != 2 && dist != 4
			isFunction[yy][xx] = true
		}
	}
}

// drawAlignmentPattern 画 alignment pattern（x/y 是中心）。
func drawAlignmentPattern(modules, isFunction [][]bool, x, y int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			d := abs(dx)
			if abs(dy) > d {
				d = abs(dy)
			}
			modules[y+dy][x+dx] = d != 1
			isFunction[y+dy][x+dx] = true
		}
	}
}

// drawFormatBits 写入格式信息（纠错等级 M + 掩码号），两份拷贝。
func drawFormatBits(modules, isFunction [][]bool, size, mask int) {
	data := (0b00 << 3) | mask // 纠错等级 M 的 formatBits 是 0b00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := ((data << 10) | rem) ^ 0x5412

	set := func(x, y int, dark bool) {
		modules[y][x] = dark
		isFunction[y][x] = true
	}
	bit := func(i int) bool { return (bits>>i)&1 == 1 }

	// 第一份：第 8 列（行 0..5、7、8）+ 第 8 行（列 8、7、5..0）
	for i := 0; i <= 5; i++ {
		set(8, i, bit(i))
	}
	set(8, 7, bit(6))
	set(8, 8, bit(7))
	set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		set(14-i, 8, bit(i))
	}

	// 第二份：第 8 行右侧 8 列 + 第 8 列底部 **7 行**。
	//
	// ⚠ 底部 7 格是 size-7 到 size-1（版本 1 即 y=14..20），
	// 不是 size-15+i（那会写到 y=6..12，把 timing 行与数据格一起污染）。
	// (8, size-8) 是固定的深色模块，不承载格式位。
	for i := 0; i < 8; i++ {
		set(size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		set(8, size-7+(i-8), bit(i))
	}
	set(8, size-8, true)
}

// drawFunctionPatterns 画全部功能图案（timing / finder / alignment / 版本信息）。
func drawFunctionPatterns(modules, isFunction [][]bool, size, version int) {
	// timing pattern
	for i := 0; i < size; i++ {
		dark := i%2 == 0
		modules[6][i] = dark
		isFunction[6][i] = true
		modules[i][6] = dark
		isFunction[i][6] = true
	}

	// 三个 finder（会覆盖部分 timing，符合规范）
	drawFinderPattern(modules, isFunction, size, 3, 3)
	drawFinderPattern(modules, isFunction, size, size-4, 3)
	drawFinderPattern(modules, isFunction, size, 3, size-4)

	// alignment
	positions := alignmentPositions(version, size)
	n := len(positions)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			// 跳过三个 finder 角落
			if (i == 0 && j == 0) || (i == 0 && j == n-1) || (i == n-1 && j == 0) {
				continue
			}
			drawAlignmentPattern(modules, isFunction, positions[i], positions[j])
		}
	}

	// 预留格式信息区（稍后写入真实值）
	drawFormatBits(modules, isFunction, size, 0)

	// 版本信息（版本 >= 7）
	if version >= 7 {
		rem := version
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1f25)
		}
		bits := (version << 12) | rem
		for i := 0; i < 18; i++ {
			dark := (bits>>i)&1 == 1
			a := size - 11 + (i % 3)
			b := i / 3
			modules[b][a] = dark
			isFunction[b][a] = true
			modules[a][b] = dark
			isFunction[a][b] = true
		}
	}
}

// drawCodewords 按 zigzag 从右下向上填充数据位。
func drawCodewords(modules, isFunction [][]bool, size int, codewords []int) {
	i := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // 跳过 timing 列
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = size - 1 - vert
				}
				if !isFunction[y][x] && i < len(codewords)*8 {
					byteVal := codewords[i>>3]
					modules[y][x] = (byteVal>>(7-(i&7)))&1 == 1
					i++
				}
				// 剩余位（0–7 个）保持构造时的浅色，符合规范
			}
		}
	}
}

// applyMask 应用掩码（XOR，自逆）。
func applyMask(modules, isFunction [][]bool, size, mask int) {
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if isFunction[y][x] {
				continue
			}
			var invert bool
			switch mask {
			case 0:
				invert = (x+y)%2 == 0
			case 1:
				invert = y%2 == 0
			case 2:
				invert = x%3 == 0
			case 3:
				invert = (x+y)%3 == 0
			case 4:
				invert = (y/2+x/3)%2 == 0
			case 5:
				invert = (x*y)%2+(x*y)%3 == 0
			case 6:
				invert = ((x*y)%2+(x*y)%3)%2 == 0
			default:
				invert = ((x+y)%2+(x*y)%3)%2 == 0
			}
			if invert {
				modules[y][x] = !modules[y][x]
			}
		}
	}
}

// penaltyForLine 单行/单列的规则 1 与规则 3 惩罚。
func penaltyForLine(line []bool, size, n1, n3 int) int {
	result := 0

	// 规则 1：连续同色
	runLength := 1
	for i := 1; i < size; i++ {
		if line[i] == line[i-1] {
			runLength++
		} else {
			if runLength >= 5 {
				result += n1 + (runLength - 5)
			}
			runLength = 1
		}
	}
	if runLength >= 5 {
		result += n1 + (runLength - 5)
	}

	// 规则 3：finder-like 图案（两个方向的 11 位窗口）
	patternA := [11]bool{true, false, true, true, true, false, true, false, false, false, false}
	patternB := [11]bool{false, false, false, false, true, false, true, true, true, false, true}
	for i := 0; i+11 <= size; i++ {
		matchA, matchB := true, true
		for j := 0; j < 11; j++ {
			if line[i+j] != patternA[j] {
				matchA = false
			}
			if line[i+j] != patternB[j] {
				matchB = false
			}
			if !matchA && !matchB {
				break
			}
		}
		if matchA {
			result += n3
		}
		if matchB {
			result += n3
		}
	}
	return result
}

// computePenalty 按规范的 4 条规则计算掩码惩罚分（越小越好）。
func computePenalty(modules [][]bool, size int) int {
	const (
		n1 = 3
		n2 = 3
		n3 = 40
		n4 = 10
	)
	result := 0

	// 规则 1 + 3：逐行、逐列
	for y := 0; y < size; y++ {
		result += penaltyForLine(modules[y], size, n1, n3)
	}
	for x := 0; x < size; x++ {
		col := make([]bool, size)
		for y := 0; y < size; y++ {
			col[y] = modules[y][x]
		}
		result += penaltyForLine(col, size, n1, n3)
	}

	// 规则 2：2×2 同色块
	for y := 0; y < size-1; y++ {
		for x := 0; x < size-1; x++ {
			c := modules[y][x]
			if c == modules[y][x+1] && c == modules[y+1][x] && c == modules[y+1][x+1] {
				result += n2
			}
		}
	}

	// 规则 4：深浅比例
	dark := 0
	for _, row := range modules {
		for _, cell := range row {
			if cell {
				dark++
			}
		}
	}
	total := size * size
	k := (abs(dark*20-total*10) + total - 1) / total // ceil
	if k > 0 {
		result += (k - 1) * n4
	}
	return result
}

// Matrix 二维码模块矩阵。
type Matrix struct {
	// Size 边长（模块数）。
	Size int
	// Modules modules[row][col]，true 表示深色模块。
	Modules [][]bool
}

// Build 生成 QR 模块矩阵（byte 模式 + 纠错等级 M + 版本 1–10）。
//
// 内容超过 213 字节时返回错误 —— 由调用方缩短内容，
// 而不是静默产出扫不出来的坏码。
func Build(text string) (*Matrix, error) {
	bytes := []int{}
	for _, b := range []byte(text) {
		bytes = append(bytes, int(b))
	}
	if len(bytes) > maxInputBytes {
		return nil, fmt.Errorf("qrcode: 内容过长（%d 字节，上限 %d 字节），请缩短内容",
			len(bytes), maxInputBytes)
	}
	version := pickVersion(len(bytes))
	if version == 0 {
		return nil, errors.New("qrcode: 内容超出可用容量")
	}

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
		return nil, err
	}
	drawCodewords(modules, isFunction, size, cw)

	// 选惩罚分最低的掩码。
	bestMask := 0
	bestPenalty := -1
	for mask := 0; mask < 8; mask++ {
		applyMask(modules, isFunction, size, mask)
		drawFormatBits(modules, isFunction, size, mask)
		p := computePenalty(modules, size)
		if bestPenalty < 0 || p < bestPenalty {
			bestPenalty = p
			bestMask = mask
		}
		applyMask(modules, isFunction, size, mask) // XOR 自逆，撤销
	}
	applyMask(modules, isFunction, size, bestMask)
	drawFormatBits(modules, isFunction, size, bestMask)

	return &Matrix{Size: size, Modules: modules}, nil
}

// RenderSVG 把文本渲染成内联 SVG 字符串。
//
// # 为什么用 SVG 而不是 canvas/PNG
//
// 登录页把它直接插进 DOM 即可，无需任何 JS 绘图调用；
// 且在任意缩放下都清晰（shape-rendering: crispEdges 保证模块边缘锐利）。
// 与参照的 renderQrSvg 同一做法。
//
// px 为 0 时用默认 158（参照的默认值）。
func RenderSVG(text string, px int) (string, error) {
	const margin = 4
	if px <= 0 {
		px = 158
	}
	m, err := Build(text)
	if err != nil {
		return "", err
	}
	dim := m.Size + margin*2

	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img">`,
		px, px, dim, dim))
	b.WriteString(fmt.Sprintf(`<rect width="%d" height="%d" fill="#ffffff"/>`, dim, dim))
	b.WriteString(`<path d="`)
	for y := 0; y < m.Size; y++ {
		for x := 0; x < m.Size; x++ {
			if m.Modules[y][x] {
				b.WriteString(fmt.Sprintf("M%d,%dh1v1h-1z", x+margin, y+margin))
			}
		}
	}
	b.WriteString(`" fill="#000000"/></svg>`)
	return b.String(), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
