// regmachine_test.go 钉住「注册机导出 → 本仓导入」这条跨项目链路。
//
// # 为什么必须有这条测试
//
// 注册机的 auth 文件与本仓 Auth **字段名与类型都不同**，而 Go 的
// json.Unmarshal 遇到一个类型不符就整条失败。于是 session 明明在
// 文件里，导入却报「凭证文件里没有可用的 session」——
// 那个错误指向 session，实际原因是 balance 的类型。
//
// 用真实导出样本钉住，比读字段对照表可靠。
package monkeycode

import (
	"bytes"
	"encoding/json"
	"testing"
)

// regMachineSample 与注册机 data/monkeycode/*.json 逐字段同形（真机样本）。
const regMachineSample = `{
  "version": 1,
  "platform": "monkeycode",
  "userId": "002d3463-53d0-4d55-8fc5-ea08566c473c",
  "userid": "002d3463-53d0-4d55-8fc5-ea08566c473c",
  "name": "blackwellaudrey12",
  "region": "intl",
  "baseUrl": "https://monkeycode-ai.net",
  "session": "c3184876-a445-434a-84e4-15c73c5b3440",
  "imageId": "1b515a3a-2bd7-4f61-9aa0-5bdc7fde03b0",
  "plan": "basic",
  "balance": 200000,
  "dailyTokenBalance": 10000000,
  "dailyTokenLimit": 10000000,
  "role": "individual",
  "status": "active",
  "isBlocked": false,
  "modelsOk": true,
  "modelsCount": 11,
  "addedAt": 1792662432437,
  "expiresAt": 1795254432437,
  "hasPassword": true,
  "password": "Fwk@02dyp4vaQaRB"
}`

// TestParseAuthAcceptsRegMachineFormat 必须能直接解注册机的记录。
//
// 判据是"能不能读出 session + 昵称 + 区域"，而不是"没报错"：
// 只断言 err==nil 的话，一个字段全空的 *Auth 也能让测试变绿。
func TestParseAuthAcceptsRegMachineFormat(t *testing.T) {
	a, err := parseAuth([]byte(regMachineSample))
	if err != nil {
		t.Fatalf("注册机格式解析失败: %v", err)
	}
	if a.Session != "c3184876-a445-434a-84e4-15c73c5b3440" {
		t.Errorf("session 没解出来: %q", a.Session)
	}
	// region 决定基址与模型目录，错了会整天 404。
	if a.Region != RegionIntl {
		t.Errorf("region: got %q want %q", a.Region, RegionIntl)
	}
	// 注册机的 name → 本仓 nickname。
	if a.Nickname != "blackwellaudrey12" {
		t.Errorf("nickname: got %q want blackwellaudrey12", a.Nickname)
	}
	// userId（驼峰）→ userid。
	if a.UserID != "002d3463-53d0-4d55-8fc5-ea08566c473c" {
		t.Errorf("userid: got %q", a.UserID)
	}
	if a.Plan != "basic" {
		t.Errorf("plan: got %q", a.Plan)
	}
	if !a.Usable() {
		t.Error("导入后必须 Usable（判据是 session 非空）")
	}
}

// TestRegMachineNumberFieldsBecomeStrings 数值字段必须转成字符串。
//
// # 这是"整条 Unmarshal 失败"的根因
//
// 注册机的 balance 是**数字**，本仓 Auth.Balance 是 **string**。
// 直接把 JSON 解进 Auth 会让 balance 那里报类型错，
// 连带 session 都读不到 —— 表面上却像是"没有 session"。
func TestRegMachineNumberFieldsBecomeStrings(t *testing.T) {
	a, err := parseAuth([]byte(regMachineSample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if a.Balance != "200000" {
		t.Errorf("balance: got %q want \"200000\"", a.Balance)
	}
	if a.DailyToken != "10000000" {
		t.Errorf("daily_token: got %q want \"10000000\"", a.DailyToken)
	}
	if a.DailyTokenLimit != "10000000" {
		t.Errorf("daily_token_limit: got %q want \"10000000\"", a.DailyTokenLimit)
	}
}

// TestImportCredentialsAcceptsJSONL 一次粘贴多行 JSONL 必须全部导入。
//
// 注册机导出的是 `.jsonl`（一行一个对象、**没有数组括号**）。
// 若只支持单个 JSON，用户会看到「内容不是合法 JSON」——
// 而那文件正是注册机自己导出的。
func TestImportCredentialsAcceptsJSONL(t *testing.T) {
	// 三行：两个本上游 + 一个别的上游（真实导出会混源）。
	//
	// ⚠ line1 必须用**紧凑化**的 JSON：真实 JSONL 的每一行都是一个
	// 单行对象。直接用 regMachineSample（带缩进换行）会让 looksLikeJSONL
	// 看到首行是 "{" 就把它当单行对象，剩下的行变成非法内容。
	line1 := compactJSON(t, regMachineSample)
	line2 := `{"platform":"monkeycode","userId":"bbb","name":"第二号","region":"cn","session":"sess-2"}`
	line3 := `{"platform":"loomy","userid":"ccc","token":"别的上游的记录"}`
	pasted := line1 + "\n" + line2 + "\n" + line3

	p := New(Config{})
	creds, err := p.ImportCredentials(pasted)
	if err != nil {
		t.Fatalf("JSONL 导入失败: %v", err)
	}
	// loomy 那行必须被跳过，不能算失败也不能算成功。
	if len(creds) != 2 {
		t.Fatalf("期望导入 2 条（跳过 loomy），实际 %d", len(creds))
	}
	if creds[1].Nickname != "第二号" {
		t.Errorf("第二条昵称: got %q", creds[1].Nickname)
	}
}

// TestRegMachineToDiskRoundTrip 翻译后的文件必须能被本仓原生格式读回。
//
// # 为什么需要这条
//
// 导入写盘走的是 MarshalAuthFile（`{"auth":{…}}` 形态）。
// 若翻译出的 *Auth 缺 Usable 必需的字段，写出来的文件**下次启动读不回来** ——
// 表现是「导入成功，重启后账号消失」。
func TestRegMachineToDiskRoundTrip(t *testing.T) {
	a, err := parseAuth([]byte(regMachineSample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	_, raw, err := MarshalAuthFile(a, "")
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	// 用原生路径读回（关掉注册机分支也不影响，因为写出来的就是原生形态）。
	back, err := parseAuth(raw)
	if err != nil {
		t.Fatalf("写出的文件读不回来: %v", err)
	}
	if back.Session != a.Session || back.Region != a.Region || back.UserID != a.UserID {
		t.Errorf("往返丢字段: 原 %+v / 读回 %+v", a, back)
	}
}

// TestIsRegMachineDocRejectsOwnFormat 不能把本仓自己的文件误判成注册机格式。
//
// # 为什么这条重要
//
// isRegMachineDoc 排在 parseAuth 的最前面。若它对本仓自己的
// `{"auth":{...}}` 也返回 true，fromRegMachine 会把顶层当记录解析 →
// 找不到 session → **本仓自己写出的凭证全部读不回来**。
func TestIsRegMachineDocRejectsOwnFormat(t *testing.T) {
	for _, own := range [][]byte{
		[]byte(`{"auth":{"session":"abc","region":"cn"}}`),
		[]byte(`{"session":"abc","api_key":"oma_x"}`),
	} {
		if isRegMachineDoc(own) {
			t.Errorf("误判本仓格式为注册机格式: %s", string(own))
		}
	}
	// 而真正的注册机记录必须被认出来。
	if !isRegMachineDoc([]byte(regMachineSample)) {
		t.Error("注册机样本没被认出来")
	}
	_ = json.Valid
}

// TestImportMinimalRegMachineRow 极简行（只有 session+region+platform）必须能导入。
//
// # 为什么这条要单独钉
//
// 用户问过：「注册机导出的 json 文件内容还是很多，你保证这些都是必须的字段么」。
// 实测穷举的答案是：35 个字段里**只有 3 个是必需的**（session/region/platform），
// 另外 25 个本仓完全不读。
//
// 所以注册机侧配了 exportFields 精简到 9 个字段。
// 而本仓必须**同时收得下**极简与全量两种 ——
// 若只认全量，用户哪天手删字段就导不进去了；
// 若只认极简，注册机的老文件又导不进来。
func TestImportMinimalRegMachineRow(t *testing.T) {
	minimal := `{"platform":"monkeycode","session":"c3184876-a445-434a-84e4-15c73c5b3440","region":"intl"}`

	p := New(Config{})
	creds, err := p.ImportCredentials(minimal)
	if err != nil {
		t.Fatalf("极简行导入失败: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(creds))
	}

	// session 与 region 必须落到盘上（那是唯二影响行为的字段）。
	var a *Auth
	if err := json.Unmarshal(creds[0].Raw, &struct {
		A **Auth `json:"auth"`
	}{&a}); err != nil {
		t.Fatalf("解析落盘内容失败: %v", err)
	}
	if a == nil {
		t.Fatal("落盘内容里没有 auth 对象")
	}
	if a.Session == "" {
		t.Error("session 没落盘")
	}
	if a.Region != RegionIntl {
		t.Errorf("region: got %q want intl", a.Region)
	}
}

// TestImportFullRegMachineRowStillWorks 全量行（35 字段）也必须仍然能导入。
//
// 精简是**优化**，不是新契约。老文件（含明文 password、expiresAt 等）
// 在用户机器上已经存在，导入路径不能因此失效。
func TestImportFullRegMachineRowStillWorks(t *testing.T) {
	p := New(Config{})
	creds, err := p.ImportCredentials(compactJSON(t, regMachineSample))
	if err != nil {
		t.Fatalf("全量行导入失败: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(creds))
	}
	if creds[0].Nickname != "blackwellaudrey12" {
		t.Errorf("昵称: got %q", creds[0].Nickname)
	}
}

// compactJSON 把带缩进的 JSON 压成单行（模拟真实 JSONL 的每行形态）。
func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatalf("compact 失败: %v", err)
	}
	return buf.String()
}
