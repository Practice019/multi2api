# new-api 图表对照 — 侦察结论（2026-10-20，Round 13 收口）

> **本文件是结论，不是待办清单。** 读了什么、确认了什么、还有什么不确定，都写在这里。

## 一、已抄且已上线（P1，共 9 个展示位）

| 项 | new-api 出处（已证实） | 我们 |
|---|---|---|
| tok/s = `completion_tokens / use_time`，任一 ≤0 显示空 | `usage-logs/components/common-log-mobile-card.tsx:198-200` | ✔ 更精确（用 ms） |
| 仅流式显示速度 | `usage-logs/components/timing-metrics-cell.tsx:223` | ✔ |
| 消耗分布 = 按模型堆叠柱 | `dashboard/components/models/consumption-distribution-chart.tsx` | ✔ |
| 三级 usage 兜底 | `relay/channel/openai/relay-openai.go` | ✔ |
| 时间桶按本地自然日对齐 + tooltip 折叠 | `dashboard/lib/charts.ts`（`timeGranularity` / `MAX_TOOLTIP_MODELS=15`） | ✔ 我们用 6+其他（更保守） |

## 二、`charts.ts` 已读，确认三件事

```ts
processChartData(data, timeGranularity = 'day', t?, chartCornerRadius?): ProcessedChartData
const MAX_TOOLTIP_MODELS = 15
```

1. **它按 `timeGranularity` 分桶**（默认 `'day'`）—— 与我们的 hour/day 同构，不必改。
2. **值的格式化走 `renderQuotaCompat`，按 `meta.kind` 决定显示 token 数还是 USD** ⇒ 它**两种都支持**。
   ⚠ 我先前写过"它只按费用聚合"，那是错的，已在此更正。
3. 每桶带 `TimeSum`（合计）—— 我们叫 `total`，语义一致。

## 三、⚠ 侦察的边界（不编造）

- `flow-charts.tsx`：grep `Line|Area|Bar|series|stack` **只命中一个 import**，**不是**图表定义文件。用途仍**未证实**。
- `model-charts.tsx` / `overview-dashboard.tsx`：未读。
- **结论：我没有找到 new-api 里"折线图"的确切出处。** 继续盲猜文件名的边际收益已经很低。

## 四、关于用户要的「折线图」

用户原话：「还有一些什么速度啊、图表啊这些折线图，这些你们都搞上去吗？」

**我们现在的图全是柱 / 环 / 热力，确实没有折线。** 但有一个事实值得先确认：

- `#statsTrend`（消耗分布）**已经有「柱状 / 面积」切换** —— 面积模式在视觉上就是折线（堆叠面积 + 顶界折线）。
- 真正缺折线的只有**按天 Token 趋势**（它只有柱状）。

⇒ 所以"补折线"这件事，**可能是给按天趋势加一个柱状/折线切换**，而不是新增一张图。
这件事**不需要再读 new-api** —— 需求是用户直接提的，实现方式我们已有现成参考（`#statsTrend` 的切换）。

## 五、下一步（不需要更多侦察）

1. 给按天 Token 趋势加「柱状 / 折线」切换（复用 `#statsTrend` 的模式：`trendView` 变量 + 只重画不重取数）
2. 若用户确认要独立的新图（比如"速度趋势"），再单独设计
3. 缓存命中率四窗 / 近 24h 双轴 —— 去留仍待用户表态，不擅自处理

## 六、已完成的长效验证

- `internal/admin/bucket_start_test.go`：时间桶边界（跨年 / 闰日 / 月末 / 幂等 / 同标签聚合），15 用例全 PASS
- `tests/e2e/probe_output_token_charts.js`：用**反向构造**数据（total 与输出反向）验证图表真的按输出着色 —— 实测柱高 7:140 = 输出比 1:20 ✓
