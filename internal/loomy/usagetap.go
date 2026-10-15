package loomy

import (
	"bytes"
	"io"
	"strconv"
)

// usagetap.go 从**已经在收的**对话响应里读出本次消耗的积分 —— 零额外请求。
//
// # 用户的想法（原话）—— 实测证明完全成立
//
//	「我们去调用那个模型接口的时候，它会不会返回额度信息呢？这样我们就可以
//	  直接获取了，就不需要再额外发请求了。就在已有的请求里，会不会已经有
//	  我们要的东西呢？」
//
//	「如果发一次请求就有返回额度的话，那就直接每次发请求之后，我们就把那个
//	  额度给更新掉就行了，把那个缓存给更新掉。」
//
// 实测（2026-10-15，两个账号，非流式 + 流式都测过）：
//
//	非流式 usage: {"prompt_tokens":32,"completion_tokens":8,"total_tokens":40,
//	               ...,"points_consumed":1}
//	流式 末块 usage: 同上，`points_consumed` 也在
//
// 而且与账本吻合：请求前 dailyBalance=1870 → 两次请求后 1868（正好 -2），
// 账本明细 `pointsActual:1, consumeSource:"daily"`。
//
// 于是"想更新缓存就必须发请求"这个困境消失了：
//
//	启动/按需查一次**绝对值** → 之后每次对话用 points_consumed 做减法
//	减到 ≤ 0 ⇒ 这个号从轮询池里出去（**零额外请求**）
//
// # ⚠ 这是热路径，三条纪律不能破
//
//	① **不改一个字节**：它是透传层，只旁听。转发出去的内容必须与不挂它时
//	   完全一致（有测试逐字节比对）。
//	② **不缓冲、不阻塞**：只在每次 Read 的返回值上扫一遍，最多留一小段
//	   "跨读边界"的尾巴。绝不把整条流攒起来 —— 那会把流式变成批式。
//	③ **读不出来也不影响转发**：解析失败/字段缺失/格式变了，一律静默。
//	   它是"顺手多拿一个数"，不是"转发的前置条件"。
type usageTap struct {
	rc  io.ReadCloser
	uid string
	// onConsumed 在流结束时回调一次（可能不回调 —— 上游没给就什么都不做）。
	onConsumed func(uid string, points int64)

	// carry 跨 Read 边界的尾巴：关键字可能被切成两次返回。
	carry []byte
	// last 本次响应里**最后**一次看到的 points_consumed。
	//
	// ⚠ 必须取"最后"而不是"累加"：实测流式里 usage 会出现在**两块** SSE
	// 里（finish_reason 那块与最后那块），值相同。累加会翻倍扣减 ——
	// 那会让账号的免费额度被过早判成用完。
	last   int64
	lastOK bool
	closed bool
}

const usageTapCarryMax = 64

func newUsageTap(rc io.ReadCloser, uid string, onConsumed func(string, int64)) *usageTap {
	return &usageTap{rc: rc, uid: uid, onConsumed: onConsumed}
}

func (t *usageTap) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.scan(p[:n])
	}
	return n, err
}

func (t *usageTap) Close() error {
	// ⚠ 只在 Close 时回调一次：Read 可能被调用很多次，而"这次请求消耗了多少"
	// 只有结束时才知道是最终值。
	if !t.closed {
		t.closed = true
		if t.lastOK && t.onConsumed != nil && t.uid != "" {
			t.onConsumed(t.uid, t.last)
		}
	}
	return t.rc.Close()
}

// scan 在**这次读到的字节**里找 `"points_consumed": <数字>`。
//
// ⚠ 不解析 JSON、不假设结构：只找这个键名，然后把它后面的数字读出来。
// 理由是这条路径必须"格式变了也不炸" —— 上游加字段、换外层包装都不影响。
func (t *usageTap) scan(b []byte) {
	// 把上次的尾巴与前缀接起来，避免关键字被读边界切开。
	var buf []byte
	if len(t.carry) > 0 {
		buf = append(append([]byte{}, t.carry...), b...)
	} else {
		buf = b
	}
	// 尾巴：留下最后 usageTapCarryMax 个字节给下一次。
	if len(buf) > usageTapCarryMax {
		t.carry = append([]byte{}, buf[len(buf)-usageTapCarryMax:]...)
	} else {
		t.carry = append([]byte{}, buf...)
	}

	const key = `"points_consumed"`
	for {
		i := bytes.Index(buf, []byte(key))
		if i < 0 {
			return
		}
		rest := buf[i+len(key):]
		// 跳过分隔（: 与空白）
		j := 0
		for j < len(rest) && (rest[j] == ':' || rest[j] == ' ' || rest[j] == '\t') {
			j++
		}
		k := j
		for k < len(rest) && (rest[k] == '-' || (rest[k] >= '0' && rest[k] <= '9')) {
			k++
		}
		if k > j {
			if v, err := strconv.ParseInt(string(rest[j:k]), 10, 64); err == nil {
				// ⚠ 覆盖而不是累加：见 last 字段的说明。
				t.last = v
				t.lastOK = true
			}
		}
		buf = rest
		if len(buf) == 0 {
			return
		}
	}
}
