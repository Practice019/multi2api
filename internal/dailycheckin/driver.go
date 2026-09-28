// Package dailycheckin 签到类动作的**共享驱动**。
//
// 详见 doc.go（含"为什么不 import gateway"这条架构约束的完整说明）。
package dailycheckin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"workbuddy2api/internal/checkinlog"
)

// Upstream 一个"有签到类每日动作"的上游需要提供的事实。
//
// 只三个方法 —— 其余（批量端点、定时任务、写历史、状态归一化）
// 都由本包完成。这正是"共用接口"的含义：上游只描述**它自己的事实**，
// 共同的流程不再复制。
type Upstream interface {
	// Accounts 列出本上游当前要处理的账号。
	//
	// 实现应当只扫**自己的**凭证目录（各上游的账号互不可见）。
	// 返回的错误会让本次任务整体中止（下一轮再试）。
	Accounts(ctx context.Context) ([]Account, error)

	// Status 查一个账号今天的签到状态。
	//
	//	signedIn 今天已签（true 时驱动**不会**再调 Claim）
	//	enabled  上游当前开放签到（false 时跳过该账号，**不记历史**）
	//	err      查询失败（驱动记 StatusFail 并继续下一个账号）
	//
	// ⚠ `enabled=false` 与 `err != nil` 必须分开：
	// 前者是"上游今天不开放"（正常，不该污染历史），
	// 后者是"我们没查到"（要让用户看见）。
	Status(ctx context.Context, a Account) (signedIn bool, enabled bool, err error)

	// Claim 替一个账号领今天的签到。
	//
	// 返回领到的数量（用于历史里的 Credits 与界面展示；无此概念的
	// 上游返回 0）。错误会让驱动记 StatusFail。
	Claim(ctx context.Context, a Account) (credits int64, err error)
}

// Account 驱动眼里的一个账号。
//
// 只带 uid 与一份**不透明**的上游凭证 —— 驱动不需要认识凭证结构，
// 它只是把凭证原样传回给同一个上游的 Status / Claim。
type Account struct {
	// UID 账号池主键（写历史与日志用）。
	UID string
	// Secret 上游私有的凭证值（驱动不解释它）。
	Secret any
}

// Descriptor 本上游签到动作的**展示与路由**信息。
//
// 这些是协议事实（ID 会进 data-act、路径会进 URL），必须由上游给出 ——
// 但它们的**形状**是统一的，所以放在本包而不是各上游各写一份
// （各写一份就会像现在这样：路径命名风格、ID 前缀、Batch 标记各不相同）。
type Descriptor struct {
	// ID 动作的稳定标识（如 "lobsterai-checkin"）。
	//
	// ⚠ 必须跨上游唯一（前端按它写 data-act）。约定 `<provider>-checkin`。
	ID string
	// Label 按钮文字（如 "签到"）。
	Label string
	// Title 悬停说明。
	Title string
	// OneURL 单账号端点（前端行内按钮 POST 它，body 带 uid）。
	OneURL string
	// AllURL 全量端点（前端分组/批量按钮 POST 它，无参数）。
	AllURL string
}

// Driver 把一个 Upstream + Descriptor 接成完整的签到能力。
//
// # 上游侧的全部成本：实现三个方法 + 写三行转发
//
//	func (p *Provider) DailyActions() []gateway.DailyAction { … }
//	func (p *Provider) Jobs() []gateway.Job                 { … }
//	func (p *Provider) AdminRoutes() []gateway.AdminRoute   { … }
//
// ⚠ 那三行**必须在各上游包内**写（不能下沉到本包）—— 因为本包不 import
// gateway，见 doc.go。代价是三行样板，收益是架构约束成立
//（第一次构建时 arch_test 就把"上游依赖上游"判成了违规）。
type Driver struct {
	up   Upstream
	desc Descriptor
	log  *checkinlog.Log
	// interval 定时任务的间隔（<=0 表示不注册自动签到）。
	interval time.Duration
	// enabled 是否注册自动签到任务（false 时只保留手动按钮）。
	enabled bool
}

// Options 构造参数。
type Options struct {
	// Log 签到历史（nil = 不记历史，仅日志）。
	//
	// ⚠ 传入 nil 是**合法**的（某些部署不落盘历史），但"不记历史"的
	// 直接后果就是界面上「今日签到」列永远为空 —— 那正是本包要修的
	// 形态之一，所以各上游的装配层应当把 checkinlog 传进来。
	Log *checkinlog.Log
	// Interval 自动签到间隔；<=0 = 不注册定时任务。
	Interval time.Duration
	// Enabled 是否启用自动签到（与 Interval 分开：可以"有按钮但不自动"）。
	Enabled bool
}

// New 构造驱动。
//
// ⚠ desc 用**值**类型：它是不可变的事实（路径/文案），
// 传值可以避免调用方事后改动影响已构造的驱动。
func New(up Upstream, desc Descriptor, opt Options) *Driver {
	return &Driver{
		up:       up,
		desc:     desc,
		log:      opt.Log,
		interval: opt.Interval,
		enabled:  opt.Enabled,
	}
}

// Ready 驱动是否可用（未接线时上游侧应当不注册任何东西）。
func (d *Driver) Ready() bool { return d != nil && d.up != nil }

// Descriptor 返回展示与路由信息（供上游侧拼 gateway 类型）。
func (d *Driver) Descriptor() Descriptor {
	if d == nil {
		return Descriptor{}
	}
	return d.desc
}

// AutoEnabled 自动签到是否启用（供上游侧决定要不要注册 Job）。
func (d *Driver) AutoEnabled() bool {
	return d.Ready() && d.enabled && d.interval > 0
}

// Interval 自动签到间隔（供上游侧填 gateway.Job.Interval）。
func (d *Driver) Interval() time.Duration {
	if d == nil {
		return 0
	}
	return d.interval
}

// ── 执行与写历史（本包的核心：唯一一处）────────────────────────────────

// Result 一个账号的签到结果。
type Result struct {
	UID     string `json:"uid"`
	Status  string `json:"status"`
	Credits int64  `json:"credits,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// CheckinOne 签一个账号，并写历史。
//
// 这是**唯一**的签到实现 —— 单账号端点、全量端点、定时任务三条路径
// 全部调它。所以"记得写历史"只在这里成立一次，不存在"某个入口忘了写"
//（那正是 trae 当年踩过的 bug，也是 lobsterai/qoder 移植时漏掉的那处）。
func (d *Driver) CheckinOne(ctx context.Context, acc Account, trigger string) Result {
	if !d.Ready() {
		return Result{UID: acc.UID, Status: checkinlog.StatusFail, Detail: "签到未接线"}
	}
	if err := ctx.Err(); err != nil {
		return Result{UID: acc.UID, Status: checkinlog.StatusFail, Detail: err.Error()}
	}

	signedIn, enabled, err := d.up.Status(ctx, acc)
	if err != nil {
		// 查不到 ≠ 签到失败，但对用户而言都是"今天没签上"，
		// 记 fail 让他看得见原因。
		d.record(acc.UID, checkinlog.StatusFail, shortErr(err), 0, trigger)
		log.Printf("%s: 签到状态查询失败 uid=%s: %v", d.desc.ID, shortUID(acc.UID), err)
		return Result{UID: acc.UID, Status: checkinlog.StatusFail, Detail: shortErr(err)}
	}
	if !enabled {
		// 上游今天不开放签到：**不记历史**。
		//
		// ⚠ 与 `err != nil` 严格分开（见 Upstream.Status 的注释）：
		// 记成 fail 会把"上游没开这个活动"染成用户的失败，
		// 而且是每天一条的噪音。
		return Result{UID: acc.UID, Status: checkinlog.StatusSkip, Detail: "上游未开放签到"}
	}
	if signedIn {
		d.record(acc.UID, checkinlog.StatusAlready, "今天已签到", 0, trigger)
		return Result{UID: acc.UID, Status: checkinlog.StatusAlready, Detail: "今天已签到"}
	}

	credits, cerr := d.up.Claim(ctx, acc)
	if cerr != nil {
		d.record(acc.UID, checkinlog.StatusFail, shortErr(cerr), 0, trigger)
		log.Printf("%s: 签到失败 uid=%s: %v", d.desc.ID, shortUID(acc.UID), cerr)
		return Result{UID: acc.UID, Status: checkinlog.StatusFail, Detail: shortErr(cerr)}
	}
	d.record(acc.UID, checkinlog.StatusOK, "", credits, trigger)
	log.Printf("%s: 签到成功 uid=%s credits=%d", d.desc.ID, shortUID(acc.UID), credits)
	return Result{UID: acc.UID, Status: checkinlog.StatusOK, Credits: credits}
}

// RunAll 扫本上游全部账号签一遍（全量端点与定时任务共用）。
//
// 签名就是 gateway.Job.Run 的形状（`func(ctx) error`）—— 上游侧填 Job 时
// 直接写 `Run: p.checkin.RunAll` 即可，不必包一层。
func (d *Driver) RunAll(ctx context.Context) error {
	if !d.Ready() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	accounts, err := d.up.Accounts(ctx)
	if err != nil {
		return fmt.Errorf("%s: 列账号失败: %w", d.desc.ID, err)
	}
	for _, acc := range accounts {
		if acc.UID == "" {
			continue
		}
		// 每个账号一次 ctx.Err() 检查：全量任务可能扫几十个号，
		// 中途被取消（换班/退出）时应当尽快停，而不是把剩下的打完。
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.CheckinOne(ctx, acc, "sched")
	}
	return nil
}

// ── HTTP 端点（单账号 / 全量）──────────────────────────────────────────
//
// 两个 handler 的签名就是 http.HandlerFunc —— 上游侧拼 gateway.AdminRoute
// 时直接写 `Handler: p.checkin.HandlerOne`，不必包一层。

// HandlerOne POST {OneURL} —— 单账号签到。
//
// # 入参：uid（query 或 JSON body 都认）
//
// 两种都认是**为了统一**：本项目既有的每日动作端点一律用 body {"uid":…}
//（前端 dayUrl 通道如此），但手工 curl 时 query 更顺手。
// 各上游此前写法不一 —— 那正是"接口不统一"的一部分。
func (d *Driver) HandlerOne(w http.ResponseWriter, r *http.Request) {
	if !d.Ready() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"ok": false, "error": "签到未接线",
		})
		return
	}
	uid, err := d.resolveUID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	acc, ok := d.accountOf(r.Context(), uid)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "账号不在本上游（uid=" + uid + "）",
		})
		return
	}
	res := d.CheckinOne(r.Context(), acc, "manual")
	// 回执形状统一为 {ok, result:{status,detail,credit}} ——
	// 前端已有一个分支认这个形状（见 webui.html 的 `r.result` 处理），
	// 所以沿用它能**零改动**接入既有渲染。
	//
	// ⚠ "skip" 也算 ok：它表示"上游今天没开放"，不是用户操作失败。
	// 判 bad 只按 fail（与 quotaScopeToast 的分档同一条原则）。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": res.Status != checkinlog.StatusFail,
		"result": map[string]any{
			"status": res.Status,
			"detail": res.Detail,
			"credit": res.Credits,
		},
	})
}

// HandlerAll POST {AllURL} —— 全量签到（无参数）。
//
// 回执 {ok, claimed:N, results:[…]}：与 codearts 福利领取同一形状，
// 前端已有分支认它（`typeof r.claimed === 'number'`）。
func (d *Driver) HandlerAll(w http.ResponseWriter, r *http.Request) {
	if !d.Ready() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"ok": false, "error": "签到未接线",
		})
		return
	}
	accounts, err := d.up.Accounts(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": "列账号失败: " + err.Error(),
		})
		return
	}
	results := make([]Result, 0, len(accounts))
	var claimed int
	for _, acc := range accounts {
		if acc.UID == "" {
			continue
		}
		if r.Context().Err() != nil {
			break
		}
		res := d.CheckinOne(r.Context(), acc, "manual")
		results = append(results, res)
		if res.Status == checkinlog.StatusOK {
			claimed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "claimed": claimed, "results": results,
	})
}

// resolveUID 从 query 或 body 取 uid（两者都认，且**都缺才报错**）。
func (d *Driver) resolveUID(r *http.Request) (string, error) {
	if uid := r.URL.Query().Get("uid"); uid != "" {
		return uid, nil
	}
	var body struct {
		UID string `json:"uid"`
	}
	// body 可能为空（GET 或空 POST）—— 忽略解码错误，按"没给"处理。
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.UID != "" {
		return body.UID, nil
	}
	return "", errMissingUID
}

// accountOf 在**本上游**的账号里找 uid 对应的那份凭证。
//
// 找不到就 404（而不是拿一份空凭证去签）—— 与 reload / 额度刷新同一条
// 原则：显式传了却查不到要刺眼地失败。
func (d *Driver) accountOf(ctx context.Context, uid string) (Account, bool) {
	accounts, err := d.up.Accounts(ctx)
	if err != nil {
		return Account{}, false
	}
	for _, acc := range accounts {
		if acc.UID == uid {
			return acc, true
		}
	}
	return Account{}, false
}

// record 写一条签到历史。
//
// ⚠ 这是界面上「今日签到」列的**唯一**数据来源（admin 读 checkinlog
// 的 KindCheckin 记录）。不写它 → 那一列永远是 `—`。
func (d *Driver) record(uid, status, detail string, credits int64, trigger string) {
	if d == nil || d.log == nil {
		return
	}
	d.log.Append(checkinlog.Record{
		At:      time.Now(),
		UID:     checkinlog.NormalizeUID(uid),
		Kind:    checkinlog.KindCheckin,
		Status:  status,
		Detail:  detail,
		Credits: credits,
		Trigger: trigger,
	})
}

// errMissingUID uid 两个来源都缺时的错误（query 与 body 都没给）。
var errMissingUID = errors.New("缺少 uid（query ?uid= 或 JSON body 任选一个）")

func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8] + "…"
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
