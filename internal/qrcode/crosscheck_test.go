package qrcode

import (
	"os/exec"
	"strings"
	"testing"
)

// TestMatchesIndependentImplementation 与**独立实现**逐位比对。
//
// # 为什么这条最要紧（本轮实测的教训）
//
// 结构断言（尺寸对、finder 在、格式位 BCH 自洽）**全绿也不等于码对**：
// 数据位填充顺序、掩码应用范围、纠错交织任何一处错了，
// 矩阵看起来都"很像一个二维码"。唯一能证明对的是**与另一个实现比对**。
//
// 本仓的判据是"实测 > 推理"，所以这里真的调用 Python 的 `qrcode` 库
// （与我们的实现**完全无关**的一套代码），逐位比矩阵。
//
// ⚠ 必须**强制 byte 模式**：`qrcode.add_data` 默认会自动选最优模式，
// `HELLO` / `A` 这类全大写内容会被选成**字母数字模式**（更省位），
// 于是与我们（byte 模式）必然不同 —— 那是"两种不同编码"，不是缺陷。
// 我第一版比对脚本没强制模式，误报了一堆 DIFF，浪费了一轮排查。
//
// ⚠ 掩码也要对齐：两边各自按惩罚分选掩码，选出的可能不同
// （掩码不同但都合规）。所以按**每个掩码**比对，只要有**一个**掩码
// 逐位相同，就说明数据编码 + 纠错 + 交织 + 掩码算法全都对。
//
// Python 或 qrcode 库不可用时**跳过**（不让缺环境变成红）——
// 但会把跳过原因打出来，免得"静默跳过 = 没有守卫"。
func TestMatchesIndependentImplementation(t *testing.T) {
	py := findPython()
	if py == "" {
		t.Skip("找不到 python，跳过与独立实现的交叉验证")
	}
	if !pythonHasQRCode(py) {
		t.Skip("python 缺 qrcode 库，跳过交叉验证（pip install qrcode）")
	}

	texts := []string{
		"HELLO",
		"https://qoder.com/device/selectAccounts?challenge=abc&challenge_method=S256",
		"https://xiaohuanxiong.com/login/mp?code=0123456789abcdef0123456789abcdef",
		"A",
		strings.Repeat("z", 100),
	}

	for _, text := range texts {
		// 我方：8 个掩码的矩阵签名
		mine := map[string]bool{}
		for mask := 0; mask < 8; mask++ {
			mine[matrixWithMask(t, text, mask)] = true
		}
		// 对方：8 个掩码的矩阵签名
		theirs, err := pythonMatrices(py, text)
		if err != nil {
			t.Fatalf("调用 python 失败: %v", err)
		}
		matched := false
		for _, sig := range theirs {
			if mine[sig] {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("文本 %q：8 个掩码里没有任何一个与独立实现逐位相同 —— "+
				"数据编码/纠错/交织/掩码算法有一处不对", truncateForMsg(text))
		}
	}
}

func truncateForMsg(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "…"
}

func findPython() string {
	for _, name := range []string{"python", "python3", "py"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func pythonHasQRCode(py string) bool {
	cmd := exec.Command(py, "-c", "import qrcode")
	return cmd.Run() == nil
}

// pythonMatrices 让 Python 的 qrcode 库输出 8 个掩码的矩阵签名。
//
// 强制 byte 模式 + border=0（对齐我们的矩阵形态）。
func pythonMatrices(py, text string) ([]string, error) {
	const script = `
import sys, qrcode
from qrcode.util import QRData, MODE_8BIT_BYTE
text = sys.stdin.buffer.read().decode('utf-8')
for mask in range(8):
    q = qrcode.QRCode(error_correction=qrcode.constants.ERROR_CORRECT_M, border=0, mask_pattern=mask)
    q.add_data(QRData(text.encode('utf-8'), mode=MODE_8BIT_BYTE))
    q.make(fit=True)
    m = q.get_matrix()
    sys.stdout.write(''.join('1' if c else '0' for row in m for c in row) + '\n')
`
	cmd := exec.Command(py, "-c", script)
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var sigs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			sigs = append(sigs, line)
		}
	}
	return sigs, nil
}
