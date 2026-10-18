# workbuddy2api 的已知问题与未解项

⚠ 这份文件只记**两类**东西：

1. **已修但值得知道为什么**（避免后人"优化"回去）
2. **未解项**（含试过什么、为什么没成）—— ⚠ 后一类最省时间：别人不必再从零试一遍

---

## 未解：请求体错误（HTTP 400）会被"换号重试"，最后报成"池子故障"

### 症状（2026-10-18 实测四次复现）

用一份"合法 JSON 但结构不合法"的 body（`{"model":"loomy/DeepSeek-V4.1-Flash","messages":"not-an-array"}`）
打网关：

```
HTTP 503  45.4 秒
{"error":{"code":"no_healthy_account","message":"all accounts unavailable (cooling/disabled)
  —— 44 个账号：0 禁用、0 冷却、0 个在途未结束（最大 0）:
  upstream client (http 400): {\"error\":{\"message\"…
```

⇒ ⚠ **一个请求体错误被报成了"池子真空"**，而且**白烧 44 次往返**（复现耗时 6.7s ~ 45s）。

### 为什么会这样

`status >= 400` 一律归 `ErrKindClient`，而它的语义是"**只换号不罚**"。
对 401/403 那是对的（不同账号凭证不同，换号可能有用）；
对 **400 是错的** —— 每个账号收到的 body 一模一样，换到哪家都是同样的 400。

正是 new-api 用 `IsAlwaysSkipRetryCode/StatusCode`（`service/relay_error.go:52-55`）
与 `ErrOptionWithSkipRetry()` 显式声明要避免的事。

### 试过的三种改法（⚠ 全部撤回，都成了我的教训）

| # | 改法 | 结果 |
|---|---|---|
| ① | 在 `status >= 400` **开头**判 `status == 400` ⇒ break | ⚠ **跳过内容拦截的降级路径**（内容拦截也是 400）⇒ **五个 `TestContentBlock*` 当场变红** ⇒ 撤回 |
| ② | 在 `classifyErr` **之后**判 `status == 400 && kind != ErrKindContentBlocked` | ⚠ **未拦住**（仍 503、仍试遍 44 个号）⇒ 说明 `kind` 落在 ContentBlocked（上游对那个坏 body 的报文命中了内容拦截标记）⇒ 撤回 |
| ③ | 在 `terr != nil` 里 `errors.As(terr, &ce)` 取 `*chatError` 判 `ce.Status == 400` | ⚠ **未命中**（`terr` 的真实类型待确认）⇒ 撤回 |

### 下一步该查什么（⚠ 别再从零试）

1. **先加一行诊断**：在 `status >= 400` 段里打印 `kind` 与 `status` 的真实值
   —— ② 失败的原因立刻就能确认，不必再猜。
2. **确认 400 走哪条路**：`chatVia`（handler.go:2401）在单上游模式下把非 2xx
   的**错误体包成流**并返回 `status`（不是 err）；多上游模式下只把**传输层**失败
   放进 `err`。⇒ 先用诊断确认这次是哪条。
3. ⚠ **改的时候注意两条既有契约**：
   - 内容拦截（400）必须仍走**降级提示词重试**（那五个测试守着它）
   - `sessionDeadMarkers` 里的 `12153` 靠**响应正文**分类 ⇒ 别在图省事时把 body 丢掉
     （`chatVia` 的注释里记着这条真踩过的坑）

### 为什么值得修

它把"请求体错误"报成"池子故障" —— ⚠ 这正是用户反复抱怨的那类**语义错**
（"这个账号在池里啊，怎么说不在了"）。但它**不影响正常请求**，
所以优先级低于"上游整体不可用"那类问题。

---

## 已修：值得知道为什么的几处

| 位置 | 做了什么 | 为什么（⚠ 别改回去） |
|---|---|---|
| `pool.pickForTier` | 排除"**明确知道**零额度"的账号 | ⚠ 判据必须是"明确知道"：`QuotaView.EffectiveFor` 对 `!HasData`（未探测）与 per_model 缺键**都返回 0**，直接用它会把新号永久排掉 |
| `pool` 的 `provState` | 上游级熔断 | 账号级熔断的聚合效应是"上游整体坏了也要等 44 个号逐个熔断" |
| `handler` 的选不出号处 | 自动换一个**同模型**的其它上游 | 实测 `deepseekv41flash` 三家都有，而 loomy 15.22% 失败、workbuddy 0.13% ⇒ 差 117 倍 |
| `chattool_send.go` | 工具循环用 `AuthByUID` 而非 `PickByUID` | 那个在途名额**是本次请求自己占的**，再判一次等于把自己挡住 ⇒ 恒 503 |
| `logbuf.Entry.Decision` | 判决留痕（`reason:source`） | ⚠ 只存 reason+source：`action` 已由 `Status` 体现，而"为什么"是行为里看不出来的 |
| `gateway.ErrConvertFailed` | 本地转换失败 ⇒ 不换号、回 400 | 换号对"请求体转不出来"完全无效（每个号背后同一套转换代码） |
