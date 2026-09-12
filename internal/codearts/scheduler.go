package codearts

import (
	"context"
	"log"
	"time"
)

// RefreshScheduler 后台主动续期。
//
// 为什么需要它：CodeArts 的 STS 凭证只有约 30 分钟寿命。
// 只用请求路径的惰性续期（ChatStream 里发现将过期才刷）有个体验问题 ——
// 网关空闲半小时后，第一个请求要先付一次续期往返（实测 1–3s）才发得出去，
// 首尾延迟翻倍。后台按周期预热可以把这个代价挪到空闲时间。
//
// **并发安全是本模块的核心约束**：
// refresh_token 是消费型的（用一次即作废，实测 STS5.1806）。
// 若后台调度与请求路径同时对一个账号发起续期，两边会拿**同一个** refresh_token
// 去换 —— 一个成功、另一个必然失败，甚至可能把刚拿到的新 token 也搅乱。
// 因此所有续期入口都必须经由 Auth.RefreshMu 串行化（见 client.RefreshToken）。
type RefreshScheduler struct {
	backend *Backend

	// Interval 扫描周期，默认 60s。
	//
	// 为什么是 60s：token 寿命 30 分钟，60s 粒度最坏情况是提前约 4 分钟续期，
	// 无害；再密也没意义（只会多消耗 refresh_token）。
	Interval time.Duration

	// Skew 提前续期窗口，默认 5 分钟。
	// 必须显著小于 token 寿命（30 分钟），否则刚换来的凭证立刻又被判定"将过期"。
	Skew time.Duration

	// OnRefreshed 续期成功后的回调（可选，用于日志/指标）。
	OnRefreshed func(uid string)
}

// NewRefreshScheduler 构造调度器（零值走默认）。
func NewRefreshScheduler(b *Backend) *RefreshScheduler {
	return &RefreshScheduler{
		backend:  b,
		Interval: 60 * time.Second,
		Skew:     5 * time.Minute,
	}
}

// Run 阻塞运行直到 ctx 取消。
//
// 启动时先跑一次（覆盖"进程刚起来时凭证已临近过期"的情形），
// 之后按 Interval 周期执行。
func (s *RefreshScheduler) Run(ctx context.Context) {
	if s.backend == nil {
		return
	}
	iv := s.Interval
	if iv <= 0 {
		iv = 60 * time.Second
	}

	// 启动即扫一次：若进程重启时凭证只剩几分钟，不必等到下一个周期。
	s.tick()

	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 正常退出（SIGTERM / 服务关闭）。不做额外清理 ——
			// 本 goroutine 不持有需要释放的资源，return 即可。
			return
		case <-t.C:
			s.tick()
		}
	}
}

// tick 扫一遍账号池，对将要过期的账号主动续期。
//
// 单个账号失败只记日志：一个号的问题不应影响其它号，也不该让调度器退出。
func (s *RefreshScheduler) tick() {
	skew := s.Skew
	if skew <= 0 {
		skew = 5 * time.Minute
	}

	accounts := s.backend.Accounts()
	var need []*Auth
	for _, proj := range accounts {
		c, err := s.backend.resolve(proj)
		if err != nil {
			continue
		}
		if c.NeedsRefresh(skew) && c.RefreshToken != "" {
			need = append(need, c)
		}
	}
	if len(need) == 0 {
		return
	}

	log.Printf("codearts: 后台续期开始，%d 个账号临近过期（窗口 %v）", len(need), skew)

	// 串行续期。并发没有收益（每账号一次网络往返），
	// 却会让日志交错、也更难判断哪个失败对应哪个号。
	for _, c := range need {
		if err := s.backend.Client.RefreshToken(c); err != nil {
			log.Printf("codearts: 后台续期失败 (uid=%s): %v", c.UID, err)
			continue
		}
		// 同步回投影对象，让 /admin/accounts 立刻反映新过期时间
		s.backend.syncByUID(c)
		if s.OnRefreshed != nil {
			s.OnRefreshed(c.UID)
		}
		log.Printf("codearts: 后台续期成功 (uid=%s)，新过期 %s",
			c.UID, time.Unix(c.ExpiresAt, 0).Format(time.RFC3339))
	}
}

// syncByUID 把刷新后的凭证同步到已投影的 auth.Auth 上。
//
// 为什么不能只改 codearts.Auth：/admin/accounts 读的是投影对象的
// ExpiresAt，不同步的话管理台会一直显示旧过期时间，
// 用户会以为后台续期没生效。
func (b *Backend) syncByUID(c *Auth) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for proj, mapped := range b.byPtr {
		if mapped == c {
			proj.AccessToken = c.SecurityToken
			proj.RefreshToken = c.RefreshToken
			proj.ExpiresAt = c.ExpiresAt
		}
	}
}
