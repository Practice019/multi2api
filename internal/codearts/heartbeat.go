package codearts

import (
	"log"
	"sync"
	"time"
)

// heartbeatInterval 是**对话进行中**的 busy 续期间隔。
//
// ★ T-232：上游的并发闸不认"请求结束"，只认 `chat-session/heartbeat` 的 busy/idle
// （官方 session.rs:5-8 原文）。本仓原来只在请求开头发**一次** busy：
// 短请求没问题，但一条长对话（读流几分钟）中途会被上游按会话超时回收 ——
// 症状是"同一账号的可用并发数莫名下降 / 偶发 TM.00001041 说并发满"，
// 而错误本身看不出是心跳停了。
//
// 为什么是 30 秒：与官方一致（session.rs 的续期间隔）。上游的超时判定比它宽，
// 30s 是"足够安全、又不至于把心跳端点刷爆"的折中。
//
// ⚠ 它是**变量**而不是常量：测试要把它压到毫秒级来验证"确实续了、且失败不中断"。
// 若写成常量，真 30s 在毫秒级的测试里永远不会触发 ⇒ 那条路径变成**测不到的死代码**。
var heartbeatInterval = 30 * time.Second

// sessionKeepAlive 在一条对话期间周期性续 busy，直到 Stop。
//
// # 为什么不复用 gateway.Job / internal/scheduler（★ 本文件存在的原因）
//
// `gateway.Job{Name, Interval, Run(ctx), Due(now)}` 与 `internal/scheduler` 是
// **全局定时任务**框架：Due 按账号/时刻错峰，Run 拿不到"当前有哪几条对话在跑、
// 各自的 session id 是什么"。而心跳是**请求级**的 ——
// 它的 lifetime 是一条响应体（从 busy 占槽成功到 body 关闭）。
// 把请求状态塞进全局调度器，等于让调度器去维护一张"当前在跑的会话表"，
// 那才是真的造轮子（还多出一份状态要同步）。
//
// 所以这里用**最小的请求级实现**：一个 goroutine + 一个 ticker + 一个 Stop。
// 复用点在本包既有的 `Client.Heartbeat`（签名、头、body 全部照旧）。
type sessionKeepAlive struct {
	c    *Client
	a    *Auth
	id   string
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// startSessionKeepAlive 启动续期循环。调用方**必须**在结束前 Stop。
//
// 启动时机：占槽（busy）之后、开始读流之前 —— 请求本身也可能很长，
// 所以不能在"拿到响应头"之后才启动。
func startSessionKeepAlive(c *Client, a *Auth, id string) *sessionKeepAlive {
	k := &sessionKeepAlive{
		c:    c,
		a:    a,
		id:   id,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go k.loop()
	return k
}

func (k *sessionKeepAlive) loop() {
	defer close(k.done)
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-t.C:
			// ★ 铁律①：续期失败**只记日志**，绝不释放槽位、绝不退出循环。
			// 一次心跳超时 ≠ 对话结束 —— 退出反而会让槽位真的泄漏
			//（循环没了，后面的 idle 之前再也不会续期，上游按超时回收）。
			if err := k.c.Heartbeat(k.a, "busy", k.id); err != nil {
				log.Printf("codearts: 会话 %s busy 续期失败（不释放槽位）: %v", k.id, err)
			}
		}
	}
}

// Stop 停止续期并**等循环真正退出**。幂等（可重复调用）。
//
// ★ 铁律④：idle 必须发生在**最后一次 busy 完成之后**。
// 只 `close(stop)` 就立刻发 idle，会与"此刻正在途中的那次 busy"赛跑：
// 落后到达的 busy 会把刚释放的槽位又标成占用 —— 表现为槽位泄漏，且极难复现
//（取决于网络与调度的相对速度）。所以 Stop 必须等 done。
func (k *sessionKeepAlive) Stop() {
	if k == nil {
		return
	}
	k.once.Do(func() { close(k.stop) })
	<-k.done
}

// claimSession 占一个会话槽，并在**首次 busy 失败时尽力注销**该 id。
//
// ★ 铁律②：首次 busy 失败也要注销刚生成的 id。
//
// 为什么：失败可能是**传输层**的（超时、连接被中途断开），而上游很可能
// **已经占上了槽**。原实现只 `log.Printf` 就继续 —— 那个槽位就永久泄漏了
//（症状：某账号的可用并发数悄悄少一个，长期跑下来"明明没请求却报并发满"）。
//
// 注销是**尽力而为**：它自己失败也只能记日志（没有别的补救手段），
// 但至少把"静默泄漏"变成"日志里可查"。
//
// 失败仍然**不阻断**主流程 —— 与 Heartbeat 既有的取舍一致：
// 配额上报失败不该把"能出内容"降级成"完全不可用"。
func (c *Client) claimSession(a *Auth, id string) {
	if err := c.Heartbeat(a, "busy", id); err != nil {
		log.Printf("codearts: heartbeat busy 失败（继续）: %v", err)
		if ierr := c.Heartbeat(a, "idle", id); ierr != nil {
			log.Printf("codearts: 会话 %s 首次 busy 失败后注销也失败（可能泄漏一个槽位）: %v", id, ierr)
		}
	}
}
