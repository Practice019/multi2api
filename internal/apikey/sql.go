// sql.go apikey 的 SQL 后端（Wave B2：存储真源从 data/apikeys.json 换到 tokens 表）。
//
// # 依赖方向：本文件是 apikey.go:221 那条"零内部依赖"的**唯一例外**
//
// 主脑裁决 B2 的存储层就是 internal/store 的 tokens 表（A2 落地），所以本文件
// import workbuddy2api/internal/store。其余文件仍不认识 groupstore 等内部包 ——
// 分组解析依旧靠装配层注入的函数值（见 groups_resolve.go）。
//
// # sk- 前缀兼容（B2 点名的头号陷阱）
//
// 两种形态同时存在，规则如下：
//
//	存量 apikey.Key.ID   sk-<16 hex>（apikey.go:741 newID）**sk- 在库里**
//	A2 新令牌表 Token.Key 48 位裸随机串（store.GenerateTokenKey，无前缀）
//
// 落库口径：**原样存**。sk- 是这把 key 的对外身份（Bearer 值、掩码、管理台
// 回显都用它），剥掉再存会让"复制粘贴出来的 Bearer"与"库里那行"对不上。
//
// 比对口径：请求侧先归一化成候选列表，逐个查索引（见 bearerCandidates）：
//
//	1 原样
//	2 剥掉 sk- 前缀
//	3 再按 hyxapi middleware/auth.go:549-551 的口径丢掉第一个 - 之后的部分
//	  （那里 -<channelId> 是"指定渠道"的语法，不是密钥的一部分）
//
// 于是 `sk-abc`、`abc`、`sk-abc-123` 三种写法都命中同一把 key，而 48 位裸串
// （字符集里没有 -）在第 1 步就命中，不受后续步骤影响。
//
// # 缓存与失效策略
//
// 索引 = 全量内存（apikey.Store 本来就是这个形态，JSON 时代也全量驻内存）：
//
//	keys      map[ID]*Key       权威内存态（管理端按 ID 读写）
//	byBearer  map[候选]*ID      Validate 热路径的 O(1) 查表（每个 key 挂 1~2 个候选）
//	rowIDs    map[ID]int        ID → tokens.id（计量走单条原子 UPDATE 要用）
//
// 失效策略 = **写路径写穿**（没有 TTL、没有定时刷新）：
//
//	· 本进程的每次变更都改内存 + 标脏，saveLocked() 把脏行 upsert 回库；
//	· 计量走 persistUsageLocked（单条原子 UPDATE），不重写整行；
//	· Delete 先摘索引再软删库行。
//
// 即"内存权威 + 同进程立即一致"。跨进程改动不订阅（本仓单实例部署，JSON 时代
// 也是同样的边界：另一个进程改了文件，本进程也要重启才看见）。
//
// # 不变量
//
// I-import：JSON → SQL 的导入**只发生一次**，由持久标记（options 表里的一行
// apikey.json_imported）保证，而**不是**由"tokens 表是否为空"保证。
//
// 推论（这就是标记必须存在的原因）：管理员把 key 全删光 ⇒ tokens 又空了，
// 导入**不会**因此重跑 —— 「删光」≠「需要重导」。少了这条不变量，删掉的 key
// 会在下次启动原样复活，而用户看到的是"我删干净的 key 又回来了"。
// 要真的重导一次：删掉那一行标记（或换一个空库）。
//
// # 计量（BumpUsage）为什么不是"只在内存里"
//
// 每个请求结束都 persistUsageLocked → store.BumpTokenUsage（一条 UPDATE：
// requests+1 / used+tokens / 非无限额度同时扣 remain）。重启后从库里读回来，
// 所以 BumpUsage 的计数**不会丢**；失败也不让请求失败（只记日志）。
package apikey

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"workbuddy2api/internal/store"
)

// bearerCandidates 把客户端凭据归一化成候选列表（顺序即优先级）。
//
// 规则与理由见文件头"sk- 前缀兼容"：原样 → 剥 sk- → 丢第一个 - 之后的部分。
// 返回的列表一定非空（原样那一条总是存在），所以调用方可以直接遍历。
func bearerCandidates(raw string) []string {
	raw = strings.TrimSpace(raw)
	out := make([]string, 0, 3)
	seen := make(map[string]bool, 3)
	add := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(raw)
	stripped := strings.TrimPrefix(raw, "sk-")
	add(stripped)
	if i := strings.IndexByte(stripped, 45); i >= 0 {
		add(stripped[:i])
	}
	return out
}

// ── Key ↔ store.Token 映射 ─────────────────────────────────────────────
//
// 对照表（左 = apikey.Key 字段，右 = tokens 列）：
//
//	ID          → key         （原样存 sk-…，见文件头）
//	Name        → name
//	Enabled     → status      （1 启用 / 2 禁用；3/4 由库侧写入，读回走 sqlStatus）
//	Limit       → unlimited_quota + remain_quota + used_quota
//	              （Limit=0 ⇒ unlimited；否则 remain = Limit - Used，读写往返无损）
//	Used        → used_quota
//	Requests    → requests    （B2 新增列）
//	Fails       → fails       （B2 新增列）
//	RPM         → rpm         （B2 新增列；热路径配置，随索引进内存）
//	CreatedAt   → created_time（RFC3339 ↔ Unix 秒）
//	ExpiredTime → expired_time（0 与 -1 都当"永不"）
//	OwnerID     → owner_id    （B2 新增列，字符串 u-xxxx；与 user_id int 无关）
//	Models      → model_limits + model_limits_enabled（逗号分隔；空 ⇒ 不限制）
//	Groups      → group       （A2 的编码串：空 ⇒ "[]"）
//	ModelMap    → model_map   （JSON text；"" ⇒ nil，"{}" ⇒ 空 map）

// remainQuotaOf 算出该 key 的 remain_quota（Limit=0 时无意义，固定 0）。
func remainQuotaOf(k *Key) int {
	if k.Limit <= 0 {
		return 0
	}
	return int(k.Limit - k.Used)
}

// keyToToken 把内存态映射成一行 tokens（不含 ID；调用方决定 create 还是 update）。
func keyToToken(k *Key) (*store.Token, error) {
	modelMap, err := encodeModelMap(k.ModelMap)
	if err != nil {
		return nil, err
	}
	t := &store.Token{
		Key:                k.ID,
		Name:               k.Name,
		Status:             statusOf(k),
		CreatedTime:        parseCreatedAt(k.CreatedAt),
		ExpiredTime:        k.ExpiredTime,
		RemainQuota:        remainQuotaOf(k),
		UnlimitedQuota:     k.Limit <= 0,
		UsedQuota:          int(k.Used),
		Requests:           k.Requests,
		Fails:              k.Fails,
		RPM:                k.RPM,
		OwnerID:            k.OwnerID,
		MigratedToGroups:   k.MigratedToGroups,
		ModelMap:           modelMap,
		ModelLimits:        strings.Join(k.Models, ","),
		ModelLimitsEnabled: len(k.Models) > 0,
		Group:              encodeGroups(k.Groups),
	}
	if t.ExpiredTime == 0 && k.sqlStatus == 0 {
		t.ExpiredTime = -1 // 0 与 -1 都表示"永不"；库里统一写 -1，避免两种"永不"
	}
	return t, nil
}

// statusOf 决定落库的 status：SQL 原始状态优先，否则按 Enabled 推导。
func statusOf(k *Key) int {
	if k.sqlStatus != 0 {
		return k.sqlStatus
	}
	if k.Enabled {
		return store.TokenStatusEnabled
	}
	return store.TokenStatusDisabled
}

// keyFromToken 把一行 tokens 还原成内存态 Key（往返无损，见测试）。
func keyFromToken(t *store.Token) *Key {
	k := &Key{
		ID:               t.Key,
		Name:             t.Name,
		Enabled:          t.Status == store.TokenStatusEnabled,
		Used:             int64(t.UsedQuota),
		Requests:         t.Requests,
		Fails:            t.Fails,
		RPM:              t.RPM,
		OwnerID:          t.OwnerID,
		MigratedToGroups: t.MigratedToGroups,
		CreatedAt:        formatCreatedAt(t.CreatedTime),
		ExpiredTime:      t.ExpiredTime,
		sqlStatus:        t.Status,
	}
	if t.UnlimitedQuota {
		k.Limit = 0
	} else {
		// Limit = Remain + Used：这是 keyToToken 的反运算，两者必须成对改。
		k.Limit = int64(t.RemainQuota) + int64(t.UsedQuota)
	}
	if t.ModelLimitsEnabled && t.ModelLimits != "" {
		k.Models = normalizeModels(strings.Split(t.ModelLimits, ","))
	}
	k.Groups = decodeGroups(t.Group)
	k.ModelMap = decodeModelMap(t.ModelMap)
	return k
}

// encodeGroups 分组列表 → 落库串（空 ⇒ "[]"，与 store.NormalizeTokenGroups 互为反运算）。
//
// ⚠ 必须走 store.EncodeTokenGroups 而不是自己拼：空列表如果写成 ""，读回来会被
// store 的默认值规则变成 ["default"] —— 那是"限制在 default 组"，而 apikey 的空
// 列表是"不限制"，两种语义完全相反。
func encodeGroups(groups []string) string {
	if len(groups) == 0 {
		return store.EncodeTokenGroups(nil)
	}
	return store.EncodeTokenGroups(groups)
}

// decodeGroups 落库串 → 分组列表（"" ⇒ nil，不经过 store 的 default 兜底）。
func decodeGroups(group string) []string {
	if strings.TrimSpace(group) == "" {
		return nil
	}
	g := store.NormalizeTokenGroups(group)
	if len(g) == 0 {
		return nil
	}
	return g
}

// encodeModelMap 映射表 → JSON text：nil ⇒ ""，空 map ⇒ "{}"，非空 ⇒ JSON。
//
// ⚠ 三种形态都要能区分（B2 验收⑤）：
//
//	nil        → ""     （没有映射；JSON 里根本没这个字段）
//	空 map     → "{}"   （显式空映射）
//	非空       → {...}
//
// 把前两者都写成 "" 会让"显式清空"和"从没设过"糊成一个状态 —— 管理台上
// 就表现为"我清空了它，重开又变回原样"（其实是被读成 nil 后又补了默认）。
func encodeModelMap(m map[string]string) (string, error) {
	if m == nil {
		return "", nil
	}
	body, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("apikey: 编码 model_map: %w", err)
	}
	return string(body), nil
}

// decodeModelMap 反向："" ⇒ nil，"{}" ⇒ 空 map（非 nil），其它 ⇒ 解析。
func decodeModelMap(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		// 坏数据不该让整个管理台 500：记日志并按"没有映射"处理（与读路径
		// 其它字段的宽容口径一致）。
		log.Printf("apikey: model_map 解析失败（按无映射处理）: %v", err)
		return nil
	}
	return m
}

// parseCreatedAt RFC3339 → Unix 秒（解析不了给 0，不 panic）。
func parseCreatedAt(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// formatCreatedAt Unix 秒 → RFC3339（0 给空串，保持"没写"的形态）。
func formatCreatedAt(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).Format(time.RFC3339)
}

// ── 装配：懒接管与显式注入两条路 ───────────────────────────────────────

// NewWithStore 显式装配入口（B5 用）：要求调用方**先** store.Open/Migrate，
// 构造完立刻是 SQL 模式，不做懒接管，也不碰 data/apikeys.json。
//
// 与 New 的区别只在时序：New 是"JSON 起手、之后可被接管"，NewWithStore 是
// "一开始就是 SQL"。两条路的存在理由见 task-59 的裁决：
//
//	· 现状 main.go:1289 的 apikey.New 早于 :1433 的 store.Open 约 150 行，
//	  所以今天只能走懒接管（不动 main.go）；
//	· B5 把装配挪到 Migrate 之后时，改成 NewWithStore 就能去掉懒接管这条隐式路径。
func NewWithStore(path string) (*Store, error) {
	s, err := New(path)
	if err != nil {
		return nil, err
	}
	if store.DB == nil {
		return nil, fmt.Errorf("apikey: NewWithStore 要求先打开 store（store.DB == nil）；" +
			"若还没有库，请用 New 走 JSON/懒接管路径")
	}
	if err := s.AttachSQL(); err != nil {
		return nil, err
	}
	return s, nil
}

// AttachSQL 把存储切到 SQL（幂等；重复调用是空操作）。
//
// 步骤：① JSON 快照一次性回填（仅当 tokens 为空，幂等）② 从 SQL 重建内存索引
// ③ 置标志位。任何一步失败都**保持 JSON 模式**并返回错误（fail-soft，不半切换）。
func (s *Store) AttachSQL() error {
	if s == nil || s.sqlMode.Load() {
		return nil
	}
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if s.sqlMode.Load() {
		return nil
	}
	if store.DB == nil {
		return fmt.Errorf("apikey: store 未打开（store.DB == nil），无法接管为 SQL 模式")
	}
	if err := s.importSnapshotToSQL(); err != nil {
		return fmt.Errorf("apikey: 回填 tokens 失败: %w", err)
	}
	if err := s.reloadIndexFromSQL(); err != nil {
		return fmt.Errorf("apikey: 从 SQL 重建索引失败: %w", err)
	}
	s.sqlMode.Store(true)
	return nil
}

// maybeAttachSQL 所有导出方法的入口守卫：库可用且还没接管时接管一次。
//
// 它是**热路径上的一次原子读**（sqlMode 是 atomic.Bool），所以 Validate 每请求
// 调一次也不心疼。库不可用（JSON 模式）时立刻返回。
//
// 失败只记日志、保持 JSON 模式：apikey 的读方法大多没有 error 返回值，让一次
// 回填失败把整个鉴权打成 500 更糟。日志出错可以查，静默降级不会丢数据（JSON
// 那份还在盘上）。
func (s *Store) maybeAttachSQL() {
	if s == nil || s.sqlMode.Load() || store.DB == nil {
		return
	}
	if err := s.AttachSQL(); err != nil {
		log.Printf("apikey: 接管 SQL 失败，继续用 JSON 快照: %v", err)
	}
}

// importSnapshotToSQL 把内存里的 JSON 快照一次性回填进 tokens 表（幂等）。
//
// 口径（task-59 要求 4）：**只有 tokens 为空时才搬**。第二次启动时表里已有行，
// 这里直接返回，所以"跑两次行数不变"。
//
// 为什么不在"表非空但少了某几把 key"时补齐：SQL 是接管后的真源，管理员在管理台
// 删掉的 key 不该被一份陈旧的 JSON 复活。缺的那几把由人重新建。
// importMarkerOptionKey 一次性导入的标记键。
//
// ⚠ 为什么光靠"tokens 为空才导入"这条门不够：管理员把库里的 key **全删光**之后，
// tokens 又变成空的，而那份 JSON 还躺在 data/ 下 —— 下次启动就会把删掉的 key
// **原样复活**（用户看到的是"我删干净的 key 又回来了"）。这个标记记下"那份 JSON
// 已经搬过了"，从此无论表空不空都不再搬。要重新导一次，删掉这一行即可。
//
// ⚠ 标记放**库内**（options 表一行）而不是下一个标记文件：文件会落在 data/ 里，
// 与 .gitignore 规则没有必然关系，有被 git-keeper 误 add 的风险；库内标记则
// 完全没有文件系统产物（主脑 2026-10-26 裁决）。
const importMarkerOptionKey = "apikey.json_imported"

// importAlreadyDone 标记是否已存在（存在 ⇒ 永远不再从 JSON 导入）。
func (s *Store) importAlreadyDone() (bool, error) {
	return store.OptionExists(importMarkerOptionKey)
}

func (s *Store) importSnapshotToSQL() error {
	done, err := s.importAlreadyDone()
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	s.mu.Lock()
	snapshot := make([]*Key, 0, len(s.keys))
	for _, k := range s.keys {
		snapshot = append(snapshot, cloneKeyForSQL(k))
	}
	s.mu.Unlock()
	if len(snapshot) == 0 {
		return nil
	}
	count, err := store.CountTokens()
	if err != nil {
		return err
	}
	if count > 0 {
		return nil // 已经导入过（或有别处写入）⇒ 幂等跳过
	}
	for _, k := range snapshot {
		// 第二重幂等守卫：同一批里重复 ID、或并发两次接管，都不会插第二行。
		existing, err := store.GetTokenByKey(k.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			continue
		}
		t, err := keyToToken(k)
		if err != nil {
			return err
		}
		if err := store.CreateToken(t); err != nil {
			return err
		}
	}
	if err := store.PutOption(importMarkerOptionKey, "imported at "+time.Now().Format(time.RFC3339)); err != nil {
		// 标记写不上不是致命：下一次启动会再判一次 count>0（那份 JSON 还是只搬一次）。
		// 但它是"数据复活"的唯一防线，所以要留下日志。
		log.Printf("apikey: 写入导入标记失败（下次仍按 tokens 非空判断）: %v", err)
	}
	log.Printf("apikey: 已把 JSON 快照里的 %d 把 key 一次性回填进 tokens 表", len(snapshot))
	return nil
}

// cloneKeyForSQL 复制一把 key（回填期间不持锁，必须脱离权威内存态）。
func cloneKeyForSQL(k *Key) *Key {
	c := *k
	c.Models = append([]string(nil), k.Models...)
	c.Groups = append([]string(nil), k.Groups...)
	c.ModelMap = cloneStringMap(k.ModelMap)
	return &c
}

// tokenRowPageSize 分页拉取的行数：store.ListTokens 会把 limit 夹在 200，
// 所以这里按同样的粒度翻页，直到取不满一页为止。
const tokenRowPageSize = 200

// listAllTokenRows 拉取全部 tokens 行（软删已由 store 排除）。
func listAllTokenRows() ([]store.Token, error) {
	all := make([]store.Token, 0, tokenRowPageSize)
	for offset := 0; ; offset += tokenRowPageSize {
		rows, err := store.ListTokens(offset, tokenRowPageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < tokenRowPageSize {
			return all, nil
		}
	}
}

// reloadIndexFromSQL 用库里的行重建三个索引（接管时调用一次）。
func (s *Store) reloadIndexFromSQL() error {
	rows, err := listAllTokenRows()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = make(map[string]*Key, len(rows))
	s.byBearer = make(map[string]string, len(rows)*2)
	s.rowIDs = make(map[string]int, len(rows))
	for i := range rows {
		k := keyFromToken(&rows[i])
		if k.ID == "" {
			continue
		}
		s.keys[k.ID] = k
		s.rowIDs[k.ID] = rows[i].ID
		s.indexBearerLocked(k)
	}
	return nil
}

// ── 索引维护（调用方持锁） ───────────────────────────────────────────

// indexBearerLocked 给一把 key 挂上全部 Bearer 候选（Validate 的 O(1) 查表）。
func (s *Store) indexBearerLocked(k *Key) {
	for _, c := range bearerCandidates(k.ID) {
		s.byBearer[c] = k.ID
	}
}

// unindexBearerLocked 摘掉一把 key 的候选（删 key 时调用）。
func (s *Store) unindexBearerLocked(id string) {
	for _, c := range bearerCandidates(id) {
		if s.byBearer[c] == id {
			delete(s.byBearer, c)
		}
	}
}

// lookupBearerLocked 按候选顺序查表，返回命中的 key（Nil = 没命中）。
//
// 候选顺序保证"原样"优先：一把 ID 恰好等于某个候选的 key 不会被另一把 key 的
// 剥壳形态抢走。
func (s *Store) lookupBearerLocked(bearer string) (*Key, string) {
	for _, c := range bearerCandidates(bearer) {
		id, ok := s.byBearer[c]
		if !ok {
			continue
		}
		if k, found := s.keys[id]; found {
			return k, id
		}
	}
	return nil, ""
}

// markDirtyLocked 标脏（saveLocked 只会把脏行写回库，不做全表重写）。
func (s *Store) markDirtyLocked(id string) {
	if s.dirty == nil {
		s.dirty = make(map[string]struct{})
	}
	s.dirty[id] = struct{}{}
}

// markAllDirtyLocked 把全部 key 标脏（批量运维用：prune/migrate 一次动很多把，
// 逐个记 id 不如整体重写一遍；这类操作本来就低频）。
func (s *Store) markAllDirtyLocked() {
	if s.dirty == nil {
		s.dirty = make(map[string]struct{}, len(s.keys))
	}
	for id := range s.keys {
		s.dirty[id] = struct{}{}
	}
}

// ── 写穿 ─────────────────────────────────────────────────────────────

// flushSQLLocked 把脏行与待删行写回库（SQL 模式下 saveLocked 的全部工作）。
//
// 每把 key 两条语句：SaveToken（元数据）+ SetTokenUsage（用量三列成对写）。
// 刻意不合并成一条：SaveToken 永不碰 used_quota（见 store 里那段注释），
// 而"把用量写回去"是这里明确的意图。两条语句让这两个意图各自可读，
// 而且**下一次 flush 会把两者都重写一遍**，所以即使中间崩了也会自愈。
func (s *Store) flushSQLLocked() error {
	for _, id := range s.pendingDel {
		rid := s.rowIDs[id]
		if rid > 0 {
			if _, err := store.DeleteToken(rid); err != nil {
				return err
			}
			delete(s.rowIDs, id)
		}
	}
	s.pendingDel = nil
	for id := range s.dirty {
		k, ok := s.keys[id]
		if !ok {
			continue
		}
		if err := s.upsertLocked(k); err != nil {
			return err
		}
	}
	s.dirty = make(map[string]struct{})
	return nil
}

// upsertLocked 写一把 key（先试更新，行不在就新建）。调用方持锁。
func (s *Store) upsertLocked(k *Key) error {
	t, err := keyToToken(k)
	if err != nil {
		return err
	}
	if rid := s.rowIDs[k.ID]; rid > 0 {
		t.ID = rid
		err = store.SaveToken(t)
		if err == nil {
			return store.SetTokenUsage(rid, int(k.Used), k.Limit <= 0, remainQuotaOf(k))
		}
		if !errors.Is(err, store.ErrTokenNotFound) {
			return err
		}
		// 行被别处软删了（管理台/别的进程）⇒ 回落成新建，避免"内存有、库里没有"。
	}
	if err := store.CreateToken(t); err != nil {
		return err
	}
	s.rowIDs[k.ID] = t.ID
	return nil
}

// persistUsageLocked 计量写穿（热路径）：一条原子 UPDATE，不重写整行。
//
// 内存先加、库再写（顺序不能反：写库失败时内存还必须是对的，否则同一进程里
// 后续请求会读到比实际少的用量）。失败只记日志 —— 用量统计不能拖垮请求。
func (s *Store) persistUsageLocked(id string, tokens int64, ok bool) {
	rid := s.rowIDs[id]
	if rid <= 0 {
		// 没有行号（JSON 模式，或该 key 还没落库）⇒ 走整行写穿，别把计量丢了。
		s.markDirtyLocked(id)
		return
	}
	if err := store.BumpTokenUsage(rid, tokens, ok); err != nil {
		log.Printf("apikey: 计量落库失败（内存已加，下次 flush 会补）: %v", err)
		s.markDirtyLocked(id) // 让下一次 saveLocked 把整行写回去兜底
	}
}
