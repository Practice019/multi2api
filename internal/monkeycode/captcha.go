// captcha.go cap.js 的 PoW 求解 —— 签到前必须先过这一关。
//
// # 这是什么
//
// MonkeyCode 的签到端点要带一个 captcha_token，而那个 token 只能由
// cap.js 的**工作量证明**换到：
//
//	POST /api/v1/public/captcha/challenge  → {challenge:{c,s,d}, token}
//	  解 c 个子挑战（每个要找到满足目标哈希的 nonce）
//	POST /api/v1/public/captcha/redeem     → {success, token}
//	  这个 token 就是 captcha_token
//
// # 三个关键点（每个写错都会静默算出永不命中）
//
//  1. capI 是 FNV-1a 播种 + xorshift32，每一步都走 32 位截断
//  2. salt 构造：capI(token+b, s) 与 capI(token+b+"d", d)
//  3. **位级比较**：目标是 4*target.length 个比特，不是逐字节全等
package monkeycode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// 验证码端点。
const (
	pathCaptchaChallenge = "/api/v1/public/captcha/challenge"
	pathCaptchaRedeem    = "/api/v1/public/captcha/redeem"
)

// captchaChallenge challenge 端点的返回。
type captchaChallenge struct {
	Challenge struct {
		C int `json:"c"` // 子挑战个数
		S int `json:"s"` // salt 长度（hex 字符数）
		D int `json:"d"` // 目标长度（hex 字符数）
	} `json:"challenge"`
	Token string `json:"token"`
}

// maxNonce PoW 的 nonce 上限。
//
// 实测正常在几千次内命中；给到 5000 万是余量，同时防止实现写错时死循环。
const maxNonce = int64(50_000_000)

// capI cap.js 的伪随机函数（逐字照抄线上实现）。
//
// # ⚠ 必须用 float64 累加，不能用 uint32 —— 这是签到一直失败的根因
//
// JS 原版：
//
//	p = (p | 0) ^ str.charCodeAt(i);
//	p = p + (((p << 1)|0) + ((p << 4)|0) + ((p << 7)|0) + ((p << 8)|0) + ((p << 24)|0));
//
// 关键：**加法那一步不截断**。JS 里 p 是 Number(float64)，溢出 32 位后
// 只是变成更大的数；只有 `<<` 才 ToInt32。
//
// 实测中间值（str="abc"）：
//
//	i=0  p0=-2128831068  shifted=-2635101304  p=-4763932372   ← 已超 32 位
//	i=1  p0=-468965042   shifted=1763236988   p=1294271946
//	i=2  p0=1294271913   shifted=-5148318878  p=-3854046965
//	最终 p >>> 0 = 440920331
//
// 我第一版把 p 写成 uint32，等于每一步都对加法结果取模 2^32 ——
// 序列从第二个字符起就与官方分叉，于是 PoW 算出的 nonce **永远不被接受**，
// 而 redeem 只回一句 `success:false`，指不到这里。
func capI(str string, length int) string {
	p := float64(2166136261)
	for i := 0; i < len(str); i++ {
		// (p | 0) ^ charCode：先 ToInt32 再异或。
		p0 := toInt32(p) ^ int32(str[i])
		// 五个子项各自 ToInt32，然后按 float64 相加（**不截断**）。
		shifted := float64(toInt32f(float64(p0) * 2))
		shifted += float64(toInt32f(float64(p0) * 16))
		shifted += float64(toInt32f(float64(p0) * 128))
		shifted += float64(toInt32f(float64(p0) * 256))
		shifted += float64(toInt32f(float64(p0) * 16777216))
		p = float64(p0) + shifted
	}
	// p >>> 0：取低 32 位当无符号。
	u := uint32(uint64(int64(p)) & 0xFFFFFFFF)
	next := func() uint32 {
		u = u ^ (u << 13)
		u = u ^ (u >> 17)
		u = u ^ (u << 5)
		return u
	}
	out := make([]byte, 0, length+8)
	for len(out) < length {
		v := strconv.FormatUint(uint64(next()), 16)
		for len(v) < 8 {
			v = "0" + v
		}
		out = append(out, v...)
	}
	return string(out[:length])
}

// toInt32 复刻 JS 的 ToInt32。
func toInt32(x float64) int32 { return toInt32f(x) }

// toInt32f 把 float64 截成有符号 32 位（JS ToInt32 的语义）。
//
// ⚠ 不能用 int32(x)：Go 的转换在溢出时是实现定义（amd64 上是截断，
// 但语言规范不保证）。也**不能**先转 int64 再截 —— x 可能超 int64 范围
// （乘 16777216 之后可达 1e17，仍在 int64 内，但为稳妥走 float→int64 的显式路径）。
func toInt32f(x float64) int32 {
	if x >= 9223372036854775808.0 || x < -9223372036854775808.0 {
		return 0 // 超出 int64：JS 会给 0（ToInt32 对 ±Inf/NaN 回 0）
	}
	return int32(uint32(int64(x) & 0xFFFFFFFF))
}

// solveOne 解一个子挑战，返回命中的 nonce。
//
// # 位级比较（最容易写错的地方）
//
// 目标 target 是 hex 串，代表 4*len 个比特。比较分两段：
//
//	整字节部分  h[0:a] 必须与目标前 a 字节全等（a = bits/8）
//	余数部分    h[a] 的高 l 位必须与目标第 a 字节的高 l 位相同（l = bits%8）
//
// 只比 h[0] 是最常见的错法 —— 那样算出的 nonce 永远不被上游接受，
// 而 redeem 只回一句 success:false，指不到这里。
func solveOne(salt, target string) (int64, error) {
	bits := 4 * len(target)
	a := bits / 8
	l := bits % 8
	pad := target
	if len(target)%2 != 0 {
		pad = target + "0"
	}
	want, err := hex.DecodeString(pad)
	if err != nil {
		return 0, fmt.Errorf("monkeycode: 目标哈希不是 hex: %w", err)
	}
	var mask byte
	if l > 0 {
		mask = byte(255 << (8 - l))
	}
	buf := make([]byte, 0, len(salt)+20)
	for nonce := int64(0); nonce < maxNonce; nonce++ {
		buf = append(buf[:0], salt...)
		buf = strconv.AppendInt(buf, nonce, 10)
		h := sha256.Sum256(buf)
		ok := true
		for i := 0; i < a; i++ {
			if h[i] != want[i] {
				ok = false
				break
			}
		}
		if ok && l > 0 && (h[a]&mask) != (want[a]&mask) {
			ok = false
		}
		if ok {
			return nonce, nil
		}
	}
	return 0, fmt.Errorf("monkeycode: PoW 求解超出上限（%d 次未命中）", maxNonce)
}

// solveCaptcha 走完整套 PoW，返回 captcha_token。
func (c *Client) solveCaptcha(ctx context.Context, a *Auth) (string, error) {
	host := a.Host()
	h := sessionHeaders(a)
	h.Set("Content-Type", "application/json")

	code, raw, err := c.doJSON(ctx, a, http.MethodPost, host+pathCaptchaChallenge, h, []byte("{}"))
	if err != nil {
		return "", err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return "", fmt.Errorf("monkeycode: challenge 返回 %d：%s", code, truncate(raw, 160))
	}
	var ch captchaChallenge
	if err := json.Unmarshal(raw, &ch); err != nil {
		return "", fmt.Errorf("monkeycode: 解析 challenge 失败：%w", err)
	}
	if ch.Token == "" || ch.Challenge.C <= 0 {
		return "", fmt.Errorf("monkeycode: challenge 内容异常：%s", truncate(raw, 160))
	}

	// 解 c 个子挑战。
	//
	// ⚠ 两个 salt 的构造必须逐字照做：
	//   salt = capI(token+b, s)
	//   tgt  = capI(token+b+"d", d)
	// b 从 1 开始递增（不是 0）。
	solutions := make([]int64, 0, ch.Challenge.C)
	for b := 1; b <= ch.Challenge.C; b++ {
		idx := strconv.Itoa(b)
		salt := capI(ch.Token+idx, ch.Challenge.S)
		tgt := capI(ch.Token+idx+"d", ch.Challenge.D)
		n, err := solveOne(salt, tgt)
		if err != nil {
			return "", err
		}
		solutions = append(solutions, n)
	}

	// redeem 换 token。
	body, err := json.Marshal(map[string]any{"token": ch.Token, "solutions": solutions})
	if err != nil {
		return "", err
	}
	code, raw, err = c.doJSON(ctx, a, http.MethodPost, host+pathCaptchaRedeem, h, body)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return "", fmt.Errorf("monkeycode: redeem 返回 %d：%s", code, truncate(raw, 160))
	}
	var rd struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &rd); err != nil {
		return "", fmt.Errorf("monkeycode: 解析 redeem 失败：%w", err)
	}
	if !rd.Success || rd.Token == "" {
		return "", fmt.Errorf("monkeycode: redeem 失败：%s", truncate(raw, 160))
	}
	return rd.Token, nil
}

// itoa 十进制转换（测试辅助，避免引 strconv 污染）。
func itoa(i int) string { return itoa64(int64(i)) }

// itoa64 int64 转十进制字符串。
func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// sha256Of 算 sha256（测试辅助）。
func sha256Of(s string) [32]byte { return sha256.Sum256([]byte(s)) }
