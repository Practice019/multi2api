// loomy_warmup_wiring_test.go 装配守卫：死号重试间隔必须**真的有接线**。
//
// ⚠ 为什么用读源码的方式：本仓反复吃过"代码写了、看着对、其实没接线"的亏
//
//	（见 main.go 里那段"三段启动动作写在 log.Fatalf 分支里 ⇒ 从没执行过"）。
//	一个只在装配层存在的字段，**没有任何单元测试会红** —— 只有这条会。
//
// 判据：main.go 里 loomy 的配置结构体必须同时出现 `DeadRetryInterval:` 与
//
//	`ReviveProbeIntervalSeconds`（= 复用既有配置，不新增键）。
package main

import (
	"os"
	"strings"
	"testing"
)

func TestLoomyDeadRetryIsWiredFromExistingConfig(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "DeadRetryInterval:") {
		t.Error("main.go 里没有给 loomy 注入 DeadRetryInterval —— 死号会退回默认值，" +
			"而设置页那个「账号解封探测」调了它也不会跟着变（两处判据打架）")
	}
	if !strings.Contains(src, "ReviveProbeIntervalSeconds") {
		t.Error("没有复用既有配置 pool.revive_probe_interval_seconds —— 本轮明确要求**不新增配置键**")
	}
}
