package sysmon

import (
	"strings"
	"testing"
)

// TestStatusRedisGoesToNotesNotWarnings 「呈现混类」的直接回归测试（T-440）。
//
// # 为什么有这条（一次真实的误判）
//
// 改造前 `warnings` 里混装两类东西：
//
//	① 影响 verdict 的告警（磁盘/内存/CPU 超阈值、成功率低、采样不可用…）
//	② 纯信息性的配置事实（Redis 未启用 —— 有 noop 兜底、不影响链路）
//
// 而页面用**同一种样式**渲染两者 ⇒ 实测有人（包括提需求的那位）看到
// 「status=critical + 一行小字说 Redis 未启用」，就把 critical 归因给 Redis ✗，
// 于是去改一个**本来没问题**的判据（真凶是同页磁盘 97.37%）。
//
// ⇒ 本测试钉住三件事：
//
//	· 磁盘 97% **必须**驱动 critical（真凶不能被这轮改动弱化）
//	· Redis 那句**不在** warnings 里（否则又会诱导误判）
//	· 它在 **notes** 里（信息仍在，只是换了类）
func TestStatusRedisGoesToNotesNotWarnings(t *testing.T) {
	snap := Snapshot{
		CPUOK: true, CPUPct: 10,
		MemOK: true, MemPct: 20,
		DiskOK: true, DiskPct: 97.37, // ← 真凶
	}
	p := BuildStatus(snap, Sources{RedisMode: "noop"}) // Redis 降级

	if p.Service.Status != VerdictCritical {
		t.Errorf("service.status = %q，want %q —— 磁盘 97.37%% 必须独立驱动 critical"+
			"（否则这轮「分列」就从「Redis 误报警」变成「整页永不报警」）", p.Service.Status, VerdictCritical)
	}
	for _, w := range p.Service.Warnings {
		if strings.Contains(w, "Redis") {
			t.Errorf("warnings 里出现了 Redis 那句（%q）—— 它不驱动 verdict，"+
				"混在里面会诱导误判（已经发生过一次）", w)
		}
	}
	found := false
	for _, n := range p.Service.Notes {
		if strings.Contains(n, "Redis") {
			found = true
		}
	}
	if !found {
		t.Error("notes 里没有 Redis 那句 —— 信息不能丢，它只是换了分类")
	}
	// 反向：Redis 正常时不该有这条 notes（避免"永远显示"变成新噪音）
	p2 := BuildStatus(snap, Sources{RedisMode: "redis"})
	for _, n := range p2.Service.Notes {
		if strings.Contains(n, "Redis") {
			t.Errorf("Redis 已启用时仍出现 Redis 的 notes（%q）—— 那会变成恒定噪音", n)
		}
	}
}

// TestStatusWarningsStillCarryRealAlarms 真告警仍在 warnings（不能把水一起倒掉）。
func TestStatusWarningsStillCarryRealAlarms(t *testing.T) {
	snap := Snapshot{
		CPUOK: true, CPUPct: 10,
		MemOK: true, MemPct: 20,
		DiskOK: true, DiskPct: 10,
	}
	// 窗口内有错误请求 ⇒ 必须出现在 warnings（它是随负载变化的真告警）
	p := BuildStatus(snap, Sources{RedisMode: "redis", WindowRequests: 100, WindowErrors: 7})
	hit := false
	for _, w := range p.Service.Warnings {
		if strings.Contains(w, "错误请求") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("窗口内有 %d 个错误请求，但 warnings 里没有对应告警（实际 %v）—— "+
			"分列时不能把真告警也挪走", 7, p.Service.Warnings)
	}
}
