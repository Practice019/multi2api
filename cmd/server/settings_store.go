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
	"strconv"
	"sync"
	"time"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/store"
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

	// optionsMu 守住下面那个「只导一次」的开关 —— Apply 会被 HTTP 并发调用。
	optionsMu sync.Mutex
	// optionsSeeded 「首次导入是否已成功」的开关。
	//
	// 只在**成功**之后置 true：库还没接进来时（PLAN §6.0 的切换点之前）它保持
	// false，于是宿主一旦把库打开，下一次 Apply 就会补做导入，而不是永远错过。
	optionsSeeded bool
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
		ReviveProbeHours:      reviveProbeHoursOf(c.Pool.ReviveProbeIntervalSeconds),
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
	// ⚠ 从 admin 取，**不再在这里抄一份**：两边各一份时加字段必然漏一处，
	//   而漏的那一处表现是"PUT 说要重启、GET 的 restart_fields 里没有"。
	//   上游专属的"需重启"字段由各 SettingField.RestartRequired 声明，
	//   与这份通用段清单由 handler 的 restartFields() 合并 —— 那才是唯一出口。
	needRestart = admin.RestartRequiredFields()
	// 把新值同步进内存 cfg，让 Snapshot 显示的就是刚保存的值。
	s.applyToMemory(p)
	// 再镜像进 options 表，让 SQL 侧与本文件里的值同源（PLAN §6.0 的迁移方向）。
	// 失败**不改**本次保存的结论：config.json 已经写成功了。
	s.mirrorOptions(p)
	return applied, needRestart, nil
}

// reviveProbeHoursOf 把 config 里的秒换算成设置页展示用的**小时**。
//
// # 为什么对外用小时、对内用秒
//
// 这个旋钮的粒度就是"天"级（默认 24 小时），用秒表示会得到 86400 这种
// 一眼读不出的数；用小时则是 24 —— 用户能直接看懂并改。
// 而 config 里沿用同族其它 interval 的秒制（health_check_interval_seconds）。
//
// # ⚠ 取整方向：**向上**取整，不能是四舍五入
//
// 四舍五入有个危险边界：secs=899（不到 1 小时但**开着**）会显示成 0 ——
// 而 0 在本页的语义是"关闭"。用户会看到"解封探测：关闭"，
// 但后台其实每 15 分钟探一次。向上取整后 899→1（"1 小时"），
// **非零值永远显示为非零**，不会把"开着"说成"关着"。
//
// 代价：手改 config 成 5400（1.5h）时页面显示 2 小时。但那只影响**显示** ——
// 调度器用的是启动时读到的 5400，页面不保存就不会改它；
// 用户一旦保存，他本来就是在把值改成整数小时，不算意外。
func reviveProbeHoursOf(secs int) int {
	if secs <= 0 {
		return 0 // 关闭态必须原样透出，不能被取整成 1
	}
	return (secs + 3599) / 3600
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
		ReviveProbeHours:      reviveProbeHoursOf(c.Pool.ReviveProbeIntervalSeconds),
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
	c.Pool.ReviveProbeIntervalSeconds = p.ReviveProbeHours * 3600
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
	if p.ReviveProbeHours < 0 {
		return fmt.Errorf("解封探测间隔不能为负（0 = 关闭）")
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
	sets := managedSettingsFor(p)
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

// managedSettingsFor 受管设置清单：persist 与 options 镜像**共用这一份**。
//
// # 为什么必须是同一份
//
// 两份清单漂移的表现是「写进 config.json 的键」与「写进 options 的键」不再一一对应，
// 而两边都不报错 —— 于是界面显示的值与库里存的值从此不同源，且没有任何地方会红。
// 本仓已经被同一个形态咬过一次（admin 的 runtime/restart 清单漂移，见
// internal/admin/settings.go 里 restartRequiredFields 的注释），所以这里从 persist 里
// 把它抽出来给两处用，而不是各写一份。
//
// section 为空串表示**顶层键**（request_log_keep_days）。
func managedSettingsFor(p admin.Settings) []managedSetting {
	return []managedSetting{
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
		{"pool", "revive_probe_interval_seconds", p.ReviveProbeHours * 3600},
		{"features", "sanitize_blacklist_fingerprints", p.SanitizeFingerprints},
		{"upstash", "url", p.UpstashURL},
		{"session_sticky", "enabled", p.SessionStickyEnabled},
		{"session_sticky", "ttl", p.SessionStickyTTL},
		{"admin", "checkin_log_keep_days", p.CheckinLogKeepDays},
		{"", "request_log_keep_days", p.RequestLogKeepDays},
	}
}

// optionKeyOf 把 (section, key) 拼成 options 的键 —— 与 config.json **摊平后**的
// 点号路径逐字一致（internal/store 的 flattenConfigJSON："a.b.c"，顶层键没有点）。
//
// 不一致的表现很隐蔽：导入那一路写的是 "schedule.checkin_enabled"，而镜像写成
// "checkin_enabled" ⇒ 同一个设置在库里有两行，读侧按其中一个键读就永远读不到新值。
func optionKeyOf(section, key string) string {
	if section == "" {
		return key
	}
	return section + "." + key
}

// optionValueString 把受管值编成 options 里的字符串形态。
//
// 口径必须与 internal/store 的 flattenConfigJSON 一致（导入那一路用的就是它）：
// 字符串原样、布尔 "true"/"false"、整数十进制、数组紧凑 JSON（"[7,19]"）。
// 不一致的表现是「导入时是 [7,19]，保存一次之后变成 \"[7,19]\"（多一层引号）」，
// 而读侧两种形态都"看得懂"，于是它不会报错，只会慢慢失真。
func optionValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	default:
		// 受管清单里只可能出现 int / bool / string / []int；[]int 走这里。
		b, err := json.Marshal(v)
		if err != nil {
			// 到这一步说明清单加了新类型而没给它字符串形态。写成空串也不 panic，
			// 但**必须**在日志里可见 —— 静默的空值是最难查的一种。
			log.Printf("设置：受管值无法编码成 option 字符串（%T）: %v", v, err)
			return ""
		}
		return string(b)
	}
}

// seedOptionsFromConfig 首次把 config.json 的可导入键写进 options（幂等）。
//
// # 为什么是「只在键不存在时写」
//
// internal/store.ImportFromConfigJSON 用的是 INSERT ... ON CONFLICT DO NOTHING。
// 反过来（每次启动都覆盖）的后果是：用户在界面上改过的值会在他下次重启时被悄悄
// 冲回配置文件里的旧值，而他只会以为「界面保存没生效」。
//
// # 库还没接进来时是 no-op
//
// 返回 ErrNotOpen 就原样返回、**不置 optionsSeeded**：PLAN §6.0 的切换点（T11 把库
// 接进宿主）之前，config.json 仍是唯一权威；一旦库打开了，下一次 Apply 会自动补做。
func (s *settingsStore) seedOptionsFromConfig() {
	s.optionsMu.Lock()
	defer s.optionsMu.Unlock()
	if s.optionsSeeded {
		return
	}
	n, err := store.ImportFromConfigJSON(s.path)
	switch {
	case errors.Is(err, store.ErrNotOpen):
		return // 库还没接进来；保持 false，下次再试
	case err != nil:
		log.Printf("设置：首次导入 options 失败（config.json 仍是权威）: %v", err)
		return
	}
	s.optionsSeeded = true
	if n > 0 {
		log.Printf("设置：已把 config.json 的 %d 个可热改键首次导入 options", n)
	}
}

// mirrorOptions 把本次保存的通用段镜像进 options 表。
//
// # 与 config.json 的关系
//
// config.json 仍是这次保存的**落盘处**（逐字节保留注释与陌生键，见 persist）；
// options 是同一份值的 SQL 侧镜像，供设置页按名读、供将来的 SQL 聚合读。
// 两处写的是**同一份清单**（managedSettingsFor），这就是它们不会漂移的保证。
//
// # 为什么失败不报错给调用方
//
// 走到这一步时 config.json 已经写成功了。此时把 PUT 判失败只会让用户以为「没保存」，
// 而磁盘上其实已经落了 —— 那是个会诱发重复操作的假信号。所以这里只记日志。
// 库没接（ErrNotOpen）时**静默**跳过：那是当前设计的正常状态，不是故障。
//
// # 上游段的边界
//
// 本函数只管**通用段**。上游段的键值形态由上游决定（可能不是标量），宿主不替它
// 决定字符串形态；它们由 seedOptionsFromConfig 的摊平路径覆盖。
func (s *settingsStore) mirrorOptions(p admin.Settings) {
	s.seedOptionsFromConfig()
	sets := managedSettingsFor(p)
	pairs := make([]store.OptionPair, 0, len(sets))
	for _, ms := range sets {
		pairs = append(pairs, store.OptionPair{
			Key:   optionKeyOf(ms.section, ms.key),
			Value: optionValueString(ms.value),
		})
	}
	if err := store.PutOptions(pairs); err != nil {
		if errors.Is(err, store.ErrNotOpen) {
			return
		}
		log.Printf("设置：写入 options 失败（config.json 已保存，JSON 仍是权威）: %v", err)
	}
}

// newSettingsStore 组装宿主实现。
//
// up 是上游的设置扩展适配器（可为 nil —— 该上游没有专属设置）。
func newSettingsStore(path string, cfg *Config, sch *scheduler.Scheduler, up UpstreamSettings,
	log *checkinlog.Log, setLogKeepDays func(int)) admin.SettingsStore {
	st := &settingsStore{
		path:           path,
		cfg:            cfg,
		sch:            sch,
		up:             up,
		log:            log,
		setLogKeepDays: setLogKeepDays,
	}
	// 首次启动把 config.json 的可热改键导入 options（幂等；未接库时是 no-op）。
	st.seedOptionsFromConfig()
	return st
}
