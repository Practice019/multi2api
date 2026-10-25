// settings_classify_test.go 「每个可写设置项都必须被归类」的守卫。
//
// # 为什么需要它（2026-10-26 实测踩到）
//
// 加 pool_revive_probe_hours 时，它只落进了 cmd/server 那份 needRestart 里，
// 没落进本包的 restartRequiredFields —— 而 GET /admin/settings 用的是后者。
// 结果：PUT 的响应说"需重启"，GET 的 restart_fields 里却没有这个键，
// 前端提示自相矛盾。
//
// 两边合并成一份之后，"两份漂移"这个形态没了；但**根本没被归类**这个形态还在：
// 往 Settings 加一个字段却忘了往两张清单加，页面就会显示成未知状态
// （既不说"立即生效"也不说"需重启"）。本用例守的就是它。
package admin

import (
	"reflect"
	"strings"
	"testing"
)

// readOnlySettingKeys 只读展示字段：它们不是可写项，不参与归类。
//
// ⚠ 字段挪出只读集合（变成可写）时，这里要同步删掉一行 —— 否则本守卫会静默
// 放过它。判据是 Settings 里该字段是否出现在 settingsUpdate 的 patch 清单里。
var readOnlySettingKeys = map[string]bool{
	"listen":      true,
	"auth_dir":    true,
	"config_path": true,
	"redis_mode":  true,
	"api_key_set": true,
}

// TestEveryWritableSettingIsClassified 每个可写设置项必须被归入两张清单之一。
func TestEveryWritableSettingIsClassified(t *testing.T) {
	known := map[string]bool{}
	for _, k := range runtimeFields {
		known[k] = true
	}
	for _, k := range restartRequiredFields {
		known[k] = true
	}

	typ := reflect.TypeOf(Settings{})
	seen := 0
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		key := strings.Split(tag, ",")[0]
		if key == "" || key == "-" || readOnlySettingKeys[key] {
			continue
		}
		seen++
		if !known[key] {
			t.Errorf("设置项 %q 既不在 runtimeFields 也不在 restartRequiredFields —— "+
				"前端拿不到「改了要不要重启」的提示，会显示成未知状态。"+
				"往对应那张清单加一行；若它其实是只读项，加进 readOnlySettingKeys", key)
		}
	}
	// 扫不到东西时上面的循环一次都不执行 —— 那本用例会退化成"永不失败"。
	if seen == 0 {
		t.Error("一个可写字段都没扫到 —— 判据失效（反射拿不到 json tag？）")
	}
	if len(restartRequiredFields) == 0 || len(runtimeFields) == 0 {
		t.Error("两张清单里有一张是空的 —— 归类形同虚设")
	}
}

// TestRestartRequiredFieldsReturnsCopy 返回的必须是副本。
//
// ⚠ 直接返回包级切片时，调用方一个 append 就会把包级状态改坏（容量够时原地写）——
// 那之后所有请求看到的清单都被污染了，且没有任何报错。
func TestRestartRequiredFieldsReturnsCopy(t *testing.T) {
	before := len(restartRequiredFields)
	got := RestartRequiredFields()
	if len(got) != before {
		t.Fatalf("长度不一致: %d vs %d", len(got), before)
	}
	// 改副本，包级不能跟着变。
	got[0] = "MUTATED"
	if restartRequiredFields[0] == "MUTATED" {
		t.Error("RestartRequiredFields 返回的是包级切片本体 —— 调用方改它会污染全局")
	}
}
