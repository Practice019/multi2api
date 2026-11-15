// migration_backup.go 迁移前的**只读备份导出**（U-09 执行的前置步骤）。
//
// # 它做什么，不做什么
//
// 导出当前 tokens / groups / channels 的**结构**，带一份内容校验和。
// **不写任何数据**，也**不含任何明文凭据** —— 每个密钥只留一个指纹。
//
// # 为什么备份里不放明文（这是本文件最重要的决定）
//
// 计划 §5 U-09 写着「备份含秘密须受限保存」。更稳妥的做法是**根本不导出秘密**：
//
//	① 迁移要的是**结构**（谁有什么权限），不是密钥值。明文对计划毫无用处。
//	② 一份带明文 Bearer 的文件一旦落地就是个长期负债 —— 它会进 git、进
//	   聊天记录、进工单附件、进某人的下载目录。
//	③ 指纹足以回答备份真正需要回答的问题：「这行记录还是原来那把 key 吗？」
//
// 所以这里的输出**构造性地**不含秘密：没有 ID 字段、没有 Key 字段，
// 只有 `sha256(值)[:12]`。判据会拿真密钥去搜，搜不到才算过。
//
// # 为什么需要它
//
// 执行迁移是不可逆的写库。动手之前必须有一份"现状长什么样"的可核对物：
//
//	· 迁移后拿它对比 → 只有预期的改动
//	· 中途出事 → 知道原来是什么
//	· 两个人对同一份配置的理解不一致 → 拿 checksum 对齐
package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"workbuddy2api/internal/channelstore"
)

// backupFingerprintLen 指纹取多少位十六进制。
//
// 12 位 = 48 bit。它不需要抗碰撞（那是哈希本身的事），只需要在同一份备份里
// 区分不同的 key —— 而一个部署里的 token 数量是两位数到三位数。
const backupFingerprintLen = 12

// backupFingerprint 一个值的指纹。**不可逆**，且刻意不带任何前缀
// （带 `sk-` 之类前缀会让一个良性的指纹看起来像泄漏的密钥）。
func backupFingerprint(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:backupFingerprintLen]
}

// backupToken 一把令牌的结构（**不含明文**）。
type backupToken struct {
	Name string `json:"name"`
	// Owner 归属账号；空 = 管理员所有。
	Owner string `json:"owner,omitempty"`
	// PolicyVersion 0 = 旧语义，2 = 组候选 ∩ 白名单。
	PolicyVersion      int      `json:"policy_version"`
	Groups             []string `json:"groups,omitempty"`
	ModelLimitsEnabled bool     `json:"model_limits_enabled"`
	Models             []string `json:"models,omitempty"`
	Enabled            bool     `json:"enabled"`
	Limit              int64    `json:"limit"`
	Used               int64    `json:"used"`
	// KeyFingerprint 这把 key 的指纹（用于"还是原来那把吗"）。
	// ⚠ 没有 ID/Key 字段 —— 这是构造性的，见文件头。
	KeyFingerprint string `json:"key_fingerprint"`
}

// backupGroup 一个分组。
type backupGroup struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	PolicyVersion int      `json:"policy_version"`
	ChannelIDs    []string `json:"channel_ids,omitempty"`
}

// backupChannel 一条渠道（密钥只留指纹）。
type backupChannel struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	ProviderID string            `json:"provider_id,omitempty"`
	BaseURL    string            `json:"base_url,omitempty"`
	Status     int               `json:"status"`
	Models     []string          `json:"models,omitempty"`
	ModelMap   map[string]string `json:"model_map,omitempty"`
	Groups     []string          `json:"groups,omitempty"`
	// KeyFingerprints 渠道密钥的指纹。
	//
	// ⚠ **局限（实测发现，必须写在这里）**：`channelstore.List()` 回传的是
	// **掩码形态**（「前四…后四」，约 11 字节），不是存入的原文。
	// 所以这里的指纹是"掩码值的指纹"，它只能区分**首尾不同的**密钥 ——
	// 两把首 4 尾 4 相同的密钥会得到同一个指纹。
	//
	// 这仍然有用（"这条渠道还挂着原来那批 key 吗"多数情况能答），
	// 但**不能**当成密钥身份的强判据。真要核对渠道密钥，得走渠道自己的
	// 凭据读取路径，而那条路径按设计只回掩码。
	KeyFingerprints []string `json:"key_fingerprints,omitempty"`
}

// migrationBackup 一份备份（结构 + 校验和）。
type migrationBackup struct {
	GeneratedAt string `json:"generated_at"`
	// StateVersion 本地数据结构版本（与 data/schema-state.json 同一口径）。
	StateVersion int             `json:"state_version"`
	Tokens       []backupToken   `json:"tokens"`
	Groups       []backupGroup   `json:"groups"`
	Channels     []backupChannel `json:"channels"`
	// Checksum 覆盖 tokens/groups/channels 的内容哈希。
	//
	// 用途：迁移后重新导出一次，checksum 相同 ⇒ 结构没变。
	// 它**不**覆盖 generated_at（那是时间戳，每次都不同）。
	Checksum string `json:"checksum"`
	Note     string `json:"note"`
}

// backupPayload 参与 checksum 计算的部分（刻意不含 time / checksum 自己）。
type backupPayload struct {
	Tokens   []backupToken   `json:"tokens"`
	Groups   []backupGroup   `json:"groups"`
	Channels []backupChannel `json:"channels"`
}

// buildMigrationBackup 组装备份。**只读**，不改任何状态。
func (h *Handler) buildMigrationBackup() migrationBackup {
	out := migrationBackup{
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		StateVersion: channelstore.LocalSchemaVersion,
		Tokens:       []backupToken{},
		Groups:       []backupGroup{},
		Channels:     []backupChannel{},
	}

	// ── 令牌 ──
	if h.cfg.APIKeys != nil {
		for _, v := range h.cfg.APIKeys.List() {
			// ⚠ v.ID 是**完整密钥**。这里只用它查策略，绝不写进输出。
			ver, groups, limitsOn, models, _ := h.cfg.APIKeys.RoutingPolicyOf(v.ID)
			out.Tokens = append(out.Tokens, backupToken{
				Name:               v.Name,
				Owner:              v.OwnerID,
				PolicyVersion:      ver,
				Groups:             sortedStrings(groups),
				ModelLimitsEnabled: limitsOn,
				Models:             sortedStrings(models),
				Enabled:            v.Enabled,
				Limit:              v.Limit,
				Used:               v.Used,
				KeyFingerprint:     backupFingerprint(v.ID),
			})
		}
		sort.Slice(out.Tokens, func(i, j int) bool {
			if out.Tokens[i].Name != out.Tokens[j].Name {
				return out.Tokens[i].Name < out.Tokens[j].Name
			}
			return out.Tokens[i].KeyFingerprint < out.Tokens[j].KeyFingerprint
		})
	}

	// ── 分组 ──
	if h.cfg.Groups != nil {
		for _, g := range h.cfg.Groups.List() {
			out.Groups = append(out.Groups, backupGroup{
				ID:            g.ID,
				Name:          g.Name,
				Description:   g.Description,
				PolicyVersion: g.PolicyVersion,
				ChannelIDs:    sortedStrings(g.ChannelIDs),
			})
		}
		sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].ID < out.Groups[j].ID })
	}

	// ── 渠道 ──
	if h.cfg.Channels != nil {
		for _, ch := range h.cfg.Channels.List() {
			bc := backupChannel{
				ID:         ch.ID,
				Name:       ch.Name,
				Kind:       ch.EffectiveKind(),
				ProviderID: ch.ProviderID,
				BaseURL:    ch.BaseURL,
				Status:     ch.Status,
				Models:     sortedStrings(ch.Models),
				ModelMap:   ch.ModelMap,
				Groups:     sortedStrings(ch.Groups),
			}
			// ⚠ 渠道密钥也是秘密 —— 同样只留指纹。
			for _, k := range ch.Keys {
				if k.Key == "" {
					continue
				}
				bc.KeyFingerprints = append(bc.KeyFingerprints, backupFingerprint(k.Key))
			}
			sort.Strings(bc.KeyFingerprints)
			out.Channels = append(out.Channels, bc)
		}
		sort.Slice(out.Channels, func(i, j int) bool { return out.Channels[i].ID < out.Channels[j].ID })
	}

	// checksum 只覆盖结构（不含时间戳），否则每次导出都不同，"变了没有"就无从判断。
	raw, _ := json.Marshal(backupPayload{
		Tokens: out.Tokens, Groups: out.Groups, Channels: out.Channels,
	})
	sum := sha256.Sum256(raw)
	out.Checksum = hex.EncodeToString(sum[:])

	out.Note = "本备份**不含任何明文凭据**：令牌与渠道密钥只有 sha256 指纹。" +
		"迁移要的是结构（谁有什么权限），不是密钥值 —— " +
		"而一份带明文 Bearer 的文件落地即为长期负债（会进 git / 聊天记录 / 工单附件）。" +
		"checksum 只覆盖结构，所以两次导出内容不变时 checksum 相同。" +
		"⚠ 渠道密钥的指纹是对**掩码值**算的（store 按设计只回「前四…后四」形态），" +
		"只能区分首尾不同的密钥 —— 不要把它当密钥身份的强判据。"
	return out
}

// sortedStrings 复制并排序（nil 安全）。
func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// migrationBackupHandler GET /admin/migration/backup —— 只读导出当前结构。
func (h *Handler) migrationBackupHandler(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !h.principalFrom(r).IsAdmin() {
		writeError(w, http.StatusForbidden, "admin required")
		return
	}
	writeJSON(w, http.StatusOK, h.buildMigrationBackup())
}
