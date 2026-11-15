// compat_test.go U-10 本地数据版本兼容判据。
//
// # 这组判据唯一要钉住的事
//
// **盘上的数据比本二进制新时，必须拒绝启动。**
//
// 别的结论都能继续（只读或迁移），只有它不能 —— 因为旧代码不认识新字段，
// 而"不认识"会被静默当成"没设置"，那是数据损坏级的错。
package channelstore

import (
	"strings"
	"testing"
)

// ★ 盘上比二进制新 ⇒ 必须拒绝启动。
func TestCompatBlocksWhenDataIsNewer(t *testing.T) {
	c := CheckCompatibility(LocalSchemaVersion+1, false)

	if c.Verdict != CompatBinaryTooOld {
		t.Fatalf("盘上 v%d 比二进制 v%d 新，应判 binary_too_old，实际 %q",
			LocalSchemaVersion+1, LocalSchemaVersion, c.Verdict)
	}
	if !c.MustBlockStartup() {
		t.Fatal("★ 必须拒绝启动 —— 否则旧二进制会把不认识的新字段静默当成未设置")
	}
	if c.OK() {
		t.Fatal("binary_too_old 不该被认为 OK")
	}
	err := c.Error()
	if err == nil {
		t.Fatal("★ Error() 必须返回错误（main 里 log.Fatal 是最难被顺手绕过的形态）")
	}
	if !strings.Contains(err.Error(), "拒绝启动") {
		t.Fatalf("错误信息应说清是拒绝启动，实际 %q", err.Error())
	}
}

// 版本一致 ⇒ 正常。
func TestCompatOKWhenVersionsMatch(t *testing.T) {
	c := CheckCompatibility(LocalSchemaVersion, false)
	if c.Verdict != CompatOK || !c.OK() || c.MustBlockStartup() {
		t.Fatalf("版本一致应 OK：%+v", c)
	}
	if err := c.Error(); err != nil {
		t.Fatalf("OK 时不该有错误：%v", err)
	}
}

// 盘上更旧 ⇒ 需要迁移，但**不阻塞启动**（只读能继续）。
func TestCompatNeedsMigrationWhenDataIsOlder(t *testing.T) {
	c := CheckCompatibility(LocalSchemaVersion-1, false)
	if c.Verdict != CompatNeedMigration {
		t.Fatalf("应判 need_migration，实际 %q", c.Verdict)
	}
	if c.MustBlockStartup() {
		t.Fatal("数据更旧不该阻塞启动 —— 只读路径还要能用")
	}
	if c.Reason == "" {
		t.Fatal("必须给出原因（运维要知道该跑迁移）")
	}
}

// 没有版本标记 ⇒ 按"引入本机制之前的库"处理。
func TestCompatUnmarkedIsTreatedAsLegacy(t *testing.T) {
	for _, c := range []DataCompatibility{
		CheckCompatibility(0, false),
		CheckCompatibility(0, true),
		CheckCompatibility(LocalSchemaVersion, true), // unknown 优先
	} {
		if c.Verdict != CompatUnmarked {
			t.Fatalf("无标记应判 unmarked，实际 %q（onDisk=%d unknown=%v）",
				c.Verdict, c.OnDiskVersion, c.OnDiskVersion != LocalSchemaVersion)
		}
		if c.MustBlockStartup() {
			t.Fatal("无标记不该阻塞启动 —— 那会把所有存量部署挡在门外")
		}
		if !c.OK() {
			t.Fatal("无标记应可继续（打标后即可）")
		}
	}
}

// 负版本 / 异常值按"更旧"处理，不能当成"新"。
func TestCompatNegativeVersionIsNotNewer(t *testing.T) {
	c := CheckCompatibility(-1, false)
	if c.MustBlockStartup() {
		t.Fatalf("负数版本不该被当成「数据更新」：%+v", c)
	}
}

// 写入时该标的版本就是 LocalSchemaVersion。
func TestCompatNextVersionOnWrite(t *testing.T) {
	if got := NextVersionOnWrite(); got != LocalSchemaVersion {
		t.Fatalf("NextVersionOnWrite = %d，want %d", got, LocalSchemaVersion)
	}
}
