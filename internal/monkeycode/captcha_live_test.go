// captcha_live_test.go 真机验证 cap.js 的 PoW 实现。
//
// # 为什么必须有这条
//
// PoW 是**纯计算 + 一个校验端点**，写错时算出的 nonce 永远不被接受，
// 而 redeem 只回一句 `success:false` —— 指不到是 capI 写错还是位比较写错。
// 所以要么真跑一遍拿到 token，要么这条测试红。
package monkeycode

import (
	"context"
	"testing"
	"time"
)

// TestCapIDeterministic capI 必须是纯函数（同一输入同输出）。
func TestCapIDeterministic(t *testing.T) {
	a := capI("tokenabc1", 32)
	b := capI("tokenabc1", 32)
	if a != b {
		t.Fatalf("同一输入得到不同结果：%s != %s", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("长度应为 32，实际 %d", len(a))
	}
}

// TestCapIDiffers capI 对不同输入必须不同。
func TestCapIDiffers(t *testing.T) {
	seen := map[string]string{}
	for i := 1; i <= 50; i++ {
		v := capI("token"+itoa(i), 32)
		if prev, ok := seen[v]; ok {
			t.Fatalf("输入 %d 与 %s 碰撞", i, prev)
		}
		seen[v] = "token" + itoa(i)
	}
}

// TestSolveOneBitLevel 位级比较必须与目标比特完全吻合。
//
// 自造一个容易的目标（全零前缀），解出来后再验一遍哈希：
// 这是**唯一**可信的判据 —— "solveOne 返回了数字"不代表它对。
func TestSolveOneBitLevel(t *testing.T) {
	// 目标：前 2 个 hex 字符为 0（= 8 bit 全零），命中概率 1/256。
	const target = "00"
	const salt = "salt-for-test"
	n, err := solveOne(salt, target)
	if err != nil {
		t.Fatalf("求解失败: %v", err)
	}
	// 独立验证：用解出来的 nonce 重算，前 8 bit 必须为 0。
	h := sha256Of(salt + itoa64(n))
	if h[0] != 0x00 {
		t.Fatalf("解 %d 的哈希首字节是 %#x，不是 0x00", n, h[0])
	}
}

// TestSolveOneOddLength 目标长度为奇数时也要正确（余数位要掩码）。
func TestSolveOneOddLength(t *testing.T) {
	// 3 个 hex 字符 = 12 bit；最高 12 位必须全零，命中概率 1/4096。
	const target = "000"
	const salt = "odd-test"
	n, err := solveOne(salt, target)
	if err != nil {
		t.Fatalf("求解失败: %v", err)
	}
	h := sha256Of(salt + itoa64(n))
	// 12 bit = 1 整字节 + 高 4 位
	if h[0] != 0x00 {
		t.Fatalf("第 1 字节应为 0，实际 %#x", h[0])
	}
	if h[1]&0xF0 != 0x00 {
		t.Fatalf("第 2 字节高 4 位应为 0，实际 %#x", h[1])
	}
}

// TestLiveCaptcha 真机过一遍完整 PoW，拿到 captcha_token。
func TestLiveCaptcha(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	dir := liveAuthDir()
	list, err := LoadDir(dir)
	if err != nil || len(list) == 0 {
		t.Skipf("无真实凭证（%s），跳过", dir)
	}
	a := list[0]
	p := New(Config{AuthDir: dir})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	started := time.Now()
	token, err := p.client.solveCaptcha(ctx, a)
	if err != nil {
		t.Fatalf("PoW 失败: %v", err)
	}
	if token == "" {
		t.Fatal("拿到空 captcha_token")
	}
	t.Logf("PoW 通过，耗时 %v，token 长度 %d（region=%s）", time.Since(started), len(token), a.RegionOrDefault())
}
