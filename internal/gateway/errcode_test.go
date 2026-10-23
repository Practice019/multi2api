// errcode_test.go —— 锁住 kind → 出站 code 这张**对外契约**。
//
// # 本文件钉住什么
//
// 四件事，每件对应一种"静默破坏客户端"的方式：
//
//  1. 每个 ErrorKind 都有映射（新增 kind 忘了登记 → 客户端收到 unknown_error）。
//  2. code 的取值逐字锁定（改一个词 = 破坏客户端的分支，必须是一次显式提交）。
//  3. code 与内部 kind 名词**不重合**（内部重构不得泄漏成对外变更）。
//  4. 未知 kind 有稳定兜底，且不 panic（脏整数不该打穿请求路径）。
package gateway

import (
	"regexp"
	"testing"
)

// allKinds 是 ErrorKind 的**全量**取值清单。
//
// # 为什么在这里再写一遍（而不是从 errorclassifier 导出）
//
// 因为这张表要给"新加了 kind"这件事一个**编译期之外**的提醒：ErrorKind 是
// 无方法的常量集合，Go 没有反射手段枚举它们。所以这里显式列一遍，并由
// TestEveryKindValueIsListed 反向校验「0..ErrKindFatalClient 之间没有被跳过」——
// 这样漏登记会以测试失败的形式出现，而不是靠人记得改两处。
//
// 与 errorclassifier_test.go 的 TestErrorKindValuesAreStable 互补：那边钉
// 数字与 String()，这边钉出站 code。两处都要改，正是"内部枚举变更"应有的动作量。
var allKinds = []struct {
	Kind ErrorKind
	Name string // 给测试报错用的可读名（与 kind.String() 一致，便于日志对照）
}{
	{ErrKindNone, "none"},
	{ErrKindHardCredit, "hard_credit"},
	{ErrKindSoftRate, "soft_rate"},
	{ErrKindSessionDead, "session_dead"},
	{ErrKindNotFound, "not_found"},
	{ErrKindAuth, "auth"},
	{ErrKindServer, "server"},
	{ErrKindClient, "client"},
	{ErrKindContentBlocked, "content_blocked"},
	{ErrKindFatalClient, "fatal_client"},
}

// TestEveryDeclaredKindIsMapped 每个 kind 都必须在这张契约表里有一行。
//
// # 为什么这是最重要的一条
//
// 新增一个 ErrorKind（例如将来加 ErrKindTimeout）时，编译器**不会**要求你
// 更新映射表 —— 出站于是静默回落 unknown_error，而这条路径可能几个月才走到一次。
// 等真有客户端按 code 分支时才发现，已经是我们把"可重试"报成"不知道"了。
func TestEveryDeclaredKindIsMapped(t *testing.T) {
	if len(allKinds) != len(errorCodeByKind) {
		t.Fatalf("ErrorKind 有 %d 个，映射表有 %d 行 —— 新增 kind 必须在 errorCodeByKind 登记"+
			"（漏登记会让客户端收到 unknown_error，且不会有任何编译错误）",
			len(allKinds), len(errorCodeByKind))
	}
	seen := map[ErrorKind]bool{}
	for _, m := range errorCodeByKind {
		if seen[m.Kind] {
			t.Errorf("kind %d 在映射表里出现了两次 —— 后一行会静默覆盖前一行", int(m.Kind))
		}
		seen[m.Kind] = true
	}
	for _, k := range allKinds {
		if !seen[k.Kind] {
			t.Errorf("kind %s(%d) 没有出站 code —— 新增 kind 必须在 errorCodeByKind 登记", k.Name, int(k.Kind))
		}
	}
}

// TestEveryKindValueIsListed 反向校验：0..ErrKindFatalClient 之间没有"被跳过"的取值。
//
// 与上一条构成双向验证：只看 len() 相等时，新增两个 kind 又删掉两个旧 kind
// 仍能通过。逐值枚举才不会漏。
func TestEveryKindValueIsListed(t *testing.T) {
	listed := map[int]bool{}
	for _, k := range allKinds {
		listed[int(k.Kind)] = true
	}
	for v := 0; v <= int(ErrKindFatalClient); v++ {
		if !listed[v] {
			t.Errorf("ErrorKind 取值 %d 不在 allKinds 里 —— 测试清单漏登记（本文件失去约束力）", v)
		}
		if got := ErrorCodeForKind(ErrorKind(v)); !got.IsKnown() {
			t.Errorf("ErrorKind(%d) 映射到了非契约 code %q", v, got)
		}
	}
}

// TestErrorCodeForKind 表驱动：逐项锁定出站 code 的字面值。
//
// ⚠ 这个测试**故意**把字符串写死。有人想改某个 code 时，必须先改这里 ——
// 这就是"对外契约变更必须显式"的机制本身，不是重复。
func TestErrorCodeForKind(t *testing.T) {
	cases := []struct {
		name string
		kind ErrorKind
		want ErrorCode
		why  string
	}{
		{"成功/不认识", ErrKindNone, CodeUpstreamUnavailable, "出口上的 none 只可能是传输失败/空流/池子空，对客户端都是『稍后重试』"},
		{"额度耗尽", ErrKindHardCredit, CodeInsufficientQuota, "重试无用，客户端必须停止重打（否则额度白烧）"},
		{"限流", ErrKindSoftRate, CodeRateLimited, "等一会儿重试有意义"},
		{"会话已死", ErrKindSessionDead, CodeNoHealthyAccount, "需人工处理；对客户端就是『没有可用账号』"},
		{"模型不存在", ErrKindNotFound, CodeModelNotFound, "实测 404 几乎都是模型未注册，给可自修的 code"},
		{"凭证失效", ErrKindAuth, CodeNoHealthyAccount, "可自动续期是我们的运维事实，客户端只需要知道现在没号"},
		{"上游 5xx", ErrKindServer, CodeUpstreamUnavailable, "可重试的暂时性故障"},
		{"其他 4xx", ErrKindClient, CodeInvalidRequest, "号换遍了仍 4xx ⇒ 是请求本身的问题"},
		{"内容拦截", ErrKindContentBlocked, CodeContentBlocked, "换号无用，要改提示词"},
		{"确定性客户端错误", ErrKindFatalClient, CodeInvalidRequest, "参数/上下文超限，重试必然同样失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ErrorCodeForKind(c.kind)
			if got != c.want {
				t.Fatalf("ErrorCodeForKind(%s) = %q，want %q —— %s",
					c.kind, got, c.want, c.why)
			}
			if !got.IsKnown() {
				t.Errorf("%q 被映射出来却不算契约值 —— IsKnown 的清单漏了这个 code", got)
			}
		})
	}
}

// TestUnknownKindFallsBack 未知 kind 必须有稳定兜底，且不得 panic。
//
// # 为什么专门测"脏整数"
//
// ErrorKind 是 int 的具名类型，任何地方都写着 gateway.ErrorKind(n)。
// 一处脏值（反序列化、日志回放、将来某个上游算错）不该让整条错误路径 panic ——
// 那会把"上游返回了一个我们不认识的错误"升级成"网关崩溃"。
func TestUnknownKindFallsBack(t *testing.T) {
	for _, v := range []int{-1, 10, 42, 999, 1 << 30} {
		got := ErrorCodeForKind(ErrorKind(v))
		if got != CodeUnknown {
			t.Errorf("ErrorCodeForKind(%d) = %q，want %q —— 未知 kind 必须如实表达『我不知道』，"+
				"不能编一个看似合理的具体 code", v, got, CodeUnknown)
		}
	}
}

// knownKindCodeCollisions 是**已存在**、且本次刻意不改的 code/kind 重名。
//
// # 为什么允许例外，而不用"清零"一次了事
//
// 出站 code 是对外契约，而 content_blocked 已经在线上发了很久 ——
// internal/server 的降级测试就是按这个字面量断言的（degrade_test.go）。
// 把它改成客户端友好的名字（例：content_policy_blocked）必须与 handler、
// 文档、客户端一起动，属于独立的契约变更，不是本文件能顺手做掉的事。
//
// ⚠ 这个列表只能**缩短**：新增重名会被下面那条测试挡住，所以"以后又混进一个"
// 必须显式往这里加一行 —— 那就是一次需要解释的决定，而不是静默漂移。
var knownKindCodeCollisions = map[ErrorCode]bool{
	CodeContentBlocked: true, // ErrKindContentBlocked.String() == "content_blocked"
}

// TestCodesAreNotInternalKindNames 出站 code 不得与内部 kind 名词同名（已登记的例外除外）。
//
// # 这是"两套词汇不得混用"的载体
//
// 如果把 kind.String() 直接当 code 发出去，那么"把 ErrKindAuth 并进
// ErrKindClient"这种纯内部重构就会**静默改变**客户端看到的 code ——
// 内部语义一调整，外部契约跟着碎。
func TestCodesAreNotInternalKindNames(t *testing.T) {
	kindNames := map[string]bool{}
	for v := 0; v <= 64; v++ {
		kindNames[ErrorKind(v).String()] = true
	}
	for _, m := range errorCodeByKind {
		if !kindNames[string(m.Code)] {
			continue
		}
		if knownKindCodeCollisions[m.Code] {
			t.Logf("已知例外：code %q 与 kind 名同名（见 knownKindCodeCollisions 的说明）", m.Code)
			continue
		}
		t.Errorf("出站 code %q 与内部 kind 名词同名 —— 内部语义一调整，客户端契约就跟着碎。"+
			"code 需要一个面向客户端的名字（例：soft_rate → rate_limited）", m.Code)
	}
}

// TestCodesAreSnakeCase 所有 code 必须是小写 snake_case。
//
// # 为什么值得一条测试
//
// 客户端普遍用代码生成器/枚举比较来判 code；驼峰或大写在不同语言 SDK 里
// 会被归一化成不同形态，同一个 code 在两种语言里就认不出来了。
func TestCodesAreSnakeCase(t *testing.T) {
	re := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	all := map[ErrorCode]bool{CodeUnknown: true}
	for _, m := range errorCodeByKind {
		all[m.Code] = true
	}
	for c := range all {
		if !re.MatchString(string(c)) {
			t.Errorf("code %q 不是小写 snake_case", c)
		}
		if !c.IsKnown() {
			t.Errorf("code %q 由映射表产出却不是契约值 —— IsKnown 漏登记", c)
		}
	}
}

// TestIsKnownRejectsHandwritten 未登记的字符串不得被认成契约值。
//
// 这挡住的是"收敛之后又有人手写一个字面量"的回归：集成层可以用 IsKnown
// 在测试里把这类字面量扫出来（见 handler 侧的用法），而这里先固定语义。
func TestIsKnownRejectsHandwritten(t *testing.T) {
	cases := []struct {
		code ErrorCode
		want bool
	}{
		{CodeInvalidRequest, true},
		{CodeRateLimited, true},
		{CodeInsufficientQuota, true},
		{CodeUpstreamUnavailable, true},
		{CodeNoHealthyAccount, true},
		{CodeContentBlocked, true},
		{CodeModelNotFound, true},
		{CodeUnknown, true},
		// ⚠ "upstream_error" 从"手写字面量"升级为**契约值**了（主脑集成时定的）。
		//
		// 原计划是让 CodeUpstreamUnavailable 的线上值就叫 "upstream_unavailable"，
		// 但那是**已发布出去的**契约值 —— 既有客户端的 switch 里写的是
		// "upstream_error"，改它等于静默破坏它们的分支判断。
		//
		// 所以常量名保留 UpstreamUnavailable（语义更准），线上值回退成
		// "upstream_error"。这条测试随之更新：它现在**是**契约值。
		{"upstream_error", true},
		{"internal_error", false},
		{"", false},
		{"Invalid_Request", false},
	}
	for _, c := range cases {
		if got := c.code.IsKnown(); got != c.want {
			t.Errorf("ErrorCode(%q).IsKnown() = %v，want %v", c.code, got, c.want)
		}
	}
}

// TestErrorCodeString 具名类型的 String 与字面值一致（进 JSON 时不会多出前缀）。
func TestErrorCodeString(t *testing.T) {
	for _, m := range errorCodeByKind {
		if m.Code.String() != string(m.Code) {
			t.Errorf("ErrorCode(%q).String() = %q", m.Code, m.Code.String())
		}
	}
}
