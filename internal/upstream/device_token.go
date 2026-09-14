// device_token.go 设备风控令牌（X-Device-Token）的文件来源与缓存。
//
// # 背景（借鉴 workbuddy2api-panel）
//
// 上游对聊天请求做设备级风控，识别依据之一是 `X-Device-Token` 头。
// 官方客户端从本地凭证库里读这个值；自建网关没有那个库，
// 所以本包支持三个来源，优先级从高到低：
//
//	① 每号凭证里的 DeviceToken（auth.Auth.DeviceToken）
//	② 全局配置 upstream.device_token
//	③ 文件 upstream.device_token_file
//
// 三者都为空 → **不注入该头**（优雅降级：少一个头的请求上游照样受理，
// 只是风控画像不完整）。
//
// # 为什么文件来源要单独做缓存
//
// 文件是**外部可变的**：用户可能让官方客户端在跑，它会周期性改写
// 那个凭证文件。每请求读一次文件会有两个问题：
//
//	① 高频 IO：聊天请求可能在几十 QPS，每次一个 open+read 是纯浪费
//	② 读到半写状态：官方客户端写文件不是原子的，
//	   恰好在写入窗口读到会被截断的 JSON —— 用一个坏值比用旧值更糟
//
// 因此加一层 TTL 缓存（5 分钟）：
//
//	读失败/读到空 → **保留上一次的有效值**（绝不因为一次坏读就清空）
//	缓存过期 → 尝试重读，成功则更新
//
// 与之配套的两个上限：
//
//	deviceTokenFileMaxLen 防止用户把 device_token_file 配到一个巨大文件上
//	                      （那会让每次读都把整个文件灌进内存）
//	deviceTokenFileTTL    决定官方客户端换 token 后我们最多滞后多久
package upstream

import (
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// deviceTokenFileTTL 文件来源的缓存时长。
	//
	// 5 分钟是一个折中：比官方客户端的 token 轮换周期短得多（它按小时级），
	// 又足以把"每请求一次 IO"降到"每 5 分钟一次"。
	deviceTokenFileTTL = 5 * time.Minute
	// deviceTokenFileMaxLen 单个设备令牌文件的最大读取字节数。
	//
	// 正常值是几十字符。给 1 KiB 的余量：超过这个长度说明配错了文件
	// （例如指到了 config.json 或日志），直接判失败而不是把它塞进请求头。
	deviceTokenFileMaxLen = 1024
)

// deviceTokenFileCache 带 TTL 的令牌文件缓存。
//
// # 为什么读失败时保留旧值
//
// 见文件头注释的"读到半写状态"：官方客户端非原子写文件，
// 我们可能恰好在写入窗口读到截断内容。此时**用旧值**是正确的 ——
// 旧值至少是曾经被上游接受过的真实令牌，
// 而清空会让后续请求全部失去风控头（一个静默的能力退化）。
type deviceTokenFileCache struct {
	mu      sync.Mutex
	path    string
	value   string
	fetched time.Time
}

var dtFileCache = &deviceTokenFileCache{}

// readDeviceTokenFile 读取设备令牌文件（带 TTL 缓存与失败保留）。
//
// 返回空串表示"没有可用的令牌" —— 调用方据此决定不注入该头。
// 本函数**不返回 error**：设备令牌缺失是合法状态（未配置就没人用它），
// 把它做成 error 会逼每个调用方写一段"忽略这个错误"的样板。
func readDeviceTokenFile(path string) string {
	if path == "" {
		return ""
	}
	c := dtFileCache
	c.mu.Lock()
	defer c.mu.Unlock()

	// 路径变了 → 缓存失效（用户在线改了配置）。
	if c.path == path && c.value != "" && time.Since(c.fetched) < deviceTokenFileTTL {
		return c.value
	}

	raw, err := readTrimmedFile(path, deviceTokenFileMaxLen)
	if err != nil || raw == "" {
		// 读失败/空内容 → 保留上次的值（可能为空串），但**不刷新时间戳**，
		// 这样下一次调用会立刻重试，而不是把一次瞬时失败缓存 5 分钟。
		c.path = path
		return c.value
	}
	c.path, c.value, c.fetched = path, raw, time.Now()
	return c.value
}

// readTrimmedFile 读取文件并 trim，超过 maxLen 字节直接判失败。
//
// 用 Stat 先看大小而不是"读进来再截断"：后者仍会把整个大文件读进内存，
// 而我们要防的正是这一点。
func readTrimmedFile(path string, maxLen int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if st, err := f.Stat(); err == nil && st.Size() > maxLen {
		return "", errDeviceTokenTooLarge
	}
	buf := make([]byte, maxLen+1)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", err
	}
	if int64(n) > maxLen {
		return "", errDeviceTokenTooLarge
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

// errDeviceTokenTooLarge 令牌文件超出上限（几乎必然是配置指错了文件）。
var errDeviceTokenTooLarge = errTooLarge{}

type errTooLarge struct{}

func (errTooLarge) Error() string { return "device token 文件超出大小上限" }

// resetDeviceTokenFileCache 清空缓存内容。仅用于测试（避免跨用例污染）。
//
// ⚠ 只清**字段**，绝不整体替换结构体 ——
// `*dtFileCache = deviceTokenFileCache{}` 会连同 mu 一起换掉，
// 而我们在持有旧 mu 的情况下解锁新 mu，直接触发
// `fatal error: sync: unlock of unlocked mutex`（进程级崩溃，recover 不住）。
// 这个坑在本文件第一次提交时就被测试抓到了。
func resetDeviceTokenFileCache() {
	c := dtFileCache
	c.mu.Lock()
	c.path, c.value, c.fetched = "", "", time.Time{}
	c.mu.Unlock()
}
