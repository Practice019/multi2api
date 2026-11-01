package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCfg 把一份 JSON 写进临时配置并 Load 出来。
func writeCfg(t *testing.T, body string) *Config {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestStrictModelPrefixHandwrittenKey 手写位置（features.strict_model_prefix）必须生效。
func TestStrictModelPrefixHandwrittenKey(t *testing.T) {
	c := writeCfg(t, "{\"listen\":\":9999\",\"api_key\":\"k\",\"features\":{\"strict_model_prefix\":true}}")
	if !c.StrictModelPrefix {
		t.Error("features.strict_model_prefix=true 没被读出来")
	}
}

// TestStrictModelPrefixAdminWins 设置页那份（admin 段）优先 —— 本仓踩过两次的坑。
//
// 只在其中一处读写 ⇒ 表现是「设置页显示已开启、config.json 里也有、重启后回到 off」。
// 两个方向都钉：admin=true 压过未配的 features；admin=false 压过 features=true
// （后者尤其重要 —— 用户关掉开关后重启不能被手写的 true 重新打开）。
func TestStrictModelPrefixAdminWins(t *testing.T) {
	if c := writeCfg(t, "{\"listen\":\":9999\",\"api_key\":\"k\",\"admin\":{\"strict_model_prefix\":true}}"); !c.StrictModelPrefix {
		t.Error("admin.strict_model_prefix=true 没被读回来（设置页的值重启后丢失）")
	}
	if c := writeCfg(t, "{\"listen\":\":9999\",\"api_key\":\"k\",\"admin\":{\"strict_model_prefix\":false},\"features\":{\"strict_model_prefix\":true}}"); c.StrictModelPrefix {
		t.Error("admin 段显式 false 应压过 features 的 true —— 否则用户关掉后重启又被打开")
	}
}

// TestStrictModelPrefixDefaultsOff 没配 ⇒ 默认关（不能把用户的线上裸名调用打死）。
func TestStrictModelPrefixDefaultsOff(t *testing.T) {
	if c := writeCfg(t, "{\"listen\":\":9999\",\"api_key\":\"k\"}"); c.StrictModelPrefix {
		t.Error("没配时必须是关")
	}
}
