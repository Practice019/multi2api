// armor_bench_test.go —— L6「留给测量」的那一项：检测在热路径的实测成本。
//
// design-notes 的 3.3 节把检测成本标为"留给测量"（前置不做微优化）。
// 这里测两个数：
//
//	BenchmarkDetectArmorMiss   纯检测扫一遍 135 KB 文本（未命中 = 最坏情况，要把所有模式走完）
//	BenchmarkJudgeBlockMiss    判决入口（含 JSON 解析出提示词文本），加载荷量级的 body
//
// ⚠ 它**不参与优化决策**：数字只用来核对"毫秒级"这个承诺，以及回答
//
//	"模式条目数 × body 的线性乘积在什么量级"（见 Q4）。
package prompt

import (
	"strings"
	"testing"
)

// benchBody 造一份 ~135 KB 的请求体（实测真实请求的载荷量级）。
func benchBody() []byte {
	one := "请把下面这段话翻成英文并解释其中的技术细节，注意保留专有名词：" +
		"the gateway forwards the request to the upstream provider after rewriting the system prompt. "
	sb := strings.Builder{}
	sb.WriteString(`{"messages":[{"role":"system","content":"你是客服"},{"role":"user","content":"`)
	for sb.Len() < 135<<10 {
		sb.WriteString(one)
	}
	sb.WriteString(`"}]}`)
	return []byte(sb.String())
}

func BenchmarkDetectArmorMiss(b *testing.B) {
	pats := ArmorPatterns(ArmorConfig{})
	text, _ := ArmorBodyText(benchBody())
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, hit := DetectArmor(text, pats); hit {
			b.Fatal("这份基准文本不该命中（命中说明模式太宽，误杀面在扩大）")
		}
	}
}

func BenchmarkJudgeBlockMiss(b *testing.B) {
	cfg := ArmorConfig{Mode: ArmorBlock, BlockScope: ArmorScopeAll}
	body := benchBody()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, d := Judge(cfg, Target{Upstream: "u"}, body)
		if d.Action != ActionPass || len(out) != len(body) {
			b.Fatal("未命中必须原样放行（同一长度）")
		}
	}
}
