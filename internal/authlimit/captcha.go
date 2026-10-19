package authlimit

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 出题参数与默认值。
const (
	// defaultCaptchaTTL 题面有效期。ttl <= 0 时用它。
	//
	// 5 分钟是"人"的尺度：打开注册页、看两眼、敲答案，一两分钟足够；而对
	// "攒一批题面、离线慢慢识别"的脚本来说，5 分钟把它的窗口压得很死。
	defaultCaptchaTTL = 5 * time.Minute

	// operandMax 加数上限：a、b 都取 1..operandMax，于是和落在 2..18。
	// 只要两位数，字模就只需要十个数字。
	operandMax = 9

	// glyphRows 字模行数（列数固定 5，见 glyphs）。
	glyphRows = 7
)

// pendingCaptcha 一道待校验的题面。
//
// ⚠ 答案只活在这里，**不出现在给前端的任何字节里** —— 这是本文件的核心约束，
// 见 drawQuestion。
type pendingCaptcha struct {
	answer int
	issued time.Time
}

// Captcha 零依赖的本地人机校验：出一道算术题，把答案留在内存里等对。
//
// # 它替代什么
//
// new-api 在 /register、/login 后面挂了 middleware/turnstile-check.go ——
// 把浏览器拿到的 turnstile token POST 给 Cloudflare 的 siteverify 接口。那条路
// 要外网、要一对 site/secret key、要用户能访问 challenges.cloudflare.com。
// 本仓是单机自用的网关，这三样都不成立，所以换成本地自绘的算术题。
//
// # 它能挡住什么、挡不住什么（别高估）
//
// 挡得住：拿 curl 循环批量注册/登录的脚本 —— 它们不会去看图。
// 挡不住：真去读图的东西（图像识别、或者把图转发给人打码）。所以它只是**第
// 三道闸**，主防线是 IP 维度的滑动窗口限速（limit.go）。本包不假装它是主防线。
//
// 并发：一把锁。出题/校验都在匿名端点上，频率低，锁粒度无所谓。
type Captcha struct {
	ttl        time.Duration
	maxPending int

	mu    sync.Mutex
	items map[string]pendingCaptcha
}

// NewCaptcha 造一个出题器。ttl <= 0 取 defaultCaptchaTTL。
//
// maxPending 是**待校验题面的条数上限**，防的是"一直取题不作答"把内存撑爆
// （取题这个动作在匿名端点上，谁都能打）。maxPending <= 0 表示不设上限，
// 只适合测试或单机自用；对外暴露的注册页必须给正数。
func NewCaptcha(ttl time.Duration, maxPending int) *Captcha {
	if ttl <= 0 {
		ttl = defaultCaptchaTTL
	}
	return &Captcha{
		ttl:        ttl,
		maxPending: maxPending,
		items:      make(map[string]pendingCaptcha),
	}
}

// Issue 出一道题，返回题面 ID（交给前端回传）与 SVG 图片。
//
// now 由调用方给，便于测试推进时间；生产用 time.Now()。
func (c *Captcha) Issue(now time.Time) (id string, svg string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// 先清过期的：条数上限只有在"表里全是活的"时才有意义，否则一批过期题面
	// 会把新题面顶在门外。
	c.cleanupLocked(now)
	if c.maxPending > 0 && len(c.items) >= c.maxPending {
		// 满了就**不发**（返回空 ID），而不是顶掉最旧的一道。
		// 顶掉最旧那道是静默失败：受害者输对了答案却被拒，最难看懂的一类故障；
		// 不发题是响亮的失败，调用方直接回 429/503，用户知道稍后再试。
		return "", ""
	}

	a := randInt(operandMax) + 1
	b := randInt(operandMax) + 1
	prompt := fmt.Sprintf("%d+%d=?", a, b)

	id = randHex(8)
	c.items[id] = pendingCaptcha{answer: a + b, issued: now}
	return id, drawQuestion(prompt)
}

// Verify 校验答案。**一次性**：无论对错，这次校验都把题面消费掉。
//
// # 为什么答错也要消费
//
// 答案空间只有 2..18 共 17 个取值。若"答错不消费"，同一道题就等于给出 17 次
// 免费尝试 —— 一个 for 循环就能过，这层人机校验彻底失效。消费掉之后每次尝试
// 都得重新取题，而取题本身受 IP 限速约束（limit.go）。于是每猜一次的成本
// = 一次取题 + 一次上报，比直接爆破口令还贵。
//
// 代价是"手滑打错一个字得重来" —— 对真人是可接受的，对人机校验而言这是
// 标准做法。
//
// # 另外两条
//
//   - 过期即废：与是否答对无关（见 pendingCaptcha 与 defaultCaptchaTTL）。
//   - 答案按**整数**比：先 TrimSpace 再 Atoi。于是 " 7 "、"7"、"07" 都算对
//     —— 我们比的是那个和，不是它的写法，读图的人不该因为多打一个空格被拒。
//     也正因如此不需要定长比较：一次性语义把爆破窗口压到 1 次尝试。
func (c *Captcha) Verify(id, answer string, now time.Time) bool {
	if c == nil {
		return false
	}
	id = strings.TrimSpace(id)

	c.mu.Lock()
	item, ok := c.items[id]
	if ok {
		delete(c.items, id)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}

	if now.Sub(item.issued) >= c.ttl {
		return false
	}
	got, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil {
		return false
	}
	return got == item.answer
}

// Cleanup 清掉已过期的题面，返回清掉几条。由调用方按节拍调（与 Limiter.Prune
// 同一套路），别指望 Issue/Verify 顺手清得干净 —— 它们只清自己那条路径上的。
func (c *Captcha) Cleanup(now time.Time) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cleanupLocked(now)
}

// cleanupLocked 调用方必须持锁。
func (c *Captcha) cleanupLocked(now time.Time) int {
	removed := 0
	for id, item := range c.items {
		if now.Sub(item.issued) >= c.ttl {
			delete(c.items, id)
			removed++
		}
	}
	return removed
}

// 出图参数。
const (
	glyphScale = 8 // 字模每个格子的边长（px）
	glyphCols  = 5 // 字模列数
)

// drawQuestion 把题面（如 "7+3=?"）画成一张 SVG。
//
// # 核心约束：题面字符、答案，一律不能以可读文本出现
//
// 需求是"挡住脚本批量注册"。而 `<text>7+3</text>` 用一条正则
// （`<text[^>]*>([^<]*)</text>`）就能读出来，等于没挡；`aria-label` / `<title>`
// 里的答案更省事，一把 grep 就够。所以：
//
//   - 题面每个字符先查 5×7 点阵字模，再变成**矩形折线坐标**
//     （`M<x> <y>h<w>v<h>h-<w>Z`，同一行相邻的亮格合并成一条子路径，
//     体积约为逐格写的三分之一），整张图里没有任何文本节点；
//   - 不写 aria-label、不写 <title>/<desc>：它们的语义就是"这里有一段人能直接
//     读的话"，留着这个位置迟早有人把答案填进去。题面说明（"请输入计算结果"）
//     属于调用方的页面，写在我们给的 <label> 里，不必进图；
//   - 答案（那个和）**从不进入** drawQuestion —— 它只在 Captcha.items 里。
//
// 出参全是本包自己生成的数字与符号，没有用户输入，所以不需要 XML 转义。
//
// # 同一道题每次画得都不一样（否则人工解一次就能建表）
//
// 加数只有 operandMax × operandMax = 81 种组合。如果同一道题永远渲染成同一张图，
// 那么人工解过一次 "7+3=?" 就得到了"这张图 = 10"这条记录，之后脚本按图查表即可
// —— 这层人机校验就白装了。所以每次出题都重新随机：每个字符的纵向偏移、字距、
// 左边距、上下留白，外加 2~4 条灰色干扰线。人靠字形连成的块来认，不受影响。
//
// 不画那种"密密麻麻的噪点"：噪点的形状也是可统计的，真正省事又有效的是让
// **同一道题不重复**，而不是把图弄花（弄花还会连带把人一起挡住）。
func drawQuestion(prompt string) string {
	const glyphW = glyphCols * glyphScale
	const glyphH = glyphRows * glyphScale

	padX := 6 + randInt(9)  // 6..14
	gap := 3 + randInt(9)   // 3..11
	padY := 10 + randInt(5) // 10..14

	var d strings.Builder
	x := padX
	for _, ch := range prompt {
		// 纵向抖 ±4：字还是那几行亮格连成的块，人一眼认得，但"题面像素模板"
		// 不再固定。
		y := padY + 4 - randInt(3)*4 // padY-4 / padY / padY+4
		appendGlyph(&d, ch, x, y, glyphScale)
		x += glyphW + gap
	}
	width := x - gap + padX
	height := padY + 4 + glyphH + 6

	// 干扰线：随机端点，横跨整图。
	var noise strings.Builder
	for i := 0; i < 2+randInt(3); i++ {
		fmt.Fprintf(&noise, "M%d %dL%d %d", randInt(width), randInt(height), randInt(width), randInt(height))
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`,
		width, height, width, height)
	b.WriteString(`<rect width="100%" height="100%" fill="#f2f4f7"/>`)
	fmt.Fprintf(&b, `<path d="%s" stroke="#c9ced6" stroke-width="2" fill="none"/>`, noise.String())
	fmt.Fprintf(&b, `<path d="%s" fill="#1f2933"/>`, d.String())
	b.WriteString(`</svg>`)
	return b.String()
}

// appendGlyph 把 ch 的字模追加成 path 坐标，落在左上角 (x0, y0)、格子边长 scale
// 的方格里。字模里没有的字符直接跳过 —— 少画一个字符，强过画出个空缺方框
// （能画出什么由 prompt 的构造保证，见 Issue）。
func appendGlyph(d *strings.Builder, ch rune, x0, y0, scale int) {
	rows, ok := glyphs[ch]
	if !ok {
		return
	}
	for r, row := range rows {
		c := 0
		for c < len(row) {
			if row[c] != '#' {
				c++
				continue
			}
			start := c
			for c < len(row) && row[c] == '#' {
				c++
			}
			w := (c - start) * scale
			fmt.Fprintf(d, "M%d %dh%dv%dh-%dZ", x0+start*scale, y0+r*scale, w, scale, w)
		}
	}
}

// glyphs 5×7 点阵字模（'#' 落笔、'.' 留白）。只画一道算术题要用的字符。
//
// 十个数字都留着（哪怕加数上限调大也不至于要改字模），但**不多画任何用不上的
// 字符**：字模是手写的，每多一个字形就多一处可能画错的地方。行数由 glyphRows
// 约束，列数固定 glyphCols —— 这两条有测试守着（改坏了会画出错位的字）。
var glyphs = map[rune][glyphRows]string{
	'0': {
		".###.",
		"#...#",
		"#...#",
		"#...#",
		"#...#",
		"#...#",
		".###.",
	},
	'1': {
		"..#..",
		".##..",
		"..#..",
		"..#..",
		"..#..",
		"..#..",
		".###.",
	},
	'2': {
		".###.",
		"#...#",
		"....#",
		"...#.",
		"..#..",
		".#...",
		"#####",
	},
	'3': {
		".###.",
		"#...#",
		"....#",
		"..##.",
		"....#",
		"#...#",
		".###.",
	},
	'4': {
		"...#.",
		"..##.",
		".#.#.",
		"#..#.",
		"#####",
		"...#.",
		"...#.",
	},
	'5': {
		"#####",
		"#....",
		"####.",
		"....#",
		"....#",
		"#...#",
		".###.",
	},
	'6': {
		"..##.",
		".#...",
		"#....",
		"####.",
		"#...#",
		"#...#",
		".###.",
	},
	'7': {
		"#####",
		"....#",
		"...#.",
		"..#..",
		".#...",
		".#...",
		".#...",
	},
	'8': {
		".###.",
		"#...#",
		"#...#",
		".###.",
		"#...#",
		"#...#",
		".###.",
	},
	'9': {
		".###.",
		"#...#",
		"#...#",
		".####",
		"....#",
		"...#.",
		".##..",
	},
	'+': {
		".....",
		"..#..",
		"..#..",
		"#####",
		"..#..",
		"..#..",
		".....",
	},
	'=': {
		".....",
		".....",
		"#####",
		".....",
		"#####",
		".....",
		".....",
	},
	'?': {
		".###.",
		"#...#",
		"....#",
		"...#.",
		"..#..",
		".....",
		"..#..",
	},
}

// randInt 返回 [0, n) 的随机数（n <= 0 时返回 0）。用 crypto/rand 而不是
// math/rand，因为这里有两处**必须**不可预测：
//
//   - 出题序列：能预测下一道题是什么，就等于不用看图也知道答案；
//   - 题面 ID：能预测 ID，就能拿别人的 ID 反复 Verify 把它烧掉（Verify 是
//     一次性的，烧掉 = 让那个人永远过不了校验）。
//
// n 只有十几，2^64 取模的偏差小到不可用（~1e-17），不做拒绝采样。
//
// ⚠ crypto/rand 失败意味着这台机器给不出不可预测的随机数。此时**不能**退化
// 成时间戳接着跑 —— 那正是攻击者要的可预测性。直接 panic，与 internal/account
// 的 newID 同一取舍（那里写的是"账号系统在无法保证随机性的环境里继续跑，
// 本身就是错的"）。
func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("authlimit: 无法生成随机数: " + err.Error())
	}
	return int(binary.BigEndian.Uint64(buf[:]) % uint64(n))
}

// randHex 生成 n 字节的十六进制串（题面 ID 用，前缀 c- 便于在日志里认出来）。
func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("authlimit: 无法生成随机数: " + err.Error())
	}
	return "c-" + hex.EncodeToString(buf)
}
