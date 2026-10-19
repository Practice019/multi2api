package main

import (
	"context"
	"log"
	"time"

	"workbuddy2api/internal/account"
	"workbuddy2api/internal/authlimit"
)

// 反批量防御（T10）的接线辅助：**默认值只写在这里一处**。
//
// # 为什么默认值不直接写在 main 的那几行里
//
// 它们要同时出现在两个地方：`authlimit.NewLimiter(...)` 的实参与启动横幅
// （"限速 20 次/20m0s"）。三处各写一个字面量 20 的话，改了配置只生效一半、
// 而横幅会**理直气壮地报出一个错的数** —— 那种偏差只有出事时才被发现。
// 所以取值与它是**同一份**：这两个函数就是那份默认值的唯一出处。
//
// # 为什么 `<=0` 是"用缺省"而不是"不限速"
//
// 配置里的 0 同时可能是"没配"（Go 的零值）与"想关掉"。把 0 解释成不限速，
// 于是一个手滑（`"auth_rate_limit": 0`）就把闸门拆了，而且**看起来是配过的**
// （字段在文件里、有值）。想要"不限速"得改代码 —— 那一步必然要过评审的眼睛。
// 上限也一并夹住：100000 这种"等于没限"的值同样没有意义。
const (
	defAuthRateLimit     = 20
	defAuthRateWindowSec = 20 * 60 // 20 分钟（抄 new-api 的 20 次/20min）
	maxAuthRateLimit     = 10000
	maxAuthRateWindowSec = 24 * 60 * 60
	minAuthRateWindowSec = 10
)

// authRateLimit 取生效的注册/登录限速额度（次/窗口）。
func authRateLimit(cfg *Config) int {
	n := cfg.Console.AuthRateLimit
	if n <= 0 {
		return defAuthRateLimit
	}
	if n > maxAuthRateLimit {
		return maxAuthRateLimit
	}
	return n
}

// authRateWindowSeconds 取生效的限速窗口长度（秒）。
//
// 下限 10 秒是为了一件事：`authlimit.NewLimiter` 对 window<=0 是
// **fail-closed**（拒绝一切），而 1 秒的窗口在真实登录节奏下等于"第一下
// 之后就一直 429" —— 那会把控制台锁死，且症状是"登录按钮没反应"。
func authRateWindowSeconds(cfg *Config) int {
	n := cfg.Console.AuthRateWindowSeconds
	if n <= 0 {
		return defAuthRateWindowSec
	}
	if n < minAuthRateWindowSec {
		return minAuthRateWindowSec
	}
	if n > maxAuthRateWindowSec {
		return maxAuthRateWindowSec
	}
	return n
}

// captchaOrNil 把"可能为 nil 的指针"转成"真正为 nil 的接口"。
//
// ⚠ Go 的类型化 nil 坑：把 `(*authlimit.Captcha)(nil)` 赋给 `account.Captcha`
// 接口，接口值**不是** nil（它有类型、有值），于是 `h.cfg.Captcha == nil`
// 判不出来 —— 端点会认为"启用了人机校验"，然后拿一个 nil 出题器发题
// （`Issue` 方法在 nil 接收者上返回空串，见 authlimit.Captcha.Issue 的 `if c == nil`
// 兜底），前端拿到空题、答什么都不过。症状是"登录页永远提示请完成人机校验"。
//
// 这个转换必须**显式**做一次，不能靠调用方自觉。
func captchaOrNil(c *authlimit.Captcha) account.Captcha {
	if c == nil {
		return nil
	}
	return c
}

// pruneInterval 风控清理的节拍。
//
// 5 分钟是"内存占用"与"清理开销"的折中：限速窗口是 20 分钟，节拍远小于窗口，
// 于是"一个 IP 不再来了"最多 5 分钟就把它那行抹掉；而每次清理只是遍历一张
// 小表（单机规模：几个 IP）。
const pruneInterval = 5 * time.Minute

// startAuthLimitPruner 起一个按节拍清理风控表的 goroutine，ctx 结束时收尾。
//
// # 为什么清理要"调用方驱动"，而不是各包自己起 goroutine
//
// 两个理由，第二个更重要：
//
//	① 生命周期归 main 管：测试里 new 出来的限速器**不会**偷偷跑定时器
//	   （否则 `go test` 会在退出时看到 goroutine 泄漏警告，或者更糟：泄露
//	   一个还在改内存的 goroutine）
//	② 定时器是"隐式行为"：藏在包内部的话，读代码的人无法回答"这东西多久
//	   清一次"、"进程退出时它在干什么"。放在这里一眼可见。
//
// limiter 为 nil 时只清题（两者都可选，但调用方只需记住"接线时两个都传"）。
func startAuthLimitPruner(ctx context.Context, limiter *authlimit.Limiter, captcha *authlimit.Captcha) {
	if limiter == nil && captcha == nil {
		return
	}
	go func() {
		t := time.NewTicker(pruneInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if limiter != nil {
					if n := limiter.Prune(now); n > 0 {
						log.Printf("风控: 清理 %d 个过期的限速键", n)
					}
				}
				if captcha != nil {
					if n := captcha.Cleanup(now); n > 0 {
						log.Printf("风控: 回收 %d 道过期的人机校验题", n)
					}
				}
			}
		}
	}()
}
