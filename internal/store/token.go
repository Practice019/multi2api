// token.go 对话令牌（Token / API Key）的表与仓储 —— Wave A2，从零建。
//
// ⚠ 本文件**不**碰 models.go / db.go：allModels() 由 A1 负责登记 &Token{}（否则
// AutoMigrate 不会建 tokens 表）。本文件的测试自己 DB.AutoMigrate(&Token{})，不依赖登记。
//
// 字段对等：AllowUserQuotaQuery bool json:"allow_user_quota_query"（允许此令牌查询账户余额，
// 默认 false）已落地在结构体、TokenView 与 SaveToken 的可编辑列里（2026-10-26 补丁）。
//
// 刻意设计（主脑 2026-10-26 确认）：ValidateToken **只读**。hyxapi 在 Redis 关闭时会就地
// 改 status（3/4），本仓把落库挪到显式的 UpdateTokenStatus —— 校验函数带隐藏写会让
// "读一下令牌状态"变成一次写盘，也会让只读调用方（探针/诊断端点）无意中改库。
//
// 与 internal/apikey 的兼容规则：apikey 的 ID 形如 sk-<16hex>（sk- 存在库里，本文件不改它）；
// 本表的 Key 是 48 位裸随机串（库里没有前缀）。HTTP 层用 ParseTokenKey 剥壳（Bearer / sk- /
// 按 - 取第一段），于是 "sk-<48位>" 与 "<48位>" 都解析到同一个库内 Key，而 apikey 的
// sk-<16hex> 剥完只剩 16 位，不可能与本表的 48 位 Key 相撞。
package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"gorm.io/gorm"
)

// 令牌状态。数值与 hyxapi common/constants.go:198-201 逐值一致（1/2/3/4）。
const (
	TokenStatusEnabled   = 1
	TokenStatusDisabled  = 2
	TokenStatusExpired   = 3
	TokenStatusExhausted = 4
)

// Token 对话令牌（控制台"创建令牌"抽屉里的那一行）。
type Token struct {
	ID                 int     `json:"id" gorm:"primaryKey;autoIncrement"`
	UserID             int     `json:"user_id" gorm:"index"`
	Key                string  `json:"key" gorm:"type:char(48);uniqueIndex"`
	Status             int     `json:"status" gorm:"not null;default:1"`
	Name               string  `json:"name" gorm:"type:varchar(64);not null;default:'';index"`
	CreatedTime        int64   `json:"created_time" gorm:"not null;default:0"`
	AccessedTime       int64   `json:"accessed_time" gorm:"not null;default:0"`
	ExpiredTime        int64   `json:"expired_time" gorm:"not null;default:-1"`
	RemainQuota        int     `json:"remain_quota" gorm:"not null;default:0"`
	UnlimitedQuota     bool    `json:"unlimited_quota" gorm:"not null;default:false"`
	ModelLimitsEnabled bool    `json:"model_limits_enabled" gorm:"not null;default:false"`
	ModelLimits        string  `json:"model_limits" gorm:"type:text;not null;default:''"`
	AllowIps           *string `json:"allow_ips" gorm:"type:text"`
	// AllowUserQuotaQuery **允许此令牌查询账户余额**（用户截图里那个开关，hyxapi model/token.go:17）。
	//
	// 它决定"用令牌打 /api/usage/token 时响应里带不带 user_quota 段"：key 可能发给第三方，
	// 默认不让它看见账户余额。默认 false（与 hyxapi 同款默认）。
	AllowUserQuotaQuery    bool   `json:"allow_user_quota_query" gorm:"not null;default:false"`
	UsedQuota              int    `json:"used_quota" gorm:"not null;default:0"`
	Group                  string `json:"group" gorm:"type:text;not null;default:''"`
	AutoGroups             string `json:"-" gorm:"type:text;not null;default:''"`
	CrossGroupRetry        bool   `json:"cross_group_retry" gorm:"not null;default:false"`
	GroupSwitchStatusCodes string `json:"-" gorm:"type:text;not null;default:''"`
	GroupSwitchRetryCount  int    `json:"group_switch_retry_count" gorm:"not null;default:0"`

	// ── 以下 5 列是 internal/apikey（Wave B2）特有，**不是 hyxapi 对等字段** ──
	//
	// 将来做 hyxapi 对齐评审时，看到这几列不要以为漏抄 model/token.go:15-39 ——
	// hyxapi 的令牌只有额度/模型限制/分组/IP/过期，没有"每分钟请求上限"、
	// 没有请求/失败计数器、没有归属账号、也没有对外模型名映射；而本仓 apikey
	// 管理台的这四项都是既有功能（internal/apikey/apikey.go:48-175），
	// 换存储不能丢功能，所以停在同一张表里、按列分区。
	//
	// Requests 累计请求数（成功与失败都算）。
	Requests int64 `json:"requests" gorm:"not null;default:0"`
	// Fails 累计失败请求数（与 Requests 同源，BumpUsage 里二选一）。
	Fails int64 `json:"fails" gorm:"not null;default:0"`
	// RPM 每分钟请求上限；0 = 不限。热路径配置，随 key 一起进内存索引。
	RPM int `json:"rpm" gorm:"not null;default:0"`
	// OwnerID 归属账号 ID（account.User.ID，形如 u-xxxx）；**空 = 管理员所有**。
	//
	// ⚠ 与上面的 UserID(int) 是两回事，别合并：
	//	· UserID 是 hyxapi 那套"令牌属于哪个数字用户"，本仓 apikey 不用它；
	//	· OwnerID 是本仓账号体系的字符串 ID，只用于**归属展示与代管过滤**
	//	  （ListFor/CanManage），**不是鉴权主键** —— 鉴权只认 Key。
	// 之所以不塞进 UserID：字符串塞不进 int，硬转会把 u-1234 这类 ID 弄坏。
	OwnerID string `json:"owner_id" gorm:"type:varchar(64);not null;default:'';index"`
	// MigratedToGroups 该令牌的模型白名单是否已迁移到分组（apikey 的 P3.5 幂等标记）。
	//
	// ⚠ 这是**第 6 个** apikey 特有列（比你批的 5 个多一个）：apikey.Key 的
	// MigratedToGroups（apikey.go:156）是"迁移已经动过这把 key"的记账位，
	// 不落库的话重启后 P3.5 迁移会重跑一遍（再建同名分组、或把已迁移的 key
	// 又按白名单语义处理）。它与 Requests/Fails/RPM/OwnerID/ModelMap 同一性质：
	// 本仓 apikey 特有，不是 hyxapi 对等字段。
	MigratedToGroups bool `json:"migrated_to_groups" gorm:"not null;default:false"`
	// ModelMap 对外模型名 → 内部模型名的映射（JSON text）。
	//
	// ⚠ 空串与 "{}" 语义不同，读写都要往返无损：
	//	""   = nil（没有映射，字段缺失）
	//	"{}" = 显式空映射（非 nil 的空 map）
	ModelMap string `json:"model_map" gorm:"type:text;not null;default:''"`

	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`

	Groups           []string `json:"groups" gorm:"-"`
	GroupSwitchCodes []int    `json:"group_switch_status_codes" gorm:"-"`
}

var (
	ErrTokenNotFound  = errors.New("store: 令牌不存在")
	ErrTokenDisabled  = errors.New("store: 令牌已被禁用")
	ErrTokenExpired   = errors.New("store: 令牌已过期")
	ErrTokenExhausted = errors.New("store: 令牌额度已耗尽")
)

const TokenKeyLength = 48

const tokenKeyChars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// GenerateTokenKey 生成 48 位 [0-9a-zA-Z] 的库内 Key（crypto/rand，无前缀）。
//
// 这个字符串就是凭据本身：可预测的凭据等于没有凭据，所以不用 math/rand、不用时间戳。
// sk- 前缀是客户端写法，由 ParseTokenKey 剥掉，库里不带（避免"有的行带、有的不带"）。
func GenerateTokenKey() (string, error) {
	b := make([]byte, TokenKeyLength)
	maxI := big.NewInt(int64(len(tokenKeyChars)))
	for i := range b {
		n, err := rand.Int(rand.Reader, maxI)
		if err != nil {
			return "", fmt.Errorf("store: 生成令牌 Key: %w", err)
		}
		b[i] = tokenKeyChars[n.Int64()]
	}
	return string(b), nil
}

// ParseTokenKey 把客户端凭据剥成库内 Key（hyxapi middleware/auth.go:535-552 同款规则）：
// 去空白 → 剥 Bearer → 剥 sk- → 按 - 取第一段。
//
// 第 4 步在 hyxapi 里表示"-"后面是指定渠道 id（不是密钥），所以 Key 里不能含 -；
// 我们的字符集本来也没有 -。
func ParseTokenKey(raw string) string {
	key := strings.TrimSpace(raw)
	if len(key) >= 7 && strings.EqualFold(key[:7], "Bearer ") {
		key = strings.TrimSpace(key[7:])
	}
	key = strings.TrimPrefix(key, "sk-")
	if i := strings.IndexByte(key, 45); i >= 0 {
		key = key[:i]
	}
	return key
}

// MaskTokenKey 掩码（hyxapi model/token.go:174-185 逐档一致）：
//
//	空串      → ""
//	<= 4      → 全 *
//	<= 8      → 前 2 + **** + 后 2
//	其余      → 前 4 + 10 个 * + 后 4
//
// 边界必须钉住：4 与 8 是各档的最后一个值，9 是下一档第一个值。
func MaskTokenKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return strings.Repeat("*", len(key))
	}
	if len(key) <= 8 {
		return key[:2] + "****" + key[len(key)-2:]
	}
	return key[:4] + "**********" + key[len(key)-4:]
}

// FullKey 明文 Key —— 只在显式取明文的方法里用（详情接口、复制按钮）。
func (t *Token) FullKey() string {
	if t == nil {
		return ""
	}
	return t.Key
}

// MaskedKey 掩码后的 Key（列表/详情默认给这个）。
func (t *Token) MaskedKey() string {
	if t == nil {
		return ""
	}
	return MaskTokenKey(t.Key)
}

// NormalizeTokenGroups 把编码串解码成去重保序的分组列表（hyxapi :41-69）：
//
//	""      → ["default"]（没写 = 默认分组）
//	"[]"    → []          （显式空优先级列表）
//	"a,b"   → ["a","b"]   （历史逗号形态）
//	JSON 数组 → 解码后逐项
//
// 顺序就是分组优先级，所以是"保序去重"，不是排序。
func NormalizeTokenGroups(group string) []string {
	group = strings.TrimSpace(group)
	if group == "[]" {
		return []string{}
	}
	var raw []string
	if strings.HasPrefix(group, "[") {
		_ = json.Unmarshal([]byte(group), &raw)
	} else if group != "" {
		raw = strings.Split(group, ",")
	}
	seen := make(map[string]struct{}, len(raw))
	groups := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		groups = append(groups, item)
	}
	if len(groups) == 0 {
		return []string{"default"}
	}
	return groups
}

// EncodeTokenGroups 编码：空 → "[]"；一个 → 裸名；两个及以上 → JSON 数组串（hyxapi :71-81）。
func EncodeTokenGroups(groups []string) string {
	if len(groups) == 0 {
		return "[]"
	}
	normalized := NormalizeTokenGroups(strings.Join(groups, ","))
	if len(normalized) == 1 {
		return normalized[0]
	}
	body, _ := json.Marshal(normalized)
	return string(body)
}

// GetGroups 解码当前分组优先级（nil 接收者给 ["default"]）。
func (t *Token) GetGroups() []string {
	if t == nil {
		return []string{"default"}
	}
	return NormalizeTokenGroups(t.Group)
}

// UsesAutoGroups 是否走自动分组（hyxapi :91-94）：空列表与 ["auto"] 都算。
func (t *Token) UsesAutoGroups() bool {
	groups := t.GetGroups()
	return len(groups) == 0 || (len(groups) == 1 && groups[0] == "auto")
}

// GetAutoGroups 解码令牌级自动分组快照（空串 → nil）。
func (t *Token) GetAutoGroups() ([]string, error) {
	if t == nil || t.AutoGroups == "" {
		return nil, nil
	}
	var groups []string
	if err := json.Unmarshal([]byte(t.AutoGroups), &groups); err != nil {
		return nil, fmt.Errorf("store: 解析令牌自动分组快照: %w", err)
	}
	return groups, nil
}

// SetAutoGroups 写入自动分组快照（空列表 → 清空）。
func (t *Token) SetAutoGroups(groups []string) error {
	if t == nil {
		return errors.New("store: token 为 nil")
	}
	if len(groups) == 0 {
		t.AutoGroups = ""
		return nil
	}
	body, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("store: 编码令牌自动分组快照: %w", err)
	}
	t.AutoGroups = string(body)
	return nil
}

// NormalizeGroupSwitchStatusCodes 归一化换分组的触发状态码（hyxapi :117-131）：只认 100..599，去重保序。
func NormalizeGroupSwitchStatusCodes(codes []int) []int {
	seen := make(map[int]struct{}, len(codes))
	normalized := make([]int, 0, len(codes))
	for _, code := range codes {
		if code < 100 || code > 599 {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		normalized = append(normalized, code)
	}
	return normalized
}

// EncodeGroupSwitchStatusCodes 编码（空 → 空串，非空 → JSON 数组串）。
func EncodeGroupSwitchStatusCodes(codes []int) string {
	normalized := NormalizeGroupSwitchStatusCodes(codes)
	if len(normalized) == 0 {
		return ""
	}
	body, _ := json.Marshal(normalized)
	return string(body)
}

// NormalizeGroupPolicy 是本表分组相关字段的**唯一归一化入口**（hyxapi :142-172）。
//
// 它一次做五件事，缺一件就会出现两份真相：
//
//	1 Groups（计算字段）与 Group（落库串）二选一补全 —— 传进来哪个都行
//	2 空 Groups 且 Group != "auto" ⇒ 落库串变 "[]"（显式空列表，不是"没设"）
//	3 状态码：解码 → 归一化 → 重新编码
//	4 RetryCount 夹到 0..10
//	5 非 auto 时 CrossGroupRetry = len(Groups) > 1
//
// 任何写入路径（CreateToken/SaveToken）都必须先过它。
func (t *Token) NormalizeGroupPolicy() {
	if t == nil {
		return
	}
	if t.Groups == nil {
		t.Groups = t.GetGroups()
	} else if len(t.Groups) == 0 {
		if t.Group != "auto" {
			t.Group = "[]"
		}
	} else {
		t.Groups = NormalizeTokenGroups(strings.Join(t.Groups, ","))
	}
	if len(t.Groups) > 0 {
		t.Group = EncodeTokenGroups(t.Groups)
	}
	if len(t.GroupSwitchCodes) == 0 && t.GroupSwitchStatusCodes != "" {
		_ = json.Unmarshal([]byte(t.GroupSwitchStatusCodes), &t.GroupSwitchCodes)
	}
	t.GroupSwitchCodes = NormalizeGroupSwitchStatusCodes(t.GroupSwitchCodes)
	t.GroupSwitchStatusCodes = EncodeGroupSwitchStatusCodes(t.GroupSwitchCodes)
	if t.GroupSwitchRetryCount < 0 {
		t.GroupSwitchRetryCount = 0
	}
	if t.GroupSwitchRetryCount > 10 {
		t.GroupSwitchRetryCount = 10
	}
	if !t.UsesAutoGroups() {
		t.CrossGroupRetry = len(t.Groups) > 1
	}
}

// TokenView 对外（列表/详情）的形态：**没有明文 Key**。
//
// 字段 json 名与 Token 一致（前端不用改），只有 key 换成掩码值；
// AutoGroups / GroupSwitchStatusCodes 依旧不下发（与 hyxapi 的 json:"-" 同口径）。
type TokenView struct {
	ID                    int      `json:"id"`
	UserID                int      `json:"user_id"`
	Key                   string   `json:"key"`
	Status                int      `json:"status"`
	Name                  string   `json:"name"`
	CreatedTime           int64    `json:"created_time"`
	AccessedTime          int64    `json:"accessed_time"`
	ExpiredTime           int64    `json:"expired_time"`
	RemainQuota           int      `json:"remain_quota"`
	UnlimitedQuota        bool     `json:"unlimited_quota"`
	ModelLimitsEnabled    bool     `json:"model_limits_enabled"`
	ModelLimits           string   `json:"model_limits"`
	AllowIps              *string  `json:"allow_ips"`
	UsedQuota             int      `json:"used_quota"`
	AllowUserQuotaQuery   bool     `json:"allow_user_quota_query"`
	Group                 string   `json:"group"`
	CrossGroupRetry       bool     `json:"cross_group_retry"`
	GroupSwitchRetryCount int      `json:"group_switch_retry_count"`
	Groups                []string `json:"groups"`
	GroupSwitchCodes      []int    `json:"group_switch_status_codes"`
}

// Masked 转成对外视图（Key 掩码；Groups/StatusCodes 给解码值，不给落库串）。
func (t *Token) Masked() *TokenView {
	if t == nil {
		return nil
	}
	copyTok := *t
	copyTok.NormalizeGroupPolicy()
	return &TokenView{
		ID: copyTok.ID, UserID: copyTok.UserID, Key: copyTok.MaskedKey(), Status: copyTok.Status,
		Name: copyTok.Name, CreatedTime: copyTok.CreatedTime, AccessedTime: copyTok.AccessedTime,
		ExpiredTime: copyTok.ExpiredTime, RemainQuota: copyTok.RemainQuota, UnlimitedQuota: copyTok.UnlimitedQuota,
		ModelLimitsEnabled: copyTok.ModelLimitsEnabled, ModelLimits: copyTok.ModelLimits, AllowIps: copyTok.AllowIps, AllowUserQuotaQuery: copyTok.AllowUserQuotaQuery,
		UsedQuota: copyTok.UsedQuota, Group: copyTok.Group, CrossGroupRetry: copyTok.CrossGroupRetry,
		GroupSwitchRetryCount: copyTok.GroupSwitchRetryCount, Groups: copyTok.Groups, GroupSwitchCodes: copyTok.GroupSwitchCodes,
	}
}

// MaskedTokens 批量掩码。
func MaskedTokens(tokens []Token) []*TokenView {
	out := make([]*TokenView, 0, len(tokens))
	for i := range tokens {
		out = append(out, tokens[i].Masked())
	}
	return out
}

// GetModelLimits 模型白名单（逗号分隔，hyxapi :487-492）。
func (t *Token) GetModelLimits() []string {
	if t == nil || t.ModelLimits == "" {
		return []string{}
	}
	return strings.Split(t.ModelLimits, ",")
}

// GetModelLimitsMap 路由层匹配用的集合（hyxapi :494-501）。
func (t *Token) GetModelLimitsMap() map[string]bool {
	limits := t.GetModelLimits()
	out := make(map[string]bool, len(limits))
	for _, m := range limits {
		out[m] = true
	}
	return out
}

// GetIpLimits IP 白名单（换行分隔；先去空格再去逗号，hyxapi :195-215）。
//
// ⚠ 与 GetModelLimits 的**分隔符不同**（换行 vs 逗号）—— hyxapi 原样语义：
// 前端 IP 框是多行文本域，模型限制是单行逗号串。写反不会报错，只会静默不生效。
func (t *Token) GetIpLimits() []string {
	out := make([]string, 0)
	if t == nil || t.AllowIps == nil {
		return out
	}
	clean := strings.ReplaceAll(*t.AllowIps, " ", "")
	if clean == "" {
		return out
	}
	for _, ip := range strings.Split(clean, "\n") {
		ip = strings.TrimSpace(ip)
		ip = strings.ReplaceAll(ip, ",", "")
		if ip != "" {
			out = append(out, ip)
		}
	}
	return out
}

// ── 仓储 ────────────────────────────────────────────────────────────────

// CreateToken 插入一条令牌。
//
// 会补三个"调用方通常不填"的值：
//
//	Key 为空          → GenerateTokenKey()（不能让空 Key 进唯一索引）
//	Status == 0       → TokenStatusEnabled（0 不是合法状态，见 TokenStatus* 注释）
//	ExpiredTime == 0  → -1（永不）。0 表示 1970 年到期，对 API Key 无意义；
//	                    而 hyxapi 的列默认值也是 -1，两边口径一致
//
// 插入前必须过 NormalizeGroupPolicy（唯一归一化入口）。
func CreateToken(t *Token) error {
	if DB == nil {
		return ErrNotOpen
	}
	if t == nil {
		return errors.New("store: CreateToken 传入 nil")
	}
	if t.Key == "" {
		key, err := GenerateTokenKey()
		if err != nil {
			return err
		}
		t.Key = key
	}
	if t.Status == 0 {
		t.Status = TokenStatusEnabled
	}
	if t.ExpiredTime == 0 {
		t.ExpiredTime = -1
	}
	if t.CreatedTime == 0 {
		t.CreatedTime = nowUnix()
	}
	t.NormalizeGroupPolicy()
	if err := DB.Create(t).Error; err != nil {
		return fmt.Errorf("store: 建令牌 %q: %w", t.Name, err)
	}
	return nil
}

// GetTokenByID 按主键取；不存在（含已软删）返回 (nil, nil)。
func GetTokenByID(id int) (*Token, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	if id <= 0 {
		return nil, nil
	}
	var t Token
	err := DB.Where("id = ?", id).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读令牌 %d: %w", id, err)
	}
	return &t, nil
}

// GetTokenByKey 按库内 Key 取；不存在返回 (nil, nil)。
//
// ⚠ 传进来的是**库内 Key**（无前缀）。客户端原文先过 ParseTokenKey。
func GetTokenByKey(key string) (*Token, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	var t Token
	err := DB.Where("key = ?", key).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 按 Key 读令牌: %w", err)
	}
	return &t, nil
}

// 列表分页的硬边界（与 hyxapi 的 searchHardLimit 同精神：模型层夹住，不信任调用方）。
const (
	tokenListDefaultLimit = 20
	tokenListMaxLimit     = 200
)

// ListTokensByUser 列出某账号的令牌（**软删自动排除**，按 id 倒序 —— hyxapi 同序）。
func ListTokensByUser(userID int, offset, limit int) ([]Token, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = tokenListDefaultLimit
	}
	if limit > tokenListMaxLimit {
		limit = tokenListMaxLimit
	}
	var rows []Token
	if err := DB.Where("user_id = ?", userID).Order("id desc").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: 列令牌(user=%d): %w", userID, err)
	}
	return rows, nil
}

// ListTokens 列出**全部**令牌（软删自动排除，按 id 倒序）。
//
// 为什么不加一个"userID 传 0 就全量"的分支：apikey 的归属是**字符串** OwnerID，
// 而且带"空 = 管理员所有"的语义，用 int 过滤表达不了它；管理台本来就是全量取回后
// 在内存里过滤（apikey.Store 全量驻内存）。两个函数各自坦白，比一个函数两种模式好读。
func ListTokens(offset, limit int) ([]Token, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = tokenListDefaultLimit
	}
	if limit > tokenListMaxLimit {
		limit = tokenListMaxLimit
	}
	var rows []Token
	if err := DB.Order("id desc").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: 列全部令牌: %w", err)
	}
	return rows, nil
}

// CountTokens 全表计数（软删不计）。
func CountTokens() (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var n int64
	if err := DB.Model(&Token{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("store: 数全部令牌: %w", err)
	}
	return n, nil
}

// BumpTokenUsage 一次请求结束后的原子计量（apikey.BumpUsage 的落库半边）。
//
// 语义：
//
//	requests + 1                       （成功与失败都算）
//	ok=true  且 tokens>0  → used_quota + tokens
//	                        并且**只对非无限额度**的行同步扣 remain_quota
//	ok=false             → fails + 1
//
// # 为什么不让调用方"读出来改完再写回"
//
// 这是热路径：每个请求调一次。读-改-写在并发下会丢更新（两个请求同时读到
// requests=7，各自写 8），而计量丢的是钱。这里两条 UPDATE 都在一个事务里、
// 各自又是库内表达式（x = x + 1），所以不会丢。
//
// # 为什么 remain 只在 unlimited_quota=false 的行上扣
//
// 不限额度令牌的 remain 没有意义（A2 的 TryReserveTokenQuota 同款口径：
// unlimited 只记账不检查）；把它一起扣会造出一个"负数余额"的假象。
// used_quota 对两种令牌都累加，因为它就是"累计用量"。
func BumpTokenUsage(id int, tokens int64, ok bool) error {
	if DB == nil {
		return ErrNotOpen
	}
	if id <= 0 {
		return fmt.Errorf("%w: id=%d", ErrTokenNotFound, id)
	}
	if tokens < 0 {
		return errors.New("store: 计量增量不能为负")
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{"requests": gorm.Expr("requests + 1")}
		if ok {
			if tokens > 0 {
				updates["used_quota"] = gorm.Expr("used_quota + ?", tokens)
			}
		} else {
			updates["fails"] = gorm.Expr("fails + 1")
		}
		res := tx.Model(&Token{}).Where("id = ?", id).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: id=%d", ErrTokenNotFound, id)
		}
		if ok && tokens > 0 {
			if err := tx.Model(&Token{}).Where("id = ? AND unlimited_quota = ?", id, false).
				UpdateColumn("remain_quota", gorm.Expr("remain_quota - ?", tokens)).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: 计量令牌 %d: %w", id, err)
	}
	return nil
}

// CountTokensByUser 某账号的令牌数（软删不计）。
func CountTokensByUser(userID int) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var n int64
	if err := DB.Model(&Token{}).Where("user_id = ?", userID).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("store: 数令牌(user=%d): %w", userID, err)
	}
	return n, nil
}

// SaveToken 更新一条令牌的**可编辑列**（列清单与 hyxapi Token.Update 对齐）。
//
// ⚠ 用 Select(...) 而不是 Updates(struct)：后者会**跳过零值**，于是"把额度改回 0"、
// "把 cross_group_retry 关掉"、"清空 IP 白名单"这三类编辑会静默不生效。
// Select 强制写这些列，语义才是"库里这一行 = 我手上这一行"。
//
// ⚠ 不含 id/user_id/key/created_time/accessed_time/used_quota：
// 前几个是身份与归属（不可编辑），used_quota 只能由额度函数成对改。
func SaveToken(t *Token) error {
	if DB == nil {
		return ErrNotOpen
	}
	if t == nil || t.ID <= 0 {
		return fmt.Errorf("%w: id 非法", ErrTokenNotFound)
	}
	t.NormalizeGroupPolicy()
	res := DB.Model(&Token{}).Where("id = ?", t.ID).Select(
		"name", "status", "expired_time", "remain_quota", "unlimited_quota",
		"model_limits_enabled", "model_limits", "allow_user_quota_query", "allow_ips", "group", "cross_group_retry",
		"auto_groups", "group_switch_status_codes", "group_switch_retry_count",
		"requests", "fails", "rpm", "owner_id", "model_map", "migrated_to_groups",
	).Updates(t)
	if res.Error != nil {
		return fmt.Errorf("store: 更新令牌 %d: %w", t.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d", ErrTokenNotFound, t.ID)
	}
	return nil
}

// UpdateTokenStatus 只改 status 一列（启用/禁用开关走它，避免顺手改其它字段）。
func UpdateTokenStatus(id int, status int) error {
	if DB == nil {
		return ErrNotOpen
	}
	res := DB.Model(&Token{}).Where("id = ?", id).UpdateColumn("status", status)
	if res.Error != nil {
		return fmt.Errorf("store: 改令牌状态 %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d", ErrTokenNotFound, id)
	}
	return nil
}

// DeleteToken 软删（GORM 的 DeletedAt：行还在，但所有读路径自动排除）。
//
// 返回删除行数（0 = 本来就不存在）。**不做物理删**：令牌的历史用量要能追溯，
// 而"物理删 + 唯一索引"还会让同一个 Key 有机会被重新插入。
func DeleteToken(id int) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	res := DB.Where("id = ?", id).Delete(&Token{})
	if res.Error != nil {
		return 0, fmt.Errorf("store: 删令牌 %d: %w", id, res.Error)
	}
	return res.RowsAffected, nil
}

// ValidateToken 校验一个**库内 Key**，返回可用的令牌。
//
// 判定顺序（四种失败必须可区分，入口才能给出不同文案）：
//
//	1 不存在（含已软删）        → ErrTokenNotFound
//	2 status == 3              → ErrTokenExpired
//	3 status == 4              → ErrTokenExhausted
//	4 status != 1（即 2 或别的）→ ErrTokenDisabled
//	5 ExpiredTime != -1 且已过  → ErrTokenExpired
//	6 非不限额度且 RemainQuota<=0 → ErrTokenExhausted
//
// ⚠ **只读是刻意的（主脑 2026-10-26 确认，不是漏写）**：本函数不改 status、不写
// accessed_time。status 的落库由 UpdateTokenStatus 显式做；accessed_time 由
// RecordTokenUsage 显式推进（它才带"单调"那条判据）。判定与落库分开是职责划分。
func ValidateToken(key string) (*Token, error) {
	key = ParseTokenKey(key)
	if key == "" {
		return nil, ErrTokenNotFound
	}
	t, err := GetTokenByKey(key)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrTokenNotFound
	}
	switch t.Status {
	case TokenStatusExpired:
		return t, ErrTokenExpired
	case TokenStatusExhausted:
		return t, ErrTokenExhausted
	case TokenStatusEnabled:
	default:
		return t, ErrTokenDisabled
	}
	if t.ExpiredTime != -1 && t.ExpiredTime < nowUnix() {
		return t, ErrTokenExpired
	}
	if !t.UnlimitedQuota && t.RemainQuota <= 0 {
		return t, ErrTokenExhausted
	}
	return t, nil
}

// IncreaseTokenQuota 退还/增加额度：remain_quota + quota **且** used_quota - quota。
//
// ⚠ 两列必须成对：used_quota 是"这个令牌累计消耗了多少"，只加 remain 会让
// 两个数互相矛盾（对外展示 total = remain + used）。
// ⚠ gorm.Expr 让加减在**库内**完成（不是读出来再加），并发下不会丢更新。
func IncreaseTokenQuota(id int, quota int) error {
	return applyTokenQuotaDelta(id, quota, true)
}

// DecreaseTokenQuota 扣减额度：remain_quota - quota 且 used_quota + quota。
func DecreaseTokenQuota(id int, quota int) error {
	return applyTokenQuotaDelta(id, quota, false)
}

// applyTokenQuotaDelta 两个方向的共同实现（increase=true → remain 加、used 减）。
func applyTokenQuotaDelta(id int, quota int, increase bool) error {
	if DB == nil {
		return ErrNotOpen
	}
	if quota < 0 {
		return errors.New("store: 额度增量不能为负")
	}
	if quota == 0 {
		return nil // 0 是合法输入（预扣 0 的信任额度旁路），不动库也不报错
	}
	remain := gorm.Expr("remain_quota + ?", quota)
	used := gorm.Expr("used_quota - ?", quota)
	if !increase {
		remain = gorm.Expr("remain_quota - ?", quota)
		used = gorm.Expr("used_quota + ?", quota)
	}
	res := DB.Model(&Token{}).Where("id = ?", id).Updates(map[string]any{
		"remain_quota": remain,
		"used_quota":   used,
	})
	if res.Error != nil {
		return fmt.Errorf("store: 调整令牌额度 %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d", ErrTokenNotFound, id)
	}
	return nil
}

// RecordTokenUsage 记录一次成功鉴权（accessed_time **单调推进**）。
//
// 条件是 `accessed_time IS NULL OR accessed_time < ?`：并发请求不能用更旧的时间
// 覆盖更新的时间（hyxapi model/token.go:466-478 同款判据）。
//
// 返回被推进的行数：0 = 没推进（时间更旧，或行不存在/已删）。
func RecordTokenUsage(id int, now int64) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	res := DB.Model(&Token{}).Where("id = ? AND (accessed_time IS NULL OR accessed_time < ?)", id, now).
		UpdateColumn("accessed_time", now)
	if res.Error != nil {
		return 0, fmt.Errorf("store: 记录令牌使用时间 %d: %w", id, res.Error)
	}
	return res.RowsAffected, nil
}

// nowUnix 当前 Unix 秒。
//
// 单独一行函数而不是到处 time.Now().Unix()：过期判定与 accessed_time 必须用**同一个**
// 时间口径（秒），集中一处也便于以后换成可注入的时钟。
func nowUnix() int64 { return time.Now().Unix() }

// SetTokenUsage 把"已用量 / 限额语义 / 余量"三列**成对**写成给定值。
//
// 为什么不让 SaveToken 顺手写 used_quota：SaveToken 的注释写着"used_quota 只能由
// 额度函数成对改" —— 调用方拿一个陈旧结构体调 SaveToken 时把累计用量覆盖回去，是
// 静默的数据丢失。本函数相反：调用方**明确表达**"我要把用量写成这个值"，所以
// used/unlimited/remain 三列一起写，三者的互洽由这一次 UPDATE 保证。
//
// 主要调用方：internal/apikey 的整行写穿（apikey.Store 以内存为权威，用量也要落库）
// 与 Reset（把已用量清零、余量补回）。计量热路径不走它 —— 那条走 BumpTokenUsage
// 的单条增量 UPDATE。
func SetTokenUsage(id int, used int, unlimited bool, remain int) error {
	if DB == nil {
		return ErrNotOpen
	}
	if used < 0 {
		return errors.New("store: 已用量不能为负")
	}
	res := DB.Model(&Token{}).Where("id = ?", id).Updates(map[string]any{
		"used_quota":      used,
		"unlimited_quota": unlimited,
		"remain_quota":    remain,
	})
	if res.Error != nil {
		return fmt.Errorf("store: 写令牌用量 %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d", ErrTokenNotFound, id)
	}
	return nil
}
