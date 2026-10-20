# 面板字段与 new-api 的逐项对照（2026-10-20）

> 源码：`D:\temp\new-api-fresh`（HEAD 32e5874）
> 目的：用户要求「对照一下我们面板有哪些字段和他们对应，显示也是同样的，然后直接用它的计算方式代替我们的」。

## 一、new-api 的卡片清单与算法（`use-dashboard-config.tsx`）

| key | 卡片标题 | getValue（算法） |
|---|---|---|
| `count` | Total Count | `stat?.rpm ?? 0` ← 命名怪，实际是**区间总请求数** |
| `quota` | Total Quota | `stat?.quota ?? 0` ← **费用** |
| `tokens` | **Total Tokens** | `stat?.tpm ?? 0` ← 命名怪，实际是**区间总 token** |
| `avgRpm` | Average RPM | `safeDivide(stat?.rpm, timeRangeMinutes)` |
| `avgTpm` | Average TPM | `safeDivide(stat?.tpm, timeRangeMinutes)` |
| `todayUsage` | Last 24h usage | `totals.todayUsageDisplay` ← **费用** |
| `usage` | Historical Usage | `totals.usedDisplay` ← **费用** |
| `requests` | Request Count | `totals.requestCountDisplay` |

> ⚠ **命名陷阱**：`stat.tpm` 在 `Total Tokens` 卡里当"总量"用，在 `Average TPM` 里又要除以分钟数。
> 它其实存的是**区间总 token**，字段名带 tpm 是历史遗留。

`safeDivide`（`lib/stats.ts:24`）= 除零/NaN/Infinity 返回 0，再按 precision 四舍五入。

## 二、它的 `token_used` 是怎么来的（关键）

`model/log.go:395`：

```go
TokenUsed: params.PromptTokens + params.CompletionTokens,
```

**后端落库时自己加，从不信任上游的 `total_tokens`。**

`lib/stats.ts:43` 再把它累加：

```js
totalTokens: acc.totalTokens + (Number(item.token_used) || 0)
```

## 三、逐项对照

### ✅ 完全对应，算法已一致

| new-api | 我们 | 算法对照 |
|---|---|---|
| Total Count | **总请求** | `Σ 行数` ↔ `s.total` ✓ |
| **Total Tokens** | **总处理量** | `Σ(prompt+completion)` ↔ `s.tokens`（`f46cdcd` 已改成自己加）✓ |
| Average RPM | **平均 RPM** | `请求数 / 窗口分钟数` ↔ `perMin(s.total)` ✓ |
| Average TPM | **平均 TPM** | `token / 窗口分钟数` ↔ `perMin(s.tokens)` ✓（自动受益于上面那个修复） |
| （无） | 成功 / 失败 / 成功率 | 我们多给的，它靠 `count` 一个数表示 |
| （无） | 输出 token | 我们有，它没有（它不分输入/输出） |
| （无） | 推理 token | 我们有，它没有 |
| （无） | 平均 TTFB / 平均总耗时 | 我们有，它的 `PerformanceHealthPanel` 里用不同方式呈现 |
| （无） | Top 模型 / 活跃天数 / 连续天数 | 我们有 |

### ❌ 不需要对应（我们无计费）

| new-api | 为什么不要 |
|---|---|
| Total Quota | 费用，我们是单用户自用网关，不计费 |
| Last 24h usage | 同上 |
| Historical Usage | 同上 |

## 四、这次改动的实质

**核心只有一处：`total()` 的算法。**

```go
// 改前（错）
if u.TotalTokens > 0 { return u.TotalTokens }   // 信上游
return u.promptTotal() + u.CompletionTokens

// 改后（照 new-api）
if pt := u.promptTotal(); pt > 0 { return pt + u.CompletionTokens }  // 自己加
if u.TotalTokens > 0 { return u.TotalTokens }                        // 输入无从得知才回退
return u.CompletionTokens
```

**为什么这是错的根源**：上游的 `total_tokens` 会漏算缓存。

实测 loomy 一条真实请求：

```
prompt_cache_hit_tokens  = 63,360
prompt_cache_miss_tokens =    722
completion_tokens        =    483
────────────────────────────────
真实处理量                = 64,565

total_tokens             =    483   ← 只报了输出
```

信 `total_tokens` 这条记 483，自己加记 64,565 —— **差 133 倍**。

全库累积：Σ tokens（旧）= 1.84 亿，而 new-api 面板 7.84 亿，差 4.3 倍。

## 五、⚠ 历史数据不会回填

那 35,697 条已按旧算法落盘（`tokens` 字段里存的就是错的数）。改的是**新请求**。

⇒ 重启后：
- 新请求的 token 会变几十倍（正常）
- 指标卡累计值从 1.84 亿开始往上走，**不会跳到 7.84 亿**
- 想修历史需要回填脚本（能重算的只有日志里有缓存字段的行，实测覆盖率 83%）

## 六、判定为「不需要改」的

| 项 | 理由 |
|---|---|
| 费用类三张卡 | 我们无计费 |
| `stat.tpm` 那个奇怪命名 | 是它的历史遗留，我们的 `s.tokens` 语义更清楚，不需要模仿命名 |
| `safeDivide` 的 precision 参数 | 我们 `winMin > 0` 已判过，等价保护 |
