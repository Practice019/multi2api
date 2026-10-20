# 缓存命中率的正确公式（DSH 源码定位，2026-10-20）

## 一、权威出处

`D:\project_GIT\test\deepseek-harness-codearts\src\openai-gateway\usage.ts:12-21` 有一张对照表，
是整个 DSH 里最清楚的口径定义：

| | DSH `TokenUsage` | OpenAI `usage` |
|---|---|---|
| 输入 | `inputTokens` **只含未命中缓存**的部分；命中单列 `cacheReadTokens` | `prompt_tokens` / `input_tokens` 是**含缓存**的总输入 |
| 明细 | 无 | `prompt_tokens_details.cached_tokens` 是上者的**子集**（不是并列项） |

它同时列出各适配器统一在做的那一步减法（`openai-compat.ts:892`、`buddy-adapter.ts:1988`、
`lobsterai-adapter.ts:1488`、`llm-adapter.ts:1797`、`gemini-messages.ts:455`）：
把上游**含缓存**的 `prompt_tokens` 减成 DSH 的**互斥**口径。

## 二、由此推出：两种口径数学等价

```
DSH 侧：   total = inputTokens + cacheReadTokens + outputTokens     （三者互斥，可直接相加）
OpenAI 侧：total_tokens = prompt_tokens + completion_tokens           （prompt 已含缓存）
          且 prompt_tokens = inputTokens + cacheReadTokens
⇒ 两式的 total 完全相等 ✓
```

**所以我们的 `s.tokens`（上游 `total_tokens`）就是 DSH 的 total。** 指标卡那个
「总处理量 = 输入 + 输出（缓存写入不重复计入）」与 DSH 的式子等价，不必新算。

## 三、缓存命中率的公式

```
命中率 = 缓存读取 / 总输入
       = cached_tokens / prompt_tokens              （OpenAI 口径）
       = cacheReadTokens / (inputTokens + cacheReadTokens)   （DSH 口径）
```

两个写法**完全等价** —— 因为 `prompt_tokens = inputTokens + cacheReadTokens`。

⚠ 关键：**分母是「总输入」，不是「总处理量」**。不能用 `cached / total_tokens`，
那会把输出 token 也放进分母，把命中率算得偏低（输出越多偏得越厉害）。

## 四、⚠ DSH 开源部分**没有**这个百分比

grep 全库（排除 node_modules / dist / tests）搜 `cacheHitRate|cacheRate|hitRate|命中率`
与 `* 100`：

- 只有**注释**提到"缓存命中率会显示偏大"（`buddy-adapter.ts:2346` 等 4 处），
  说的是"命中部分要单列，否则算出来偏大"—— **这是口径约束，不是实现**
- **没有任何一处计算这个百分比**

⇒ 用户看到的那个「缓存命中 98%」不在 DSH 的开源仓库里。可能来源：
1. DSH 客户端 UI 的闭源包（`@deepseek-ai/dsh-client-ui-chat`）
2. 或者是**上下文占用百分比**（`usage.ts:23-26` 记着一个真实缺陷：
   "接入 Codex 后上下文占用被少算上百倍…换成 trae 就正常显示占了 300k 多"
   —— 那是 `已用 / 窗口`，与缓存命中是两个不同的指标）

## 五、我们该怎么做

**如果要做缓存命中率**，照第三节的公式：

```js
const hitRate = (cached > 0 && prompt > 0) ? (cached / prompt * 100) : null;
// 显示：hitRate === null ? '—' : fmtTps(hitRate) + '%'
```

⚠ 分母用 `prompt_tokens`（总输入），**不是** total_tokens。
⚠ 无缓存字段时显示 `—`，不回退成 0。

**先确认要不要做** —— 用户提过两次「缓存命中是多少」，倾向是要。但需要先确认
他要的是"命中率百分比"还是"缓存命中的 token 量"（后者我们已经有了）。
