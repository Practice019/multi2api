package store

import (
	"errors"
	"strings"
	"testing"
)

// openTokenDB 开一个临时库并**显式**建 tokens 表。
//
// 为什么不用 Migrate() 顺带建：allModels() 里还没有 &Token{}（那是 A1 的登记动作），
// 所以本测试自己 AutoMigrate，不依赖登记顺序 —— 表结构由 token.go 自己负责。
func openTokenDB(t *testing.T) {
	t.Helper()
	openTempDB(t)
	if err := DB.AutoMigrate(&Token{}); err != nil {
		t.Fatalf("AutoMigrate(Token): %v", err)
	}
}

func newToken(t *testing.T, userID int, name string) *Token {
	t.Helper()
	tok := &Token{UserID: userID, Name: name, RemainQuota: 100}
	if err := CreateToken(tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return tok
}

// TestTokenGroupEncodingRoundTrip 分组编码三种形态的往返 + NormalizeGroupPolicy 的五件事。
func TestTokenGroupEncodingRoundTrip(t *testing.T) {
	// 编码三形态
	if got := EncodeTokenGroups(nil); got != "[]" {
		t.Fatalf("空列表应当编码成 []，实际 %q", got)
	}
	if got := EncodeTokenGroups([]string{"default"}); got != "default" {
		t.Fatalf("单分组应当编码成裸名，实际 %q", got)
	}
	t.Logf("编码 空=%q 单=%q 多=%q", EncodeTokenGroups(nil), EncodeTokenGroups([]string{"default"}), EncodeTokenGroups([]string{"a", "b"}))
	if got := EncodeTokenGroups([]string{"a", "b"}); got != "[\"a\",\"b\"]" {
		t.Fatalf("多分组应当编码成 JSON 数组，实际 %q", got)
	}
	// 解码：四种输入
	if got := NormalizeTokenGroups(""); len(got) != 1 || got[0] != "default" {
		t.Fatalf("空串应当解码成 [default]，实际 %v", got)
	}
	if got := NormalizeTokenGroups("[]"); len(got) != 0 {
		t.Fatalf("[] 应当解码成空列表，实际 %v", got)
	}
	if got := NormalizeTokenGroups("a,b"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("逗号形态解码错: %v", got)
	}
	if got := NormalizeTokenGroups("[\"a\",\"b\"]"); len(got) != 2 || got[0] != "a" {
		t.Fatalf("JSON 形态解码错: %v", got)
	}
	// 保序去重（顺序就是优先级，所以不能排序）
	if got := NormalizeTokenGroups("b,a,b"); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("保序去重错: %v", got)
	}
	// 往返
	for _, in := range [][]string{{"x"}, {"x", "y"}, {"x", "y", "z"}} {
		round := NormalizeTokenGroups(EncodeTokenGroups(in))
		if len(round) != len(in) {
			t.Fatalf("往返丢项: %v → %v", in, round)
		}
		for i := range in {
			if round[i] != in[i] {
				t.Fatalf("往返顺序变了: %v → %v", in, round)
			}
		}
	}
	// NormalizeGroupPolicy：多分组 ⇒ CrossGroupRetry；retry 夹到 0..10；状态码只留 100..599
	multi := &Token{Groups: []string{"a", "b"}, GroupSwitchCodes: []int{200, 99, 200, 600, 503}, GroupSwitchRetryCount: 99}
	multi.NormalizeGroupPolicy()
	if !multi.CrossGroupRetry {
		t.Fatal("多分组应当自动打开 CrossGroupRetry")
	}
	if multi.GroupSwitchRetryCount != 10 {
		t.Fatalf("retry 应当夹到 10，实际 %d", multi.GroupSwitchRetryCount)
	}
	if len(multi.GroupSwitchCodes) != 2 || multi.GroupSwitchCodes[0] != 200 || multi.GroupSwitchCodes[1] != 503 {
		t.Fatalf("状态码归一化错: %v", multi.GroupSwitchCodes)
	}
	if multi.Group != "[\"a\",\"b\"]" {
		t.Fatalf("多分组落库串错: %q", multi.Group)
	}
	// 空列表（非 auto）⇒ 落库串 "[]" 且不算 auto 的多分组（CrossGroupRetry 保持 false）
	empty := &Token{Groups: []string{}}
	empty.NormalizeGroupPolicy()
	if empty.Group != "[]" {
		t.Fatalf("显式空列表应当落库为 []，实际 %q", empty.Group)
	}
	if !empty.UsesAutoGroups() {
		t.Fatal("空优先级列表应当算 auto 模式")
	}
	// 回夹负数
	neg := &Token{GroupSwitchRetryCount: -5}
	neg.NormalizeGroupPolicy()
	if neg.GroupSwitchRetryCount != 0 {
		t.Fatalf("负 retry 应当夹到 0，实际 %d", neg.GroupSwitchRetryCount)
	}
}

// TestTokenMaskBoundaries 掩码三档边界：4 与 8 是各档的最后一个值，9 进下一档。
func TestTokenMaskBoundaries(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"a", "*"},
		{"abcd", "****"},
		{"abcde", "ab****de"},
		{"abcdefgh", "ab****gh"},
		{"abcdefghi", "abcd**********fghi"},
	}
	for _, c := range cases {
		got := MaskTokenKey(c.in)
		t.Logf("掩码 len=%2d in=%-6q out=%q", len(c.in), c.in, got)
		if got != c.want {
			t.Errorf("MaskTokenKey(%q) = %q，want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("x", TokenKeyLength)
	masked := MaskTokenKey(long)
	if len(masked) != 4+10+4 {
		t.Fatalf("48 位 Key 的掩码长度应当是 18，实际 %d（%q）", len(masked), masked)
	}
	if !strings.HasPrefix(masked, "xxxx") || !strings.HasSuffix(masked, "xxxx") {
		t.Fatalf("掩码首尾不对: %q", masked)
	}
	if strings.Contains(masked[4:14], "x") {
		t.Fatalf("中间 10 位必须是 *，实际 %q", masked)
	}
}

// TestTokenKeyGenerationAndParse Key 生成（48 位、无前缀、不可预测）与客户端剥壳规则。
func TestTokenKeyGenerationAndParse(t *testing.T) {
	k1, err := GenerateTokenKey()
	if err != nil {
		t.Fatalf("GenerateTokenKey: %v", err)
	}
	if len(k1) != TokenKeyLength {
		t.Fatalf("长度应当是 %d，实际 %d", TokenKeyLength, len(k1))
	}
	if strings.HasPrefix(k1, "sk-") {
		t.Fatal("库内 Key 不应带 sk- 前缀")
	}
	for i := 0; i < len(k1); i++ {
		if !strings.ContainsRune(tokenKeyChars, rune(k1[i])) {
			t.Fatalf("第 %d 位 %q 不在字符集内", i, k1[i])
		}
	}
	k2, err := GenerateTokenKey()
	if err != nil || k1 == k2 {
		t.Fatalf("两次生成必须不同（k1==k2=%v, err=%v）", k1 == k2, err)
	}
	// 剥壳：Bearer / sk- / -<channelId> 三种噪声都要剥掉
	parseCases := []struct{ in, want string }{
		{k1, k1},
		{"Bearer " + k1, k1},
		{"bearer " + k1, k1},
		{"sk-" + k1, k1},
		{"Bearer sk-" + k1, k1},
		{"Bearer sk-" + k1 + "-789", k1},
		{"  " + k1 + "  ", k1},
	}
	for _, c := range parseCases {
		if got := ParseTokenKey(c.in); got != c.want {
			t.Errorf("ParseTokenKey(%q) = %q，want %q", c.in, got, c.want)
		}
	}
}

// TestTokenValidateFailureReasons 四种失败原因必须可区分（外加两条成功路径）。
func TestTokenValidateFailureReasons(t *testing.T) {
	openTokenDB(t)
	now := nowUnix()

	// 1) 不存在
	if _, err := ValidateToken("nosuchkey000000000000000000000000000000000000"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("不存在的 Key 应当 ErrTokenNotFound，实际 %v", err)
	}
	// 2) 禁用
	disabled := newToken(t, 1, "disabled")
	disabled.Status = TokenStatusDisabled
	if err := SaveToken(disabled); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if _, err := ValidateToken(disabled.Key); !errors.Is(err, ErrTokenDisabled) {
		t.Fatalf("禁用令牌应当 ErrTokenDisabled，实际 %v", err)
	}
	// 3) 过期（status=3）
	expiredStatus := newToken(t, 1, "expired-status")
	expiredStatus.Status = TokenStatusExpired
	if err := SaveToken(expiredStatus); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if _, err := ValidateToken(expiredStatus.Key); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("status=3 应当 ErrTokenExpired，实际 %v", err)
	}
	// 3b) 过期（ExpiredTime 已过，status 仍启用）
	timeExpired := newToken(t, 1, "time-expired")
	timeExpired.ExpiredTime = now - 10
	if err := SaveToken(timeExpired); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if _, err := ValidateToken(timeExpired.Key); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("到期时间已过应当 ErrTokenExpired，实际 %v", err)
	}
	// 4) 耗尽（status=4 与 remain<=0 两种）
	exhaustedStatus := newToken(t, 1, "exhausted-status")
	exhaustedStatus.Status = TokenStatusExhausted
	if err := SaveToken(exhaustedStatus); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if _, err := ValidateToken(exhaustedStatus.Key); !errors.Is(err, ErrTokenExhausted) {
		t.Fatalf("status=4 应当 ErrTokenExhausted，实际 %v", err)
	}
	noQuota := newToken(t, 1, "no-quota")
	if err := DecreaseTokenQuota(noQuota.ID, noQuota.RemainQuota); err != nil {
		t.Fatalf("DecreaseTokenQuota: %v", err)
	}
	if _, err := ValidateToken(noQuota.Key); !errors.Is(err, ErrTokenExhausted) {
		t.Fatalf("额度为 0 应当 ErrTokenExhausted，实际 %v", err)
	}
	// 成功：启用 + 未过期 + 有额度
	good := newToken(t, 1, "good")
	got, err := ValidateToken("Bearer sk-" + good.Key)
	if err != nil {
		t.Fatalf("正常令牌应当通过（连 Bearer/sk- 一起剥壳），实际 %v", err)
	}
	if got.ID != good.ID {
		t.Fatalf("拿到的令牌不对: %+v", got)
	}
	// 成功：不限额度且额度为 0
	unlimited := newToken(t, 1, "unlimited")
	unlimited.UnlimitedQuota = true
	unlimited.RemainQuota = 0
	unlimited.ExpiredTime = -1
	if err := SaveToken(unlimited); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if _, err := ValidateToken(unlimited.Key); err != nil {
		t.Fatalf("不限额度令牌应当通过，实际 %v", err)
	}
}

// TestTokenQuotaPairAndUsageMonotonic 额度成对加减 + accessed_time 单调 + SaveToken 写零值。
func TestTokenQuotaPairAndUsageMonotonic(t *testing.T) {
	openTokenDB(t)
	tok := newToken(t, 7, "quota")
	if err := IncreaseTokenQuota(tok.ID, 30); err != nil {
		t.Fatalf("IncreaseTokenQuota: %v", err)
	}
	got, _ := GetTokenByID(tok.ID)
	if got.RemainQuota != 130 || got.UsedQuota != -30 {
		t.Fatalf("增加额度必须成对（remain+30 / used-30），实际 remain=%d used=%d", got.RemainQuota, got.UsedQuota)
	}
	if err := DecreaseTokenQuota(tok.ID, 50); err != nil {
		t.Fatalf("DecreaseTokenQuota: %v", err)
	}
	got, _ = GetTokenByID(tok.ID)
	if got.RemainQuota != 80 || got.UsedQuota != 20 {
		t.Fatalf("扣减额度必须成对（remain-50 / used+50），实际 remain=%d used=%d", got.RemainQuota, got.UsedQuota)
	}
	if err := IncreaseTokenQuota(tok.ID, -1); err == nil {
		t.Fatal("负数增量必须报错")
	}
	if err := IncreaseTokenQuota(tok.ID, 0); err != nil {
		t.Fatalf("0 增量应当是无操作而不是错误，实际 %v", err)
	}

	// accessed_time 单调推进：先用较新的时间，再用较旧的时间
	base := nowUnix()
	n, err := RecordTokenUsage(tok.ID, base+100)
	if err != nil || n != 1 {
		t.Fatalf("第一次记录应当推进 1 行，实际 (%d, %v)", n, err)
	}
	n, err = RecordTokenUsage(tok.ID, base)
	if err != nil {
		t.Fatalf("第二次记录报错: %v", err)
	}
	if n != 0 {
		t.Fatalf("更旧的时间不该覆盖，实际推进了 %d 行", n)
	}
	got, _ = GetTokenByID(tok.ID)
	if got.AccessedTime != base+100 {
		t.Fatalf("accessed_time 应当停在较新的值 %d，实际 %d", base+100, got.AccessedTime)
	}

	// SaveToken 必须能写零值（用 Select 而非 Updates(struct) 的理由）
	got.RemainQuota = 0
	got.UnlimitedQuota = false
	got.AllowIps = nil
	if err := SaveToken(got); err != nil {
		t.Fatalf("SaveToken(零值): %v", err)
	}
	after, _ := GetTokenByID(tok.ID)
	if after.RemainQuota != 0 || after.UnlimitedQuota || after.AllowIps != nil {
		t.Fatalf("SaveToken 没有写零值（Select 失效？）: remain=%d unlimited=%v ips=%v", after.RemainQuota, after.UnlimitedQuota, after.AllowIps)
	}
}

// TestTokenSoftDeleteHidden 软删后：单查/列表/计数/按 Key 查/校验全部看不见它。
func TestTokenSoftDeleteHidden(t *testing.T) {
	openTokenDB(t)
	keep := newToken(t, 9, "keep")
	del := newToken(t, 9, "del")
	n, err := DeleteToken(del.ID)
	if err != nil || n != 1 {
		t.Fatalf("DeleteToken = (%d, %v)，want (1, nil)", n, err)
	}
	if got, err := GetTokenByID(del.ID); err != nil || got != nil {
		t.Fatalf("软删后按 ID 不该查到: (%+v, %v)", got, err)
	}
	if got, err := GetTokenByKey(del.Key); err != nil || got != nil {
		t.Fatalf("软删后按 Key 不该查到: (%+v, %v)", got, err)
	}
	if _, err := ValidateToken(del.Key); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("软删后校验应当 ErrTokenNotFound，实际 %v", err)
	}
	rows, err := ListTokensByUser(9, 0, 0)
	if err != nil {
		t.Fatalf("ListTokensByUser: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != keep.ID {
		t.Fatalf("列表应当只剩未删的那条，实际 %+v", rows)
	}
	if cnt, err := CountTokensByUser(9); err != nil || cnt != 1 {
		t.Fatalf("计数应当排除软删，实际 (%d, %v)", cnt, err)
	}
	// 幂等：再删一次返回 0 行
	if n, err := DeleteToken(del.ID); err != nil || n != 0 {
		t.Fatalf("重复删除应当 0 行，实际 (%d, %v)", n, err)
	}
}

// TestTokenCreateDefaultsAndList 创建默认值 + 列表分页边界 + 掩码视图不含明文。
func TestTokenCreateDefaultsAndList(t *testing.T) {
	openTokenDB(t)
	tok := &Token{UserID: 3, Name: "defaults"}
	if err := CreateToken(tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if len(tok.Key) != TokenKeyLength {
		t.Fatalf("空 Key 应当自动生成 48 位，实际 %q", tok.Key)
	}
	if tok.Status != TokenStatusEnabled {
		t.Fatalf("status=0 应当补成启用，实际 %d", tok.Status)
	}
	if tok.ExpiredTime != -1 {
		t.Fatalf("expired_time=0 应当补成 -1（永不），实际 %d", tok.ExpiredTime)
	}
	if tok.CreatedTime == 0 {
		t.Fatal("created_time 应当补成当前时间")
	}
	// 掩码视图：key 是掩码，明文不在里面
	view := tok.Masked()
	if view.Key == tok.Key {
		t.Fatal("视图里的 key 不能是明文")
	}
	if view.Key != MaskTokenKey(tok.Key) {
		t.Fatalf("视图掩码与 MaskTokenKey 不一致: %q vs %q", view.Key, MaskTokenKey(tok.Key))
	}
	if view.Groups == nil || len(view.Groups) != 1 || view.Groups[0] != "default" {
		t.Fatalf("视图应当带上解码后的分组，实际 %v", view.Groups)
	}
	views := MaskedTokens([]Token{*tok})
	if len(views) != 1 || views[0].Key == tok.Key {
		t.Fatal("MaskedTokens 必须掩码")
	}
	// 列表分页边界：limit<=0 用默认、超大被夹到 200、offset<0 归零
	if _, err := ListTokensByUser(3, -5, -1); err != nil {
		t.Fatalf("边界参数不该报错: %v", err)
	}
	all, err := ListTokensByUser(3, 0, 9999)
	if err != nil || len(all) != 1 {
		t.Fatalf("超大 limit 应当被夹住并正常返回，实际 (%d, %v)", len(all), err)
	}
	if cnt, err := CountTokensByUser(3); err != nil || cnt != 1 {
		t.Fatalf("CountTokensByUser = (%d, %v)", cnt, err)
	}
}

// TestTokenAllowUserQuotaQuery 「允许此令牌查询账户余额」开关：默认值 / 往返 / 隔离性 / 零值可关回。
//
// 这个开关落在 AllowUserQuotaQuery（json allow_user_quota_query）上，且必须出现在掩码视图里 ——
// 否则前端拿不到它的状态，界面上那个开关会永远显示"关"。
func TestTokenAllowUserQuotaQuery(t *testing.T) {
	openTokenDB(t)
	tok := newToken(t, 11, "quota-query")
	if tok.AllowUserQuotaQuery {
		t.Fatal("默认必须是 false（与 hyxapi 同款默认）")
	}
	got, err := GetTokenByID(tok.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTokenByID: (%+v, %v)", got, err)
	}
	if got.AllowUserQuotaQuery {
		t.Fatal("落库后仍应是 false")
	}
	if v := got.Masked(); v.AllowUserQuotaQuery {
		t.Fatal("默认 false 时掩码视图也必须是 false")
	}
	// 打开开关 + 同时改其它字段：开关不能把它们带坏
	got.Name = "quota-query-renamed"
	got.RemainQuota = 42
	got.AllowUserQuotaQuery = true
	if err := SaveToken(got); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	after, err := GetTokenByID(got.ID)
	if err != nil || after == nil {
		t.Fatalf("GetTokenByID(after): (%+v, %v)", after, err)
	}
	if !after.AllowUserQuotaQuery {
		t.Fatal("开关没写进去")
	}
	if after.Name != "quota-query-renamed" || after.RemainQuota != 42 {
		t.Fatalf("其它字段被带坏: name=%q remain=%d", after.Name, after.RemainQuota)
	}
	if v := after.Masked(); !v.AllowUserQuotaQuery {
		t.Fatal("掩码视图必须带上开关状态（否则前端读不到）")
	}
	// 再关回去：零值必须能写（SaveToken 用 Select 而非 Updates(struct) 的理由）
	after.AllowUserQuotaQuery = false
	if err := SaveToken(after); err != nil {
		t.Fatalf("SaveToken(false): %v", err)
	}
	back, err := GetTokenByID(after.ID)
	if err != nil || back == nil {
		t.Fatalf("GetTokenByID(back): (%+v, %v)", back, err)
	}
	if back.AllowUserQuotaQuery {
		t.Fatal("关回去没生效（零值写不进）")
	}
}

// TestTokenViewExposesAutoGroups —— 自动分组配置的**回读口子**不许再丢。
//
// 2026-11-01 用户报：「保存按钮，它没有真的保存自动分组配置 不会保存啊」。
// 根因是三处叠加：① store.Token.AutoGroups 标了 json:"-"（模型层不序列化）；
// ② 对外视图 TokenView 不含 auto_groups/auto_group_mode ⇒ /admin/tokens 永不返回；
// ③ 前端抽屉无源可还原，只能硬写 inherit + 空列表 ⇒ 一保存就把已存配置覆盖成空。
// 这是一类很容易复发的缺陷（写路径一直好用、读路径静默缺字段），所以钉在这里：
//   快照非空 ⇒ custom + 原样返回；快照空 ⇒ inherit + 空。
func TestTokenViewExposesAutoGroups(t *testing.T) {
	tok := &Token{ID: 1, Name: "x"}
	if err := tok.SetAutoGroups([]string{"a", "b"}); err != nil {
		t.Fatalf("SetAutoGroups: %v", err)
	}
	v := tok.Masked()
	if v == nil {
		t.Fatal("Masked 返回 nil")
	}
	if v.AutoGroupMode != "custom" {
		t.Errorf("非空快照应为 custom，得到 %q", v.AutoGroupMode)
	}
	if len(v.AutoGroups) != 2 || v.AutoGroups[0] != "a" || v.AutoGroups[1] != "b" {
		t.Errorf("自动分组应原样回读，得到 %v", v.AutoGroups)
	}

	empty := &Token{ID: 2, Name: "y"}
	if err := empty.SetAutoGroups(nil); err != nil {
		t.Fatalf("SetAutoGroups(nil): %v", err)
	}
	v2 := empty.Masked()
	if v2.AutoGroupMode != "inherit" {
		t.Errorf("空快照应为 inherit，得到 %q", v2.AutoGroupMode)
	}
	if len(v2.AutoGroups) != 0 {
		t.Errorf("空快照不应带回分组，得到 %v", v2.AutoGroups)
	}
}
