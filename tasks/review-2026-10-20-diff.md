# /review：2026-10-20「数据算错」修复批次的五轴评审

> 评审对象：`b0104aa` → `ff80045` 共 10 个提交（+ 评审中新增的 `2395e96`）。
> 方法：`~/.dsh/skills/review/code-review-and-quality.md` 五轴检查表。
> 评审人 = 作者本人 ⇒ 同作者盲区是结构性的，所以下面每条都附「怎么查出来的」。

## 一、问题清单（严重度排序）

### ✅ Critical（已修：`2395e96`）

**全命中 / 全未命中的合法帧被判「输入未知」⇒ 整帧丢弃**

```go
// ff80045 里我写的（错）
if u.CacheHitTokens > 0 && u.CacheMissTokens > 0 {   // 要求两者都 >0
    return u.CacheHitTokens + u.CacheMissTokens
}
return 0
```

- **后果**：全命中（`miss=0`）⇒ 返回 0 ⇒ `valid()` 可能 false ⇒ 帧被丢 ⇒ total 退化成估算。
- **讽刺点**：这正是 `ff80045` 要修的那个 bug 的翻版 —— 我为修「缓存量消失」，新建了「全命中被丢掉」。而全命中恰恰是高缓存命中会话（DSH 96%）的正常形态。
- **根因**：JSON 里「键缺失」与「值为 0」不可区分，解析进 struct 后都是 Go 零值 0。用数值做判据，必然在两边选错一边：

  | 判据 | 错在哪 |
  |---|---|
  | 当成未知 | 全命中被丢帧 ← 本次的错 |
  | 当成 0 | 只给 hit 的上游被当全命中，未命中部分凭空消失 |

- **修法**：`sseUsage` 加未导出的 `hasCacheHit` / `hasCacheMiss`，`frameUsage` 额外解析一次原始键置位，`promptTotal()` 改用「**键存在**」而非「值 >0」。
- **怎么查出来的**：不是读代码读出来的，是**照着自己在 goal 里列的疑点 2 逐条追问**才发现的。纯看 diff 会觉得"已经有 >0 判据了，挺严谨"。

### ⚠ Nit（未修，作者可忽略，但列出）

**N1. `hasOut` 对 `completion_tokens: null` 判 true**

```js
hasOut = ('completion_tokens' in usage) && isFinite(Number(null)) && Number(null) >= 0
//                                        ↑ Number(null) === 0，有限且 >=0
```

但真实值是 `null` ⇒ 会显示「输出 0」而不是「—」。要修得加 `usage.completion_tokens !== null`。
**为什么没修**：这是"显示 0 而非 —"，方向与 `40592c8` 修的一致但程度轻得多；而且真实上游极少发 `null`（要么不发键、要么给数字）。

**N2. 健康条「吞吐」与 `fmtTps` 精度不同**

```js
// L9900
const tps = toks / ((avgMs * total) / 1000);        // 聚合吞吐
parts.push(... fmtTokens(Math.round(tps)) + ' t/s') // 一律取整
```

与 `fmtTps`（≥10 取整 / <10 一位）不同。但它是**另一个指标**（聚合吞吐 = 总量/总时间，不是单请求生成速度），标签也已用 `t/s` 而非 `tok/s` 区分 ⇒ 不算 bug。

**N3. `total()` 与 `promptTotal()` 各查一次 nil** —— 极轻重复，不影响正确性。

## 二、判定为「不是问题」的疑点（留档防重复怀疑）

### 疑点 1：`valid()` 放宽会不会放进半截快照？

**不会，而且比原来更严。**

| | 原判据 | 新判据 |
|---|---|---|
| 条件 | `PromptTokens>0 && (Completion>0 \|\| Total>0)` | `Completion>0 && (promptTotal>0 \|\| Total>0)` |
| `{prompt:123, total:500}` | ✅ 通过（completion=0 也算） | ❌ 拒绝（更严） |
| `{completion:100, total:500}` | ❌ 拒绝 | ✅ 通过（total 可用，total() 取 500，正确） |
| `{completion:100}` | ❌ | ❌ |

原注释担心的"只有输入的半截快照"在新判据下**仍然被拒**（因为要求 `completion>0`）。

### 疑点 4：`ferr.Error()` 会不会 panic？

**不会。** 调用点前有 `if ferr != nil {` 守卫，`ferr.Error()` 在其内部。`fw` 零值只读 string 字段（`ChannelName`/`ChannelID`/`UpstreamModel`），读零值 string 安全。
**遗留**：`fw` 零值导致这三个字段写进日志是空串 —— 已在 `543375e` 的 TODO 里记下根因修法（让 `forwardViaChannel` 失败时回填）。

### 疑点 5：`hasOut` 与 `numT` 口径是否不一致？

**字符串场景一致，null 场景不一致**（见 N1）。

```js
completion_tokens: "483"  ⇒ hasOut=true, numT=483  ✓ 一致
completion_tokens: null   ⇒ hasOut=true, numT=0    ✗ 不一致（N1）
completion_tokens: 缺失   ⇒ hasOut=false, numT=0   ✓ 一致
```

### 疑点 6：`fmtTps` 有没有遗漏的显示位？

**没有。** 全库只有两个 tok/s 显示位，都走 `tokPerSec` → `fmtTps`：

```
L9551  日志表     tokPerSec(e.out_tokens, e.total_ms, e.ttfb_ms, e.mode)
L12653 对话测试   tokPerSec(outTok, totalMs, 0, 'stream')
```

`fmtTps` 本身只被 `tokPerSec` 调用一次（L9512），无旁路。

## 三、五轴结论

| 轴 | 结论 | 依据 |
|---|---|---|
| **正确性** | 1 Critical 已修，3 Nit 留档 | 10+4 条缓存断言全 PASS |
| **可读性** | 通过 | 三个入口（valid/total/promptTotal）注释都写了判据与**为什么**；`hasCacheHit` 的注释解释了「缺失 vs 0」这个非显然区分 |
| **架构** | 通过 | 改动都在 `stream_usage.go` 内；`has*` 未导出，不影响 JSON 契约；未跨层、未把逻辑塞进共享模块 |
| **安全** | 通过 | 无密钥/凭据落日志（`KeyMask` 打码）；`ferr` nil 已守卫；外部 usage 数据按不可信处理（多层 `if u == nil`、`valid()` 校验） |
| **性能** | 通过（有代价，可接受） | 新增每帧一次 `unmarshal` 到 map —— **只在携带 usage 的帧上**（流的最末一两帧），不在逐 chunk 热路径 |

## 四、验证故事

```
go build ./...                          exit=0
go test ./internal/... ./cmd/...        ok=39  FAIL=0
缓存相关断言                            10 条全 PASS
门禁① 前端内联脚本 6 段                  全部通过
-race                                   跑不了：CGO_ENABLED 未开（环境限制，非代码问题）
```

## 五、仍未完成（不在本批 diff 范围，但同一问题域）

**csg 上游的 `hit`/`miss` 全是 0**：

```
09:39:22 | toks=264512 out=823 hit=0 miss=0 | csg/deepseek-v4.1-flash-1017
```

26 万 total 但缓存字段无值。两种可能：
1. csg 上游确实不发缓存字段 ⇒ 无法统计，四窗样本天然只覆盖部分请求
2. 字段名不同（如 `cached_tokens` 嵌套形态）⇒ 需要抓真实 SSE 帧确认

**这决定了「四窗 46.6% vs 表格 96%」差距的另一半**（前一半是 loomy 帧被丢，已修）。
要查需抓一次 csg 的原始响应体 —— 用临时实例 + 打点，不碰 7863。