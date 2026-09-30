package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestEveryProviderSectionResolves 逐节检查「配置段 → 解析后字段」这条链路是通的。
//
// # 为什么要有这条（它不是补丁，是补一类洞）
//
// 实测事故：config.json 里明明写了 `"lobsterai":{"enabled":true}` 与
// `"qoder":{"enabled":true}`，启动日志照旧打印「未启用（config 里 xxx.enabled
// 缺省为 false）」。根因是**两处整块缺失**：
//
//  1. qoder 段连 struct 定义与 json tag 都没有 → json.Unmarshal 把整个对象
//     当未知字段**静默丢弃**（encoding/json 对未知键不报错）
//  2. lobsterai / qoder 的解析赋值块没有 → 即便 struct 在，`XxxEnabled`
//     也永远是零值 false
//
// 两种缺失都**没有编译错误、没有运行时警告、没有测试变红**：
// 表现是"用户配了、程序说没配"，排查时只能靠人肉对比 config.go 与 main.go。
// 而这一节写对的写法，与上一节是完全同构的复制粘贴 ——
// 同构复制漏了一行，正是最容易发生又最难发现的一类缺陷。
//
// 所以这里用**反射枚举**而不是给 lobsterai/qoder 补两条断言：
// 枚举「所有带 enabled + auth_dir 的配置段」，对每一节断言
// 「解析后字段确实被赋值了」。将来再加第十一个上游，忘了写解析块就会红，
// 不需要谁记得回来补测试。
func TestEveryProviderSectionResolves(t *testing.T) {
	dir := t.TempDir()
	base := "./myauths"

	for _, sec := range providerSections(t) {
		t.Run(sec.name, func(t *testing.T) {
			fp := filepath.Join(dir, sec.key+".json")
			body := `{"auth_dir":"` + base + `","` + sec.key + `":{"enabled":true}}`
			if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(fp)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			cfg := reflect.ValueOf(c).Elem()

			// ---- 判据 1：解析后的 Enabled 必须为 true ----
			//
			// 这是本次事故的直接判据：段在、enabled=true，解析后字段却为 false
			// ⇒ 说明解析块缺失（或字段名与约定不符，反射也就找不到）。
			if f := cfg.FieldByName(sec.name + "Enabled"); !f.IsValid() {
				t.Fatalf("没有 `%sEnabled` 这个解析后字段 —— "+
					"新增上游时忘了在 Config 里加解析后字段（约定：%s + Enabled）",
					sec.name, sec.name)
			} else if !f.Bool() {
				t.Errorf("`%s.enabled=true` 未反映到 `%sEnabled`（得到 false）。\n"+
					"最可能的原因：normalize() 里缺少 `c.%sEnabled = c.%s.Enabled` 这一行 —— "+
					"配置被静默吞掉，启动日志会照旧打印「未启用」",
					sec.key, sec.name, sec.name, sec.name)
			}

			// ---- 判据 2：解析后的 AuthDir 必须落成 base 的**一级**子目录 ----
			//
			// 两级子目录 = 拼错来源（用已被改写的 c.AuthDir 而不是 c.AuthsBase），
			// 即「一个上游的目录长在另一个上游里面」：能读写、不报错，
			// 但语义错乱。这条断言把它钉死，不靠注释提醒。
			af := cfg.FieldByName(sec.name + "AuthDir")
			if !af.IsValid() || af.Kind() != reflect.String {
				t.Fatalf("没有 `%sAuthDir` 这个解析后字段", sec.name)
			}
			got := af.String()
			if got == "" {
				t.Errorf("`%s.auth_dir` 留空时 `%sAuthDir` 未被填充默认值（得到空串）—— "+
					"normalize() 里缺 `if ... == \"\" { = filepath.Join(c.AuthsBase, ...) }`",
					sec.key, sec.name)
			} else if rel, err := filepath.Rel(base, got); err != nil || rel == "." ||
				rel == ".." || strings.ContainsAny(rel, `/\`) {
				t.Errorf("`%sAuthDir` = %q 不是 %q 的一级子目录（rel=%q）。\n"+
					"若形如 %s/workbuddy/%s，说明用了已被改写的 c.AuthDir 而不是 c.AuthsBase",
					sec.name, got, base, rel, base, sec.key)
			}

			// ---- 判据 3：启用且未显式关 → 默认并入账号池 ----
			//
			// 与其它上游一致的语义；漏了赋值同样是 false（零值），
			// 表现是"账号池里空空如也"，而日志只说"未并入"。
			if pf := cfg.FieldByName(sec.name + "PoolAccounts"); pf.IsValid() {
				if pf.Kind() != reflect.Bool {
					t.Fatalf("`%sPoolAccounts` 不是 bool", sec.name)
				}
				if !pf.Bool() {
					t.Errorf("`%s` 已启用且未显式 pool_accounts=false，"+
						"`%sPoolAccounts` 应为 true（得到 false）—— "+
						"缺 `= %sEnabled && boolOr(%s.PoolAccounts, true)`",
						sec.key, sec.name, sec.name, sec.name)
				}
			}
		})
	}
}

// TestAbsentProviderSectionResolvesDisabled 反向：段缺席 ⇒ 解析后一律 false。
//
// 与上面那条互为补集。只测一个方向会漏掉「把零值当启用」的写法：
// 若解析写成 `c.XxxEnabled = boolOr(c.Xxx.Enabled, true)`（默认 true），
// 段缺席时就会凭空多出一个上游 —— 而现有部署的 config.json 里
// 恰恰**没有**这些新段，升级后行为会突然改变。
func TestAbsentProviderSectionResolvesDisabled(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(fp, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := reflect.ValueOf(c).Elem()

	for _, sec := range providerSections(t) {
		t.Run(sec.name, func(t *testing.T) {
			if f := cfg.FieldByName(sec.name + "Enabled"); !f.IsValid() || f.Bool() {
				t.Errorf("空配置下 `%sEnabled` 必须为 false（段缺席 = 不注册上游）", sec.name)
			}
			// 未启用时并池开关必须恒 false —— 不注册的上游不该在池里留痕迹
			if pf := cfg.FieldByName(sec.name + "PoolAccounts"); pf.IsValid() && pf.Bool() {
				t.Errorf("空配置下 `%sPoolAccounts` 必须为 false", sec.name)
			}
		})
	}
}

// providerSection 一个"上游配置段"的反射视图。
type providerSection struct {
	// name Config 里的结构体字段名（如 Lobsterai），解析后字段的前缀。
	name string
	// key JSON 里的键名（如 lobsterai）。
	key string
}

// providerSections 枚举所有"上游配置段"。
//
// 判据：JSON 对象字段里**同时**有 `enabled` 与 `auth_dir`。
// 这两个键是上游段的定义性特征（顶层 server/login/cooldown/... 都没有 auth_dir；
// session_sticky 有 enabled 但没有 auth_dir，故被自然排除）。
//
// 用结构而非硬编码清单，是为了让"新增上游忘了写解析块"必然变红 ——
// 硬编码清单只会保护清单里那几节，而漏的恰恰是清单外新增的那节。
func providerSections(t *testing.T) []providerSection {
	t.Helper()
	var out []providerSection
	ct := reflect.TypeOf(Config{})

	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		if f.Type.Kind() != reflect.Struct {
			continue
		}
		key := jsonKey(f)
		if key == "" {
			continue
		}
		if !hasJSONField(f.Type, "enabled", reflect.Bool) ||
			!hasJSONField(f.Type, "auth_dir", reflect.String) {
			continue
		}
		out = append(out, providerSection{name: f.Name, key: key})
	}
	if len(out) < 8 {
		// 自我校验：枚举机制本身失效（比如有人把 tag 写成了别的形态）时
		// 上面的循环会"零节通过"，看起来全绿 —— 那是最糟的假绿。
		//
		// ⚠ 这个下限要跟着"上游数量"改（删掉 mimo 后从 9 降到 8）。
		// 它是**枚举机制的存活检查**，不是上游清单 —— 清单由反射枚举出来，
		// 所以新增上游不需要改这里（那正是用结构而非硬编码清单的理由）。
		t.Fatalf("只枚举到 %d 个上游配置段（预期 ≥8：codearts/workbuddy_intl/loomy/"+
			"trae/cline/raccoon/lobsterai/qoder）—— 枚举判据可能已失效", len(out))
	}
	return out
}

// jsonKey 取字段的 JSON 键名；无 tag 或 tag 为 "-" 时返回空串。
func jsonKey(f reflect.StructField) string {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" || name == "-" {
		return ""
	}
	return name
}

// hasJSONField 判断结构体是否有某个 JSON 键、且类型匹配。
func hasJSONField(t reflect.Type, key string, kind reflect.Kind) bool {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if jsonKey(f) != key {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		return ft.Kind() == kind
	}
	return false
}
