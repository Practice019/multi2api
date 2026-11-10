// sink_size_test.go 钉住「体积上限按**真实字节数**算」这条契约。
//
// # 为什么需要它（2026-11-10 实测）
//
// 旧 `approxSize` 只算 `len(Model)+len(UID)+len(Mode)+96`，**完全没算后加的字段**：
// `upstream_err`（上限 4 KiB）、`upstream_req`（上限 8 KiB）、`decision`、`client_ua`…
// 后果：上限写的是 8 MiB，磁盘上的文件却长到 **22 MB（2.7 倍）**，
// 而 `s.size` 自报 8.28 MB —— 差了 14 MB。于是"按体积裁剪"这条纪律实际没生效。
//
// 本测试用**带大字段**的条目（正是真实失败请求的形态）验证裁剪真的按真实体积发生。
package logbuf

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestEntrySizeCountsLargeFields 真实字节数必须把大字段算进去。
func TestEntrySizeCountsLargeFields(t *testing.T) {
	small := Entry{At: time.Now(), Model: "m", UID: "u", Mode: "stream"}
	huge := small
	huge.UpstreamErr = strings.Repeat("e", 4096)
	huge.UpstreamReq = strings.Repeat("r", 8192)
	huge.Decision = strings.Repeat("d", 500)

	ds := entrySize(small)
	dh := entrySize(huge)
	// 大字段合计 12788 字节 ⇒ 差值必须接近它（允许 JSON 转义等少量偏差）。
	if dh-ds < 12000 {
		t.Errorf("大字段没被计入：small=%d huge=%d 差=%d（应 ≥12000）", ds, dh, dh-ds)
	}
}

// TestSinkPruneRespectsRealByteCap 裁剪必须让文件**真的**落在上限内。
//
// 构造：每条带 8 KiB 的 upstream_req（真实失败请求的形态），
// 写满若干条后触发裁剪 —— 旧实现在这里会留下远超上限的文件。
func TestSinkPruneRespectsRealByteCap(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/log.jsonl"
	const cap = 64 << 10 // 64 KiB，便于用小数据触发
	s, err := OpenSink(path, 7, cap)
	if err != nil {
		t.Fatalf("OpenSink: %v", err)
	}
	defer func() { _ = s.Close() }()

	body := strings.Repeat("x", 8192)
	for i := 0; i < 40; i++ { // 40 × ~8 KiB ≈ 320 KiB ⇒ 必然触发裁剪
		s.Append(Entry{
			At:          time.Now(),
			Model:       "m",
			UID:         "u",
			Mode:        "stream",
			Status:      502,
			UpstreamReq: body,
		})
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// 允许一次写入的余量（Append 是"写完再裁"，不会恰好等于上限）。
	if fi.Size() > cap+16<<10 {
		t.Errorf("★ 裁剪没按真实字节数生效：文件 %d 字节，上限 %d（超出 %d）",
			fi.Size(), cap, fi.Size()-cap)
	}
}
