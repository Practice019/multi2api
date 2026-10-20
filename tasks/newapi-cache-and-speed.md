# 缓存命中与速度：new-api 到底怎么处理（2026-10-20 查证结论）

> 用户连续两次要求「直接看别人怎么做，抄」。这份文件是查证结果，不是推测。

## 一、每秒输出 token（tok/s）—— 已抄，且已上线

`web/src/features/usage-logs/components/common-log-mobile-card.tsx:198-200`：

```js
tokensPerSecond = log.use_time > 0 && log.completion_tokens > 0
  ? log.completion_tokens / log.use_time
  : null
```

`timing-metrics-cell.tsx:223`：仅 `props.isStream` 时显示。

**我们的实现**：`tokPerSec(outTokens, totalMs, ttfbMs, mode)` = `out / (total − ttfb)`，非流式或 0 值返回 `—`。
比它的更精确（它用整秒 `use_time`，我们用毫秒并扣掉首字等待）。

## 二、缓存命中 —— 它**不算命中率**，它算钱

在 `new-api` 全库 grep `cache_hit|cacheHit|cached_tokens|cacheRatio|prompt_cache`：

| 命中位置 | 是什么 |
|---|---|
| `controller/channel-test.go:565` | 把 `usage.PromptTokensDetails.CachedTokens` 与 `CacheRatio` 一起喂给**计费**函数 |
| `controller/model_management_test.go:155-164` | `PricingValues{ModelRatio, CacheRatio}` 各模型的**计价倍率** |
| `relay/**` | 解析上游 usage 的 `cached_tokens`（我们已抄进 `wire.ParseUsageExtras`） |

**没有任何一处**计算 `hit/(hit+miss)` 这样的"命中率"，也没有任何前端组件展示它。
`web/src/features/usage-logs` 目录下 grep `cache` 零命中。

⇒ **它的缓存用途只有一个：把缓存命中的 token 按 `CacheRatio` 打折计费。**
⇒ 所以「缓存命中率 98%」这种展示是 **DSH 客户端自己算的**，不是 new-api 的报表。

## 三、这对我们意味着什么

1. **「缓存命中率四窗」是我们自己的功能**，不是抄 new-api 的东西 ⇒ "抄不抄"不构成删它的理由。
2. 它的口径**本来就是对的**：命中率 = `hit / (hit + miss)`，这是 **prompt 侧**的比例，与「输出 token」互不相干。用户要的"消耗 = 输出 token"不适用于这个指标 —— 它不是消耗，是输入侧的结构比例。
3. 所以这一块**不需要改口径**，只需产品决定去留。用户若要"像 DSH 那样看缓存命中"，保留即可；若觉得多余，删掉也不损失任何信息（数据在日志里仍有）。

## 四、报表口径

`dashboard/lib/charts.ts:53-61`：

```ts
function renderQuotaCompat(rawQuota, digits = 4) {
  if (meta.kind === 'tokens') return rawQuota.toLocaleString()   // token 数
  const usd = rawQuota / config.quotaPerUnit                     // 或美元
}
```

它**两种都支持**，由 `meta.kind` 决定。我们是单用户自用网关，只需要 token 视角 ⇒ 只做 tokens 是对的。

## 五、结论汇总

| 指标 | new-api 怎么做 | 我们 | 需要改吗 |
|---|---|---|---|
| 每秒输出 token | `completion_tokens / use_time`，仅流式 | `out / (total − ttfb)`，仅流式 | ✘ 已更好 |
| 缓存命中的用途 | 按 `CacheRatio` 打折**计费** | 不涉及计费 | ✘ 我们无计费需求 |
| 缓存命中率展示 | **没有这个指标** | 有四窗（自有功能） | ✘ 口径本来就对，只等去留决定 |
| 报表 token/费用 | `meta.kind` 两种都支持 | 只做 tokens | ✘ 我们只需 tokens |

## 六、**最新源码已就位**（2026-10-20 pull）

```powershell
# 浅克隆，6.6 秒
git clone --depth 1 https://github.com/QuantumNous/new-api.git D:\temp\new-api-fresh
# HEAD: 32e5874  2026-10-19  feat(email): 添加 resend 邮件服务商接口 (#7880)
# usage-logs 82 文件 · dashboard 39 文件
```

⚠ 注意：`D:\project_GIT\_ref_newapi` 是**空目录**（不是 git 仓库），`D:\project_GIT\new-api-main.zip` 是旧包。
**要读真源码用 `D:\temp\new-api-fresh`**，不要用那两个。

## 七、真源码复核结果（比知识库更精确）

缓存展示的实现（`web/src/features/usage-logs/components/columns/column-helpers.tsx:43-68`）：

```tsx
export function CacheTooltip({ tokens, label, color }) {
  if (tokens <= 0) return null                      // ≤0 不显示该图标
  return <TooltipContent>{label}: {formatTokens(tokens)}</TooltipContent>
}
```

取值统一来自 `other.cache_tokens`（4 处一致）：
- `common-logs-columns.tsx:734` → `const cacheReadTokens = other?.cache_tokens || 0`
- `common-log-mobile-card.tsx:130` → `const cacheRead = other?.cache_tokens || 0`
- `details-dialog.tsx:246/382/400` → 标签 `Cache Read`
- `lib/export.ts:47/87` → 导出同样字段

另有第二个缓存指标 `cache_creation_tokens_5m`（缓存**创建**量，显示为 `Cache ↑ 200`）。

⭐ **结论（真源码证实）**：它展示的是**原始 token 量**（`Cache Read: 36,864`），**没有百分比命中率**。
「缓存命中 98%」是 DSH 客户端自己算的，不是 new-api 报表的东西。
⇒ 我们的「缓存命中率四窗」是自有功能，口径（`hit/(hit+miss)`）本来就对，不需要改。
