// settings.go workbuddy 自己的设置项（核心通过适配器消费，见 SettingsExt 的说明）。
//
// # 为什么设置项要跟着上游走
//
// 改造前这些开关是 internal/admin/settings.go 里**写死的九个字段**：
//
//	TravelAutoClaim    bool `json:"travel_auto_claim"`
//	GrowthAutoAccept   bool `json:"growth_auto_accept_tasks"`
//	GrowthAutoOpen     bool `json:"growth_auto_open"`
//	…
//
// 加上 restart_fields / runtime_fields 两张写死的字符串表，共 19 处上游概念。
// 后果是：核心的 Settings struct 里出现了 codearts 永远不会有的字段，
// 而且加第三个上游时还要继续往同一个 struct 里塞。
//
// 现在它们由**上游声明**：核心只负责把 Fields() 合并进 /admin/settings 的响应、
// 把 PUT 补丁原样交给 ApplySettings。核心不认识 "growth_auto_open" 这个键。
//
// # 与 admin.SettingsExt 的关系
//
// 接口定义在消费方（admin），本包实现它 —— workbuddy 不 import admin
// （架构约束），靠方法签名匹配自动满足。转换在 cmd/server 的适配器里做
// （Fields() 的 admin.SettingField 类型需要一次逐字段搬运）。
package workbuddy

import (
	"encoding/json"
)

// 设置键名。它们是**对外契约**：既是前端读写的 JSON 键，
// 也是 config.json 里的键，改动会破坏已保存的配置。
const (
	keyTravelAutoClaim    = "travel_auto_claim"
	keyTravelWatchSeconds = "travel_watch_interval_seconds"

	keyGrowthAutoClaim  = "growth_auto_claim"
	keyGrowthAutoAccept = "growth_auto_accept_tasks"
	keyGrowthAutoMakeup = "growth_auto_makeup"
	keyGrowthAutoRedeem = "growth_auto_redeem"
	keyGrowthAutoOpen   = "growth_auto_open"
	keyGrowthAutoDraw   = "growth_auto_draw"

	keyGrowthWatchSeconds = "growth_watch_interval_seconds"
)

// SettingField workbuddy 的一个设置项。
//
// 与 admin.SettingField 结构相同、定义独立（上游不得 import admin）。
// Value 用 any：这些开关有 bool 也有 int（间隔秒数），
// 核心不解释它，只原样序列化。
type SettingField struct {
	Key             string
	Value           any
	RestartRequired bool
}

// SettingsExt 上游自己的设置项（形状由本包声明，核心有一个同名的）。
//
// # 与方法名/类型有关的一条硬事实
//
// 核心（admin.SettingsExt）的 Fields() 返回的是 **admin.SettingField** ——
// 上游不得 import admin，所以本包必须自己声明一份结构相同的 SettingField。
// 两边类型不同 ⇒ Go 的方法集精确匹配不成立 ⇒
// `gateway.ExtOf[admin.SettingsExt](provider)` **必然失败，而且不报错**
// （表现成设置页少几个键）。
//
// 所以设置这条线走**显式适配器**（cmd/server 的 upstreamSettingsAdapter），
// 而不是像 AdminExt / JobExt 那样靠类型断言自动发现 —— 那两条能自动发现，
// 是因为它们的接口类型全部定义在 gateway 里（只有一种定义）。
//
// 本接口的作用是固定那个适配器要满足的形状，并给 Provider 一个编译期断言。
type SettingsExt interface {
	// Fields 当前生效的上游专属设置项。顺序稳定（前端按序渲染）。
	Fields() []SettingField
	// ApplySettings 应用补丁中属于本上游的键。
	//
	// 为什么给**整份补丁**而不是预先切好的一份：切分需要核心知道 Key 的归属，
	// 那就等于核心认识上游字段了 —— 与这个接口存在的理由相反。
	ApplySettings(patch []byte) (applied []string, err error)
}

// Fields 返回本上游的设置项（SettingsExt 的实现）。
//
// 方法名跟随消费方（admin 用 Fields），但**不构成**对 admin.SettingsExt 的满足 ——
// 返回类型不同。见上面 SettingsExt 的说明。
func (p *Provider) Fields() []SettingField { return p.SettingsFields() }

// SettingsFields 返回本上游的设置项（Fields 的可读同义名）。
//
// # 哪些是"运行时生效"、哪些"需重启"
//
//   - 六个自动动作开关 + 旅行自动领奖：**运行时生效**。它们只是
//     Provider 内存里的 bool（见 growthWatchState.auto* / travelWatchState.autoClaim），
//     改完下一轮守卫立刻按新值走。
//   - 两个守卫轮间隔：**需重启**。间隔在调度器注册 gateway.Job 时就被读走了
//     （见 Provider.Jobs 的 Interval），运行中改不了它 —— 如实归入需重启，
//     而不是假装生效（假装会让用户以为改了但实际没变）。
func (p *Provider) SettingsFields() []SettingField {
	if p == nil {
		return nil
	}
	accept, makeup, redeem, open, draw, claim := p.GrowthToggles()

	// 旅行自动领奖：p.travel 为 nil（未构造）时报告 false，
	// 与 TravelAutoClaimEnabled() 的行为一致。
	autoClaim := p.TravelAutoClaimEnabled()

	return []SettingField{
		// 顺序稳定且与改造前 admin.Settings 的字段声明顺序一致，
		// 便于人工比对两份响应。
		{Key: keyTravelAutoClaim, Value: autoClaim},

		// GrowthAutoClaim 领奖：把条件已达成但未领的任务奖励领回来，
		// 是唯一让积分到账的动作。纯收益，默认开。
		{Key: keyGrowthAutoClaim, Value: claim},
		// GrowthAutoAccept 接单：只是把任务接进列表开始计进度，**不发奖励**，默认开。
		// 默认开的原因见 cmd/server/config.go 里同名字段的注释：
		// not_accepted 只在「还没领到第一只 Buddy」的窄窗口存在，官方前端连按钮都不给。
		{Key: keyGrowthAutoAccept, Value: accept},
		// 补签是纯收益（只花补签卡）；下面三个会消耗能量/连登天数/抽奖次数，默认关。
		{Key: keyGrowthAutoMakeup, Value: makeup},
		{Key: keyGrowthAutoRedeem, Value: redeem},
		{Key: keyGrowthAutoOpen, Value: open},
		{Key: keyGrowthAutoDraw, Value: draw},

		{Key: keyTravelWatchSeconds, Value: int(p.WatchInterval().Seconds()), RestartRequired: true},
		{Key: keyGrowthWatchSeconds, Value: int(p.GrowthWatchInterval().Seconds()), RestartRequired: true},
	}
}

// ApplySettings 应用补丁中属于本上游的键（实现本包的 SettingsExt 接口）。
//
// # 为什么收整份补丁
//
// 预先切分需要核心知道 Key 的归属，那就等于核心认识上游字段了 ——
// 与那个接口存在的理由相反。这里自己挑出认得的键，不认得的忽略。
//
// # 只接**内存**里的运行时开关
//
// 那六个 bool 直接改 Provider 的内存状态（下一轮守卫即生效）。
// 需要重启的两个间隔**不在这里改**：它们由 cmd/server 的 settingsStore
// 写进 config.json，重启后经 workbuddy.Config 生效。这里对它们不做任何事，
// 但要在返回值里报告它们（否则前端会说"保存失败"）。
func (p *Provider) ApplySettings(patch []byte) ([]string, error) {
	if p == nil || len(patch) == 0 {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(patch, &raw); err != nil {
		return nil, err
	}

	var applied []string

	// 六个自动动作：任一存在就用**整份当前值 + 补丁覆盖**写回，
	// 保持与 SetGrowthToggles 的全量语义一致（它不接受"只改一个"）。
	if anyGrowthKey(raw) {
		accept, makeup, redeem, open, draw, claim := p.GrowthToggles()
		if b, ok := boolField(raw, keyGrowthAutoAccept); ok {
			accept = b
		}
		if b, ok := boolField(raw, keyGrowthAutoMakeup); ok {
			makeup = b
		}
		if b, ok := boolField(raw, keyGrowthAutoRedeem); ok {
			redeem = b
		}
		if b, ok := boolField(raw, keyGrowthAutoOpen); ok {
			open = b
		}
		if b, ok := boolField(raw, keyGrowthAutoDraw); ok {
			draw = b
		}
		if b, ok := boolField(raw, keyGrowthAutoClaim); ok {
			claim = b
		}
		p.SetGrowthToggles(accept, makeup, redeem, open, draw, claim)
		for _, k := range []string{
			keyGrowthAutoClaim, keyGrowthAutoAccept, keyGrowthAutoMakeup,
			keyGrowthAutoRedeem, keyGrowthAutoOpen, keyGrowthAutoDraw,
		} {
			if _, ok := raw[k]; ok {
				applied = append(applied, k)
			}
		}
	}

	// 旅行自动领奖：单值开关。
	if b, ok := boolField(raw, keyTravelAutoClaim); ok {
		p.SetTravelAutoClaim(b)
		applied = append(applied, keyTravelAutoClaim)
	}

	// 两个守卫轮间隔：需重启，由 cmd/server 写盘，这里只报告。
	for _, k := range []string{keyTravelWatchSeconds, keyGrowthWatchSeconds} {
		if _, ok := raw[k]; ok {
			applied = append(applied, k)
		}
	}

	return applied, nil
}

// anyGrowthKey 补丁里是否出现任一成长开关键。
func anyGrowthKey(raw map[string]json.RawMessage) bool {
	for _, k := range []string{
		keyGrowthAutoClaim, keyGrowthAutoAccept, keyGrowthAutoMakeup,
		keyGrowthAutoRedeem, keyGrowthAutoOpen, keyGrowthAutoDraw,
	} {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	return false
}

// boolField 从原始补丁里取一个 bool 字段；缺失或类型不符时 ok=false。
//
// 类型不符时**不报错**：前端可能传 "true"（字符串）之类的松散值，
// 而这里的正确行为是"当作没传"而不是让整次保存失败 ——
// 校验的职责在 cmd/server 的 validateSettings，这里只做取值。
func boolField(raw map[string]json.RawMessage, key string) (bool, bool) {
	v, ok := raw[key]
	if !ok {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false, false
	}
	return b, true
}

// 编译期断言：Provider 满足本包声明的设置扩展点（也就满足了 admin 的同名接口）。
var _ SettingsExt = (*Provider)(nil)
