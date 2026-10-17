# P2 设计：缓存聚合产物（把 `/admin/stats` 的 3 秒首屏吃掉）

> ⚠ 状态：**设计稿，尚未实现**。写它的目的是 R1/R2 复核期间不空转，
> 以及让实现时能一次做对（这是**核心接口**，改错的代价是全页崩）。

---

## 1 · 问题与靶子（实测，不是估计）

```
/admin/stats 三次冷启动：4634ms · 3078ms · 3507ms     ← ⚠ 波动大，当时有代理在跑（见 SUMMARY §6）
热态：            268ms · 248ms · 163ms · 231ms
```

⚠ **两段慢，成因不同**：

| 段 | 成因 | 当前状态 |
|---|---|---|
| 冷启动头几秒 | `sink.LoadAll()` 读 28000 行 + 首次解析（**冷 IO**） | ✅ **已用"启动预热"缓解**（`admin.New` 末尾先跑一遍 `statsItems()`） |
| ⚠ **剩下的 ~3 秒** | ⚠ **`aggregateChatLog` 逐行遍历 28000 条**（纯计算） | ❌ **未治** —— 本设计治它 |

⚠ **"预热聚合"已实测无效**（3078ms vs 3156ms，差异属噪声）
⇒ 那 3 秒**不是**首次分配/GC，就是**每次都要重算**
⇒ ⚠ **只能真的把产物缓存起来**。

---

## 2 · 为什么现在不能直接缓存（核心障碍）

```go
// internal/admin/admin.go
2418:  resp := aggregateChatLog(items)      // ← 纯聚合产物（map[string]any）
2419:  resp["source"] = source              // ↓ 之后十几处**就地改这个 map**
2420:  resp["capacity"] = h.cfg.Ring.Cap()
2421:  resp["held"] = h.cfg.Ring.Len()
2428:  resp["aggregated"] = len(items)
2431:  resp["error"] = …
2434:  resp["file"] = f
2437:  resp["truncated"] = true
2460:  resp["model_multipliers"] = h.modelMultipliers(resp)   // ⚠ 还**读** resp["by_model"]
2461:  resp["model_catalog"] = …
2513:  byModel, ok := resp["by_model"].(map[string]int)       // 只读
```

⇒ ⚠ **若把 `aggregateChatLog` 的返回 map 直接存进缓存并在下次返回同一个实例**，
第二个请求会看到一个已经被第一个请求**塞满派生字段**的 map ⇒ **污染**。

---

## 3 · ⭐ 关键简化：**只需要一层浅拷贝**

⚠ 我原本以为要深拷贝（因为 `by_model` / `by_uid_detail` 是嵌套 map）。**但不用**：

> **观察**：那些就地写入的字段，**全部是顶层新键**（`source` / `capacity` / `held` /
> `aggregated` / `error` / `file` / `truncated` / `model_multipliers` / `model_catalog` …）；
> ⚠ 而聚合出来的那些**嵌套结构（`by_model` / `by_uid` / `by_provider` / `by_*_detail`）
> 在这条路径上只被读、不被改**（2513 那处是 `.(map[string]int)` 取值读它）。

⇒ ✅ **所以"缓存产物 + 每次返回一层浅拷贝"就够了**：
```go
cached := h.statsAggCached()            // 缓存里的那一份（只读）
resp := make(map[string]any, len(cached)+16)
for k, v := range cached { resp[k] = v }  // ⚠ 浅拷贝：嵌套 map 共享，但它们只读
// 之后照旧 resp["source"] = … / resp["model_multipliers"] = … —— 写的都是顶层新键
```
⚠ 浅拷贝 28000 行聚合出的表（键数不多：几个 `by_*` + 十几个标量）⇒ **微秒级** ✓

### ⚠⚠ 这个简化有前提，必须写死在注释里

> **前提**：`aggregateChatLog` 的产物里，**嵌套值（所有 `by_*` / `by_*_detail`）只读不写**。
> 一旦将来有人在这条路径上**改动/删除嵌套 map 的内容**
>（例如 `delete(resp["by_model"], k)`、或往某个 `by_*` 里塞东西），
> **浅拷贝就会变成真污染**，届时必须升级成深拷贝。

⇒ 实现时要在**缓存函数**和 **`stats()` 就地写入那一串**两处都留这条注释，并加一个
**回归测试**（见 §5 第 3 条）。

---

## 4 · 缓存层设计（照本项目既有范式）

⚠ 项目里已有两套同型的：`statsItems()`（缓存原始行，TTL 5s）与
`multTableCached()`（内存表 + **过期时丢后台重建**，不在请求里重建）。
⇒ ✅ 这里照 **`multTableCached`** 那套写：

```go
// 字段（加在 Handler 上，与 multTbl/multAt/multBuilding 并列）
aggMu      sync.Mutex
aggCache   map[string]any   // ⚠ 只读共享；返回给调用方前必须浅拷贝
aggAt      time.Time
aggCount   int              // ⚠ 建它时的 entries 条数 —— 见下"正确性"一条
aggErr     error
aggBuilding bool

// 取缓存（请求路径）：
//   ① 新鲜（TTL 内）且 aggCount == 本次 len(items) ⇒ 直接浅拷贝返回
//   ② ⚠ 条数变了（日志被追加/轮转）⇒ 即使没到 TTL 也要重建 —— 否则总数会停在旧值
//   ③ 需要重建 ⇒ 起后台 goroutine 建；本次**先把旧产物浅拷贝交出去**（有值就好过空）
//   ④ ⚠ 首次（缓存为 nil）⇒ **同步建一次**（与 multTableCached 的修法一致：
//      "返回上一个版本的真数据"可以接受，"返回空"不行）
```

⚠ **TTL 选值**：沿用 `statsCacheTTL`（5s）—— 与它上层的 `statsItems` 一致，
否则两层 TTL 会互相错位（外层新鲜而内层已换 ⇒ 条数不一致，见 ③）。

---

## 5 · 验收（缺一条不算做完）

1. ⚠ **数字完全不变**：同一份冻结日志下，改造前后 `aggregateChatLog` 的**每个字段**逐项相等
   （用 A2 的 `tools/verify_stats.py --file <冻结件> --json` + A1 的 `tools/verify_byuid_detail.py` 双重核对）。
2. ⚠ **热态提速有数字**：改造前后各测 5 次取**中位数**，⚠ **且必须在没有别的代理在跑时测**（账本 `E058`）。
3. ⚠ **污染回归测试**（最关键的一条）：对 `GET /admin/stats` 打**两次**，
   断言① 两次的 `total` 相等且**等于日志行数**；② 第一次响应里**不应**出现第二次才该有的键
   （反向也断言），③ 直接调 `stats()` 之后缓存的内部 map **键数没有增长**。
   ⇒ ⚠ **这条测试是"浅拷贝够用"这个前提的守门人** —— 没有它，将来一次疏忽就会静默污染。
4. ⚠ **条数变化要生效**：往日志追加一行后立刻请求，`total` 必须 +1（不能停在缓存里的旧值）。
5. ⚠ **首次不为空**：进程刚起、缓存为 nil 时请求，返回的仍是**完整**产物（不是空 map）。

---

## 6 · 风险与回退

* ⚠ **风险面**：`/admin/stats` 是**全站最热**的接口（仪表盘 + 卡片 + 榜/环 + 30s 轮询）。
  ⇒ 改坏了不是"报表区不显示"，而是**整页崩**。
* ✅ **回退**：改动集中在 `stats()` 与新增的缓存函数两处；
  把 `aggCache` 的读写换成"直接调 `aggregateChatLog(items)`"即回到今天的行为 ——
  ⚠ 实现时**单独一个 commit**（便于 `git revert`）。
* ⚠ **不要顺手做别的**：这次只治"重算"，**不动** TTL 语义、不动 `statsItems`、不动字段名。

---

## 7 · ⚠ 我还没验的

* 3 秒到底花在 `aggregateChatLog` 的哪一步（没有 CPU profile ⇒ 只是从"预热无效"反推它是纯计算）。
  ⇒ 若要更精确，实现前可先跑一次 `go test -bench` 或 pprof，⚠ **但那需要在干净环境下**。
* 缓存是否会让**并发请求**看到不一致的两层（`statsItems` 与 `aggCache` 各自 TTL）。
  ⇒ §4 的 `aggCount` 判据是为了挡这个，但**尚未实测**。
* ⚠ 本设计**未经 R1/R2 复核**（它们还在跑，且各自的题目不同）。
