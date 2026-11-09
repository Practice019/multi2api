// shortuid.go gateway 侧只剩**转发**（T-403）。
//
// # 为什么实现不在这里
//
// 最初把实现放在 gateway（所有上游都 import 它，看起来最自然），但架构测试当场抓住两点：
//
//  1. `internal/dailycheckin` 有显式规则 `TestNoGatewayImport`：那个包只需要"把 uid 缩短"，
//     不该为一个日志格式化函数依赖**契约层**；
//  2. 架构测试用"是否 import gateway"自动发现上游 ⇒ 给 dailycheckin 加上 gateway 后
//     它被判成上游，于是 `TestUpstreamsDoNotDependOnCore` 连报三条
//     "上游 X 依赖了另一个上游 dailycheckin"。
//
// ⇒ 真实现搬去了 `internal/logbuf.ShortUID`（日志传输层，且是架构测试的豁免包）。
// 这里保留 `gateway.ShortUID` 只是**转发**：7 个上游包的调用点（`var shortUID = gateway.ShortUID`）
// 因此一行都不用改，而"实现只有一处"这个真正的要求仍然满足。
package gateway

import "workbuddy2api/internal/logbuf"

// ShortUID 转发到 logbuf.ShortUID（唯一实现）。
//
// ⚠ 不要在这里加逻辑 —— 两份实现正是 T-403 要消灭的东西。
func ShortUID(uid string) string { return logbuf.ShortUID(uid) }
