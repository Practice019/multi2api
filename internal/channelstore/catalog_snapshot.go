// catalog_snapshot.go 目录快照的**持有者** —— 把"获取"与"读取"分开（U-04）。
//
// # 为什么必须分开
//
// 改造前 `/v1/models` 每次请求都现算一遍渠道库存。后果不是"慢"，是**不一致**：
//
//	· 一次请求里目录可能读到半更新的配置（渠道正在被增删）
//	· 上游拉取失败时，用户看到的仍是库存旧值，却**没有任何标记**说明它是旧的
//	· "刷新了 3 次"与"你现在看到的这份"之间没有绑定关系 —— 运维无法判断
//	  眼前这份是第几次的结果
//
// 所以分两件事：
//
//	获取（fetch）  可能失败。失败**不改**当前快照，只记 Attempts 与 LastError。
//	读取（read）   永不失败。永远返回上一份成功的快照（可能一份都没有）。
//
// # 一条硬不变量
//
//	Sequence 只在**发布成功**时前进；Attempts **每次尝试**都前进。
//
// 于是 `Attempts - Sequence` 就是"累计失败次数"，而"你现在看到的这份"永远等于
// 第 Sequence 次成功发布的结果 —— 这就是「刷新计数明细同版本」。
//
// ⚠ 如果让失败也推进 Sequence（或在快照里塞一个"最新尝试时间"），
// 运维会看到"刷新成功 5 次"却拿着一份 3 分钟前的旧目录且无从发现。
package channelstore

import (
	"sync"
	"time"
)

// CatalogSnapshot 一次已发布的目录快照及其刷新元数据。
//
// ⚠ Catalog.Version 与 Sequence 是**两把尺子**，不要混用：
//
//	Version   内容指纹：配置没变时连续两次发布**版本相同** —— 用来判"要不要通知客户端"
//	Sequence  发布序号：每次成功发布都 +1（哪怕内容一样）—— 用来判"这份是第几次的"
type CatalogSnapshot struct {
	Catalog     UnifiedCatalog `json:"catalog"`
	Sequence    int64          `json:"sequence"`
	Attempts    int64          `json:"attempts"`
	PublishedAt time.Time      `json:"published_at"`
	// LastError 最近一次失败的原文（成功后清空）。空 = 最近一次尝试是成功的。
	LastError string `json:"last_error,omitempty"`
}

// Ready 是否已有可用的快照（发布过至少一次）。
func (s CatalogSnapshot) Ready() bool { return s.Sequence > 0 }

// FailedAttempts 累计失败次数 = 尝试 − 成功。
func (s CatalogSnapshot) FailedAttempts() int64 {
	if s.Attempts <= s.Sequence {
		return 0
	}
	return s.Attempts - s.Sequence
}

// Stale 当前快照的年龄。未发布过时返回 0（没有可比的时间基准）。
func (s CatalogSnapshot) Stale(now time.Time) time.Duration {
	if !s.Ready() || s.PublishedAt.IsZero() {
		return 0
	}
	if now.Before(s.PublishedAt) {
		return 0
	}
	return now.Sub(s.PublishedAt)
}

// CatalogSnapshotStore 快照持有者。零值可用（首次读取得到空快照）。
//
// 并发约定：`Publish` / `Fail` 用写锁，`Read` 用读锁；读路径不阻塞读路径。
type CatalogSnapshotStore struct {
	mu  sync.RWMutex
	cur CatalogSnapshot
	now func() time.Time // 测试注入；nil 用 time.Now
}

// NewCatalogSnapshotStore 建一个空持有者。
func NewCatalogSnapshotStore() *CatalogSnapshotStore { return &CatalogSnapshotStore{} }

func (s *CatalogSnapshotStore) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Publish 发布一份新快照。
//
// 无论内容是否变化都推进 Sequence（发布这个动作本身发生了）；
// 内容是否真的变了由调用方比较 Catalog.Version 判断。
func (s *CatalogSnapshotStore) Publish(c UnifiedCatalog) CatalogSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.Sequence++
	s.cur.Attempts++
	s.cur.Catalog = c
	s.cur.PublishedAt = s.clock()
	s.cur.LastError = ""
	return s.cur
}

// Fail 记录一次失败。
//
// ⚠ 这里**只**动 Attempts 与 LastError：Catalog / Sequence / PublishedAt 全部保持——
// 一次上游抖动不该让客户端看到一个空目录。"保留旧快照"就是这两行的全部意思。
func (s *CatalogSnapshotStore) Fail(err error) CatalogSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.Attempts++
	if err != nil {
		s.cur.LastError = err.Error()
	} else {
		// 没有错误对象也要留下痕迹：调用方报了失败就必须能看出失败过。
		s.cur.LastError = "refresh failed"
	}
	return s.cur
}

// Read 读当前快照。永不失败 —— 一份都没发布过时返回 Sequence=0 的空快照。
func (s *CatalogSnapshotStore) Read() CatalogSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}
