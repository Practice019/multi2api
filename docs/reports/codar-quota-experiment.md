# 决定性实验：`token` 字段是"上限"还是"general 桶余额"（T-347）

结论一句话：**是 general 桶余额（能花），不是上限**。两个桶各自可被消费扣减，
扣费顺序看起来是"先扣 agent 桶，agent 桶为空时才扣 general"。

## 1. 实验设计

两个账号各发 **1 次**真实请求（`glm-5.3-flash`，`max_tokens=20`，非流式，直连上游不带网关），
请求前后各记一次 `agent/balance` + `gettoken` + `agent/ledger`。

为什么这样能判定：

| 假说 | 预测 |
|---|---|
| A. `token` = 上限（配额池大小） | 消费**不会**让 `token` 变小（上限不变），扣减只体现在 `agent_token` 或一个未暴露的桶 |
| B. `token` = general 桶余额 | 若该桶被扣，`token` **变小**，且 ledger 出现 `point_type=general` 的负数 |
| C. 两桶独立、各管一类模型 | 同上，但能观察到"哪个桶在减"取决于模型/账号状态 |

## 2. 数据

| 账号 | 步骤 | agent_balance | gettoken.token | agent_token | ledger 总数 | 新增流水 |
|---|---|---|---|---|---|---|
| 老 `user_1508…7985` | 基线 | 1707 | 2500 | 1707 | 15 | — |
| 老 `user_1508…7985` | **消费后** | **1706** | **2500（不变）** | 1706 | 16 | `change=-1 point_type=agent note="glm-5.3-flash \| token：输入 13，输出 20，缓存 0"` |
| 新 `user_1927…8275` | 基线 | 0 | 500 | 0 | 1 | `change=+500 point_type=general note="新用户赠送"` |
| 新 `user_1927…8275` | **消费后** | **0（不变）** | **499** | 0 | 2 | `change=-1 point_type=general note="glm-5.3-flash \| token：输入 13，输出 20，缓存 0"` |

两次请求都**成功**（响应是正常的 `chat.completion`，含 `reasoning_content`）⇒ 新账号的 500 **确实能花**。

## 3. 结论（B 的修正版）

1. **`token` 不是上限** —— 它因消费从 500 变 499 ✗ 上限不会变；
   （此前"传 token:0 与 token:1 返回值相同 ⇒ 它是上限"的推理不成立：
   那个入参看起来只是请求开关，跟返回值语义无关。）
2. **`token` = general 桶余额**（可花）。`agent_token`/`agent_balance` = agent 桶余额（可花）。
   两本账在 ledger 里分别以 `point_type=general` / `agent` 记账 ✓
3. **扣费有顺序**（推断，但两个账号的表现自洽）：**先扣 agent 桶；agent 桶为空才扣 general**
   - 老账号两桶都有钱 ⇒ 只扣 agent 桶（`token` 纹丝不动 ✓）
   - 新账号 agent 桶为 0 ⇒ 只能扣 general（`token` 500→499 ✓）
4. ⇒ **总可用额度 = `agent_balance` + `token`**
   - 老账号 1706 + 2500 = **4206**（此前我们只显示 1706）
   - 新账号    0 +  500 = **500**（此前我们显示 0 —— 用户问的"怎么可能没有额度"至此有定论）

## 4. 对实现的建议（`quota_detail` / `RefreshQuota`）

```json
{
  "agent_balance": 4206,          // ← 保持字段名（前端兼容）：改成"可花总和"
  "ok": true, "uid": "…", "upstream_status": 200,
  "quota_detail": {
    "agent":   { "balance": 1706 },        // agent 桶（= /v1/agent/balance）
    "general": { "balance": 2500 },        // general 桶（= gettoken 的 token）
    "rewrite": { "balance": 0 },           // gettoken 的 rewrite_token（本项目恒 0）
    "total": 4206,                          // 可花总和 = 三桶相加
    "spend_order": "先扣 agent，agent 为空才扣 general（T-347 实验推断）",
    "source": { "agent": "/v1/agent/balance", "general": "/v1/gettoken" }
  },
  "raw": { "balance": {…}, "gettoken": {…} }
}
```

**逐条说明：**

1. `RefreshQuota` 的 `QuotaView` 值改为**三桶求和**（`agent_balance + token + rewrite_token`）——
   这次是**实验支持**的求和，不是 T-346 那版猜测的求和；
   ⚠️ 与 T-346 版的区别：T-346 把 `token` 当"上限"才被撤回，本版有"它会被消费扣减"的直接证据。
2. **两个上游调用都要失败才算 Unknown**：`gettoken` 拿到就加 general 桶；
   只拿到 `agent/balance` 时返回 agent 桶值 + 标 `partial: true`（数据不完整）；
   两个都失败 ⇒ `UnknownQuota()`。
3. **去掉 `used`**：`limit - balance` 的恒等式被实验否决（新账号会算出 500 的假"已用"）。
4. `agent_balance` 字段名保留（前端兼容），语义改为"可花总和"；界面若要区分桶，读 `quota_detail`。

## 5. 未决

1. **扣费顺序是推断**（两个账号的现象自洽，但没有"老账号 agent 桶花光后转扣 general"的实测）。
   要钉死它需要把某个账号的 agent 桶耗到 0 —— 成本高，不建议专门做；出现反例再修。
2. `rewrite_token` 恒 0（两个账号都是）⇒ 它是什么桶没有证据，按 0 计入总和即可。
3. `used` / 上限概念被否决 ⇒ 界面上不要出现"已用/上限"字样（除非另找数据源）。
