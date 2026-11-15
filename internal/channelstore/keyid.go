// keyid.go Key 的**持久稳定资源 ID**（U-06 剩余段）。
//
// # 这个文件要修的是什么
//
// 轮询游标原先存的是**数组下标**：
//
//	s.rr[ch.ID] = pick + 1      // pick 是 ch.Keys 里的下标
//
// 于是删掉第 0 把 key 之后，所有下标左移一位 —— 游标 1 指的是**另一把 key**。
// 表现是轮询跳过一把明明可用的 key，而日志里看不出任何异常
// （每次请求都成功，只是分布歪了）。新增/插入/拖拽排序同理。
//
// # 修法：游标存 ID，不存下标
//
//	顺序    仍然是**声明顺序** —— 运维在界面上排的顺序就是轮转顺序，这条不变
//	游标    存上一次选中的 Key 的稳定 ID
//	下一个  在"可用 key 的声明序列"里找游标 ID 的**后继**，找不到就回到第一个
//
// 这样删除任何一把 key 都只影响它自己：其余 key 的相对次序与游标都不动。
//
// # 为什么 ID 是派生出来的、而且 json:"-"
//
// ① **不落盘**：ID 由 Key 的值派生，写进配置文件等于把密钥指纹存了一份 ——
//
//	渠道列表本来就要掩码，没必要顺手泄漏一个可比对的短哈希。
//
// ② **不按位置派生**：若用"第几把"派生，删除后 ID 全体变化，缺陷原样复现。
// ③ **确定性**：同一把 key 在任何进程、任何重启后得到同一个 ID，
//
//	于是"重启从首项开始"这条既有语义（计划 F-04）仍然成立，不需要持久化游标。
package channelstore

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// keyIDSalt 派生 ID 的域分隔串。换它等于换一套 ID（游标会重置一次），
// 所以带版本号：v1。
const keyIDSalt = "wb2a/key-id/v1\x00"

// derivedKeyID 由 Key 的值派生一个稳定 ID。
//
// 取 sha256 前 10 个 hex（40 bit）。这里**不是**安全边界 ——
// 它的唯一用途是"同一把 key 在删除别人之后仍然认得出自己"。
// 40 bit 对"同一渠道内十几把 key"的碰撞概率可以忽略；
// 万一真撞了还有 keyIDsOf 的重复消除兜底。
func derivedKeyID(value string) string {
	sum := sha256.Sum256([]byte(keyIDSalt + value))
	return "k" + hex.EncodeToString(sum[:5])
}

// keyIDsOf 返回与 keys 一一对应的稳定 ID。
//
// ⚠ 重复消除只在**同一渠道内两把 key 的值完全相同**时触发（配置错误）。
// 那时给后出现的一把加 `-2` 后缀 —— 这是唯一引入位置因素的地方，
// 但重复 key 本身就没有"稳定的语义"，两害取其轻。
func keyIDsOf(keys []ChannelKey) []string {
	ids := make([]string, len(keys))
	used := make(map[string]bool, len(keys))
	for i, k := range keys {
		base := k.ID
		if base == "" {
			base = derivedKeyID(k.Key)
		}
		id := base
		for n := 2; used[id]; n++ {
			id = base + "-" + strconv.Itoa(n)
		}
		used[id] = true
		ids[i] = id
	}
	return ids
}

// indexOfID 在"可用下标序列"里找 want 这个 ID 的位置；找不到返回 -1。
//
// usable 是按声明顺序筛出来的下标列表，ids 与 keys 一一对应 ——
// 所以我们要的是"usable 里第几个元素的 ID 等于 want"。
func indexOfID(ids []string, usable []int, want string) int {
	if want == "" {
		return -1
	}
	for position, index := range usable {
		if index >= 0 && index < len(ids) && ids[index] == want {
			return position
		}
	}
	return -1
}
