# new-api 图表对照 — 最终结论（2026-10-20 收口）

> 源码：`D:\temp\new-api-fresh`（HEAD 32e5874 2026-10-19）
> **这份是结论。图表对照这件事到此结束，没有未决项。**

## 一、它到底有几种图 —— 已确认

`components/models/model-charts.tsx:44-47`：

```ts
type ChartSpecKey = 'spec_model_line' | 'spec_pie' | 'spec_rank_bar'

const VIEW_SPEC = {
  trend: 'spec_model_line',    // ← 折线 = 趋势视图
  pie:   'spec_pie',           // ← 饼图
  rank:  'spec_rank_bar',      // ← 排行条形
}
```

**关键**：这三种是**同一份模型维度数据的三种画法**（趋势 / 占比 / 排行），
不是三张不同的数据。

## 二、逐项对照 —— 我们全都有

| new-api | 性质 | 我们 |
|---|---|---|
| `spec_model_line`（趋势折线） | 模型维度 × 时间的趋势 | ✅ 按天 Token 趋势（柱+折线叠加，v1.9.6） |
| `spec_pie`（饼图） | 模型/渠道占比 | ✅ 上游用量环 + 模型用量环 |
| `spec_rank_bar`（排行条形） | 按量排序 | ✅ 次数排行 |
| 消耗分布堆叠柱 | `consumption-distribution-chart.tsx` | ✅ `#statsTrend`（带柱状/面积切换） |
| 汇总卡 | `overview/summary-cards.tsx` | ✅ 13 张（总处理量 + 输出 token 并存） |
| 性能健康 | `overview/performance-health-panel.tsx` | ✅ 健康条（成功率/延迟/吞吐/模型 chip） |
| **在线率** | `overview/uptime-panel.tsx` | ❌ **唯一没有的** |
| 公告 / API 信息 / FAQ | `overview/*-panel.tsx` | ❌ 自用网关不需要 |
| 用户维度图 | `users/user-charts.tsx` | ❌ 单用户，不需要 |

## 三、我们比它多的

| 我们的 | new-api |
|---|---|
| 活跃热力图（365 天日历阵） | ❌ 没有 |
| 缓存命中率四窗 | ❌ 没有（它只显示 Cache Read 原始量） |
| 按天 Token 趋势独立面板 | ❌ 没有（只在模型页里作为视图之一） |
| 双轴趋势 | ❌ 没有 |

## 四、结论

**「照 new-api 补齐图表」这件事已完成** —— 它的四种图表形态我们都有，
且我们另有四种它没有的视图。

**唯一实质缺口：在线率面板（`uptime-panel`）**

它能回答「过去 N 天有多少时间服务不可用」。对多上游自用网关有意义：
某个上游频繁 502 时，这个数会说话（成功率只看请求，在线率看时间）。

是否要做，等用户表态。它不属于「口径」范畴，属于新增功能。
