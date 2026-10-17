// settings_store.go 管理台设置页的宿主持久化实现。
//
// 两条设计约束：
//  1. 落盘用「读原 JSON → 改嵌套键 → 写回」，而不是把 Config 结构体整体序列化。
//     否则用户手工在 config.json 里留的注释键、未知键、字段顺序都会在一次保存后消失。
//  2. 只有明确能在运行时生效的字段才立刻应用；其余照常写盘，但如实回报
//     「需重启」，绝不假装已生效。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/workbuddy"
)

// settingsStore 实现 admin.SettingsStore。
// settingsStore 实现 admin.SettingsStore。
//
// # Task 3c 之后的形状
//
// 补丁是**原始 JSON**，因为里面混着两类键：
//
//	通用段   由本文件解释（签到/保活时点、上游超时、账号池限流…）
//	上游段   由上游自己解释（workbuddy 的成长/旅行开关，见 internal/workbuddy/settings.go）
//
// 核心不再认识 "growth_auto_open" 这类名字 —— 那 19 处上游概念
// （9 个字段 + 两张写死的清单）已随设置项一起搬进上游包。
//
// 上游段的落盘也在这里做（而不是让上游写 config.json）：
// 上游包不得碰配置文件格式，那是宿主的职责。
type settingsStore struct {
	path string
	cfg  *Config
	sch  *scheduler.Scheduler
	// up 上游的设置扩展（workbuddy 的成长/旅行开关）。
	// 为 nil 时上游段全为空 —— 与"该上游没有专属设置"一致。
	up  UpstreamSettings
	log *checkinlog.Log
	// setLogKeepDays 调整落盘请求日志的保留天数（由 main 注入，避免这里依赖 logbuf）。
	setLogKeepDays func(int)
}

// UpstreamSettings 上游专属设置在**宿主**这一侧需要的形状。
//
// 只有一个方法：给出要落盘的键值。
//
// # 为什么没有 Apply
//
// 写入点只有一个 —— admin 的 handler 调 `SettingsExt.ApplySettings`。
// 宿主在这里再写一遍不仅冗余，还会让 applied 里出现重复键
// （前端会把它读成一件说不清的事）。
// 宿主在这一侧只做两件事：落盘（读当前值）与回显（Snapshot 读 config）。
type UpstreamSettings interface {
	// Values 本上游全部设置项的当前值（用于持久化与回显）。
	//
	// 必须是**全量**而不是补丁里那几个键：补丁是稀疏的，
	// 只写补丁里的键会把其它开关从 config.json 里抹掉。
	Values() map[string]any
}

func (s *settingsStore) Snapshot() admin.Settings {
	c := s.cfg
	checkinH := s.sch.SlotHours(workbuddy.SlotCheckin)
	keepaliveH := s.sch.SlotHours(workbuddy.SlotKeepalive)
	return admin.Settings{
		// ⚠⚠ **这两个值必须读 `cfg.Schedule`，不能读 `s.sch.SlotEnabled(...)`。**
		//
		// 原因（`schedulerAdapter` 的字段注释里也写着同一件事）：
		// 签到/保活已统一成 `JobExt` 扫描，**槽位不再注册** ⇒ `SlotEnabled` **恒为 false**
		// ⇒ 这里若读槽位，设置页就会**永远显示"已关闭"**，而配置文件里可能明明是 true。
		//
		// ⚠ 这是实测到的缺陷：`/admin/settings` 报 `签到=false 保活=false`，
		// 而 `/admin/schedule` 报 `签到=true 保活=true`、`config.json` 里是 `签到=true 保活=false`
		// —— **同一件事三个答案**，设置页顶部 chip 与下面正文互相打脸。
		// 根因之一：`main.go` 里给 `adminSchedulerAdapter` 的 `keepaliveOn` 曾被**写死 `true`**
		// （已改成读配置）；另一半就是这里读槽位恒 false。
		// ⇒ 两处都改成**读配置**，这样 setting / schedule / config 三者同源。
		CheckinEnabled:   c.Schedule.CheckinEnabled,
		KeepaliveEnabled: c.Schedule.KeepaliveEnabled,
		CheckinHours:     checkinH,
		KeepaliveHours:   keepaliveH,

		CheckinLogKeepDays: s.log.KeepDays(),
		RequestLogKeepDays: c.RequestLogKeepDays,

		SoftRate:              c.Cooldown.SoftRate,
		UpstreamTimeoutSec:    c.Upstream.TimeoutSeconds,
		UpstreamHeaderTimeout: c.Upstream.HeaderTimeoutSeconds,
		UpstreamIdleTimeout:   c.Upstream.IdleTimeoutSeconds,
		PoolMaxInFlight:       c.Pool.MaxInFlight,
		BreakerThreshold:      c.Pool.BreakerThreshold,
		BreakerCooldown:       c.Pool.BreakerCooldown,
		BreakerCooldownMax:    c.Pool.BreakerCooldownMax,
		SanitizeFingerprints:  c.Features.SanitizeBlacklistFingerprints,
		UpstashURL:            c.Upstash.URL,
		SessionStickyEnabled:  c.SessionSticky.Enabled,
		SessionStickyTTL:      c.SessionSticky.TTL,

		Listen:     c.Listen,
		AuthDir:    c.AuthDir,
		ConfigPath: s.path,
		APIKeySet:  c.APIKey != "",
	}
}

// Apply 校验 → 写盘 → 应用运行时字段。
//
// 校验失败在写盘之前返回，磁盘保持原样。
//
// # 补丁的分发顺序
//
//  1. 解析出通用段（不认识的键被 encoding/json 静默忽略 —— 正是我们要的）；
//  2. 校验通用段 —— 失败即返回，**不写盘、不改内存**；
//  3. 落盘（通用段 + 上游段的当前值）；
//  4. 应用通用段的运行时值。
//
// 顺序的关键在第 2 步：校验必须在任何副作用之前。
//
// # 上游段的运行时不在这里应用
//
// 上游的设置值由 admin 的 handler 调 `SettingsExt.ApplySettings` 应用
// （那是唯一的写入点）。这里**只负责落盘**，因为它要读上游应用**之后**的当前值。
// 两边都写一次不仅冗余，还会让 applied 里出现重复键 ——
// 而前端会把重复的"已生效"读成一件说不清的事。
func (s *settingsStore) Apply(patch json.RawMessage) (applied, needRestart []string, err error) {
	p, err := s.parse(patch)
	if err != nil {
		return nil, nil, err
	}
	if err := validateSettings(p); err != nil {
		return nil, nil, err
	}

	// 落盘：通用段（本文件负责）+ 上游段的当前值。
	if err := s.persist(p); err != nil {
		return nil, nil, fmt.Errorf("写入 %s 失败: %w", s.path, err)
	}

	// 运行时生效（通用段）
	//
	// 槽位名在这里出现是**装配处**的职责：本文件是唯一同时认识"设置页的
	// 签到/保活字段"与"核心的槽位"的地方。核心只认识槽位，设置页只认识中文，
	// 翻译就发生在这一层。
	s.sch.SetSlotEnabled(workbuddy.SlotCheckin, p.CheckinEnabled)
	s.sch.SetSlotEnabled(workbuddy.SlotKeepalive, p.KeepaliveEnabled)
	if err := s.sch.SetSlotHours(workbuddy.SlotCheckin, p.CheckinHours); err != nil {
		return nil, nil, errors.New("签到时点超出范围（0-23）")
	}
	if err := s.sch.SetSlotHours(workbuddy.SlotKeepalive, p.KeepaliveHours); err != nil {
		return nil, nil, errors.New("保活时点超出范围（0-23）")
	}
	if p.CheckinLogKeepDays > 0 {
		s.log.SetKeepDays(p.CheckinLogKeepDays)
	}
	if p.RequestLogKeepDays > 0 && s.setLogKeepDays != nil {
		s.setLogKeepDays(p.RequestLogKeepDays)
	}
	applied = []string{
		"checkin_enabled", "keepalive_enabled", "checkin_hours", "keepalive_hours",
		"checkin_log_keep_days", "request_log_keep_days",
	}

	// 只写盘、需重启才生效（上游 client 的超时、账号池限流器、Redis 连接
	// 都是在启动时一次性装配的，运行中改不了；守卫轮间隔同理，
	// 由上游在 SettingField.RestartRequired 里声明）。
	needRestart = []string{
		"cooldown_soft_rate",
		"upstream_timeout_seconds", "upstream_header_timeout_seconds", "upstream_idle_timeout_seconds",
		"pool_max_in_flight", "pool_breaker_threshold", "pool_breaker_cooldown", "pool_breaker_cooldown_max",
		"sanitize_blacklist_fingerprints", "upstash_url",
		"session_sticky_enabled", "session_sticky_ttl",
	}
	// 把新值同步进内存 cfg，让 Snapshot 显示的就是刚保存的值。
	s.applyToMemory(p)
	return applied, needRestart, nil
}

// parse 从原始补丁里取出**通用段**。
//
// 不认识的键被 encoding/json 静默忽略 —— 那正是"核心不认识上游字段"的实现方式：
// 它不必拒绝它们，只是不解释它们。
//
// # ⚠ 关键：先播当前值，再让补丁覆盖
//
// 早先的实现是 `var p admin.Settings; json.Unmarshal(patch, &p)` ——
// 补丁里**没出现的键**保持零值，而调用方随后把这个 p 整个写回 config.json。
// 于是前端只改一个开关（`{"checkin_enabled":false}`）就会把
// upstream.timeout_seconds / pool.max_in_flight / cooldown.soft_rate /
// session_sticky 等**未提交的配置全部清零**。
//
// 这是阶段 0 评审复现的 F1（真实存在，改造前就有）：
//
//	PUT {"checkin_enabled":false}  →
//	  upstream.timeout_seconds: 120 → 0
//	  pool.max_in_flight:         8 → 0
//	  cooldown.soft_rate:     "60s" → ""
//	  session_sticky.enabled:  true → false
//
// 修法：把**当前内存里的值**作为基底先填进 p，再让补丁覆盖它。
// 这样"补丁里没有的键"保持原值，语义从"整体替换"变成真正的"局部更新"。
//
// 为什么用"先播值再 Unmarshal"而不是 map[string]json.RawMessage 探测键存在性：
// 后者需要为每个字段写一遍存在性判断（20+ 个字段，且加字段必忘），
// 而"先播值"对**所有**字段自动正确，加字段零成本。
func (s *settingsStore) parse(patch json.RawMessage) (admin.Settings, error) {
	// 基底 = 当前生效值（含启动时从 config.json 读到的、以及上次保存的）
	p := s.currentSettings()
	if len(patch) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return p, fmt.Errorf("设置解析失败: %w", err)
	}
	return p, nil
}

// currentSettings 把当前生效的通用段汇成一个 admin.Settings。
//
// 用途：作为 parse 的基底，保证"补丁里没提到的键"不被动过。
// 必须与 applyToMemory 的字段清单**一一对应**（一个是读、一个是写），
// 缺字段会导致该字段每次保存都被清零 —— 有测试守着这个对称性。
func (s *settingsStore) currentSettings() admin.Settings {
	c := s.cfg
	// log 在部分装配路径（测试、或未启用签到日志的部署）可能为 nil。
	// 不判空会 panic —— 这是我在写这条修复时实测踩到的。
	var checkinLogKeepDays int
	if s.log != nil {
		checkinLogKeepDays = s.log.KeepDays()
	}
	return admin.Settings{
		SoftRate:              c.Cooldown.SoftRate,
		UpstreamTimeoutSec:    c.Upstream.TimeoutSeconds,
		UpstreamHeaderTimeout: c.Upstream.HeaderTimeoutSeconds,
		UpstreamIdleTimeout:   c.Upstream.IdleTimeoutSeconds,
		PoolMaxInFlight:       c.Pool.MaxInFlight,
		BreakerThreshold:      c.Pool.BreakerThreshold,
		BreakerCooldown:       c.Pool.BreakerCooldown,
		BreakerCooldownMax:    c.Pool.BreakerCooldownMax,
		SanitizeFingerprints:  c.Features.SanitizeBlacklistFingerprints,
		UpstashURL:            c.Upstash.URL,
		SessionStickyEnabled:  c.SessionSticky.Enabled,
		SessionStickyTTL:      c.SessionSticky.TTL,
		CheckinEnabled:        c.Schedule.CheckinEnabled,
		KeepaliveEnabled:      c.Schedule.KeepaliveEnabled,
		CheckinHours:          c.Schedule.CheckinHours,
		KeepaliveHours:        c.Schedule.KeepaliveHours,
		CheckinLogKeepDays:    checkinLogKeepDays,
		RequestLogKeepDays:    c.RequestLogKeepDays,
	}
}

// applyToMemory 把刚保存的通用段同步进内存 cfg，让 Snapshot 显示的就是新值。
//
// 上游段的对应字段由 upstreamSettingsAdapter.applyToMemory 负责 ——
// 那份清单属于上游，核心不认识它们（这正是 Task 3c 解耦掉的东西）。
func (s *settingsStore) applyToMemory(p admin.Settings) {
	c := s.cfg
	c.Cooldown.SoftRate = p.SoftRate
	c.Upstream.TimeoutSeconds = p.UpstreamTimeoutSec
	c.Upstream.HeaderTimeoutSeconds = p.UpstreamHeaderTimeout
	c.Upstream.IdleTimeoutSeconds = p.UpstreamIdleTimeout
	c.Pool.MaxInFlight = p.PoolMaxInFlight
	c.Pool.BreakerThreshold = p.BreakerThreshold
	c.Pool.BreakerCooldown = p.BreakerCooldown
	c.Pool.BreakerCooldownMax = p.BreakerCooldownMax
	c.Features.SanitizeBlacklistFingerprints = p.SanitizeFingerprints
	c.Upstash.URL = p.UpstashURL
	c.SessionSticky.Enabled = p.SessionStickyEnabled
	c.SessionSticky.TTL = p.SessionStickyTTL
	c.Schedule.CheckinEnabled = p.CheckinEnabled
	c.Schedule.KeepaliveEnabled = p.KeepaliveEnabled
	c.Schedule.CheckinHours = p.CheckinHours
	c.Schedule.KeepaliveHours = p.KeepaliveHours
	c.Admin.CheckinLogKeepDays = p.CheckinLogKeepDays
	c.RequestLogKeepDays = p.RequestLogKeepDays
	// 上游段：由适配器按自己的键表同步（见 upstreamSettingsAdapter）。
	if s.up != nil {
		if a, ok := s.up.(interface{ applyToMemory(map[string]any) }); ok {
			a.applyToMemory(s.up.Values())
		}
	}
}

func validateSettings(p admin.Settings) error {
	checkHour := func(name string, hours []int) error {
		for _, h := range hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("%s: %d 不是合法小时（0-23）", name, h)
			}
		}
		return nil
	}
	if err := checkHour("签到时点", p.CheckinHours); err != nil {
		return err
	}
	if err := checkHour("保活时点", p.KeepaliveHours); err != nil {
		return err
	}
	if p.SoftRate != "" {
		if _, err := time.ParseDuration(p.SoftRate); err != nil {
			return fmt.Errorf("429 冷却时长: %w", err)
		}
	}
	for name, v := range map[string]string{
		"熔断基础时长":   p.BreakerCooldown,
		"熔断退避封顶":   p.BreakerCooldownMax,
		"会话粘性 TTL": p.SessionStickyTTL,
	} {
		if v == "" {
			continue
		}
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if p.UpstreamTimeoutSec < 0 || p.UpstreamHeaderTimeout < 0 || p.UpstreamIdleTimeout < 0 {
		return fmt.Errorf("上游超时不能为负")
	}
	if p.PoolMaxInFlight < 0 || p.BreakerThreshold < 0 {
		return fmt.Errorf("账号池参数不能为负")
	}
	if p.CheckinLogKeepDays < 0 || p.RequestLogKeepDays < 0 {
		return fmt.Errorf("日志保留天数不能为负")
	}
	return nil
}

// persist 把设置合并进 config.json，保留文件里已有的其它键。
//
// # ⚠⚠ 这里曾经把用户的整个配置重写掉（真实事故，2026-10-15）
//
// 事故现场：用户的 config.json 从 26 个顶层键变成 **9 个** —— 只剩这个函数管的
// 那些设置段（admin / cooldown / features / pool / request_log_keep_days /
// schedule / session_sticky / upstash / upstream）。丢掉的包括：
//
//	api_key      （对话凭据 —— 丢了客户端全部 401）
//	listen       （丢了就回落 Default() 的 ":7863" = 监听全部网卡）
//	auth_dir / state_file
//	注释（用户手写的说明）
//	全部上游段
//
// 两条原因**叠加**：
//
//	① 旧代码用 `json.Unmarshal` 读这个文件，而它**不认注释**。
//	   而本仓的 config.json 是**支持注释**的（JSONC，见 jsonc.go）——
//	   用户手写的 73 行说明让它解析失败。
//	② 解析失败时旧代码写的是：
//
//		_ = json.Unmarshal(raw, &doc) // 解析失败就用空文档重写
//
//	   把"解析失败"当成了"从空开始"，而"从空开始"= **丢掉用户的一切配置**
//	   然后 MarshalIndent 写回去。于是"点一下保存设置"就把配置擦了。
//
// 修法两条，都要有：
//
//	· 读用它该用的解析器（unmarshalJSONC，与 Load 同一条路径）
//	· **解析失败时绝不重写**：报错退出，让用户看到"保存失败"
//
// 第 2 条是这次的关键教训：对"用户的配置文件"，**任何读不懂的情形都必须
// 停下**，绝不能"当成空的"继续写 —— 那是把一次读失败放大成一次数据丢失。
func (s *settingsStore) persist(p admin.Settings) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(s.path); err == nil {
		// ⚠ 用 unmarshalJSONC（不是 json.Unmarshal）：config.json 允许写注释，
		// 而标准库不认。这里读不懂就**必须**报错 —— 见函数头的事故记录。
		if err := unmarshalJSONC(raw, &doc); err != nil {
			return fmt.Errorf("读取现有配置失败，已中止保存（避免把配置写坏）: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取 %s 失败: %w", s.path, err)
	}

	// ⚠ 逐项**字节级**写回（不是解析成 map 再序列化）。
	//
	// 解析成 map 再 MarshalIndent 写回去会**丢掉注释与字段顺序** ——
	// 那正是用户上一轮报的「配置太乱」的同一个机制，也是本次事故里
	// "73 行注释全没了" 的直接原因。见 settings_edit.go 的文件头。
	sets := []managedSetting{
		{"schedule", "checkin_enabled", p.CheckinEnabled},
		{"schedule", "keepalive_enabled", p.KeepaliveEnabled},
		{"schedule", "checkin_hours", p.CheckinHours},
		{"schedule", "keepalive_hours", p.KeepaliveHours},
		{"cooldown", "soft_rate", p.SoftRate},
		{"upstream", "timeout_seconds", p.UpstreamTimeoutSec},
		{"upstream", "header_timeout_seconds", p.UpstreamHeaderTimeout},
		{"upstream", "idle_timeout_seconds", p.UpstreamIdleTimeout},
		{"pool", "max_in_flight", p.PoolMaxInFlight},
		{"pool", "breaker_threshold", p.BreakerThreshold},
		{"pool", "breaker_cooldown", p.BreakerCooldown},
		{"pool", "breaker_cooldown_max", p.BreakerCooldownMax},
		{"features", "sanitize_blacklist_fingerprints", p.SanitizeFingerprints},
		{"upstash", "url", p.UpstashURL},
		{"session_sticky", "enabled", p.SessionStickyEnabled},
		{"session_sticky", "ttl", p.SessionStickyTTL},
		{"admin", "checkin_log_keep_days", p.CheckinLogKeepDays},
		{"", "request_log_keep_days", p.RequestLogKeepDays},
	}
	// 上游段：由适配器给出它自己的键值（核心不认识这些键名，也不再列它们）。
	if s.up != nil {
		for k, v := range s.up.Values() {
			sets = append(sets, managedSetting{"admin", k, v})
		}
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("读取 %s 失败: %w", s.path, err)
		}
		raw = []byte("{}\n")
	}
	out, missing, err := applyManagedSettings(raw, sets)
	if err != nil {
		return err
	}
	// 别名键收敛：写入规范键 growth_auto_claim 后，删掉曾用过（且曾被静默忽略）的
	// growth_auto_claim_tasks，避免两个键并存给出互相矛盾的信号。
	// 字节级抹掉（保留行号），其余不动。
	out = blankKeyInSection(out, "admin", "growth_auto_claim_tasks")
	if len(missing) > 0 {
		log.Printf("设置：这些项原本不在 config.json 里，已插入 %v（其余内容逐字节保留）", missing)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return os.Rename(tmp, s.path)
}

// newSettingsStore 组装宿主实现。
//
// up 是上游的设置扩展适配器（可为 nil —— 该上游没有专属设置）。
func newSettingsStore(path string, cfg *Config, sch *scheduler.Scheduler, up UpstreamSettings,
	log *checkinlog.Log, setLogKeepDays func(int)) admin.SettingsStore {
	return &settingsStore{
		path:           path,
		cfg:            cfg,
		sch:            sch,
		up:             up,
		log:            log,
		setLogKeepDays: setLogKeepDays,
	}
}
