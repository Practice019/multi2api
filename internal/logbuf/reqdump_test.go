// reqdump_test.go 失败请求体落盘器的回归。
package logbuf

import (
	"os"
	"sync"
	"testing"
)

// TestReqDumpNoCollisionOnConcurrentWrites 同一毫秒的并发失败**不能互相覆盖**。
//
// 第一版文件名只用「时间戳（毫秒）+ 模型哈希」，而哈希对同模型的失败是**恒定**的
// ⇒ 同一毫秒里两条同模型的失败会写到同一个文件，后者覆盖前者。
// 并发失败在真实场景里很常见（DSH 一次发多个请求），那会让"最后一次失败"的证据丢掉。
func TestReqDumpNoCollisionOnConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	d, err := NewReqDump(dir, 0, 0)
	if err != nil {
		t.Fatalf("NewReqDump: %v", err)
	}
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.Write("same/model", "same-provider", "502", map[string]string{"i": itoa(i)}, []byte(`{"x":1}`))
		}(i)
	}
	wg.Wait()
	ents, _ := os.ReadDir(dir)
	if len(ents) != n {
		t.Errorf("并发写 %d 次应产生 %d 个文件（同名会被覆盖），实际 %d 个", n, n, len(ents))
	}
}
