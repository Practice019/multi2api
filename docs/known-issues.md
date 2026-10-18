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
| ② | 在 `classifyErr` **之后**判 `status == 400 && kind != ErrKindContentBlocked` | ⚠ **未拦住**（仍 503、仍试遍 44 个号）⇒ 撤回 |
| ③ | 在 `terr != nil` 里 `errors.As(terr, &ce)` 判 `ce.Status == 400` | ⚠ **未命中**（`terr` 的真实类型待确认）⇒ 撤回 |

### ⚠ 后续进展（2026-10-18 晚，在同一晚继续做）

**分类层已经修好了**（按知识库 `experiences/错误分类落点判定` 的落点：各上游自己的分类器）：

- `upstream.ErrKind` / `gateway.ErrorKind` 各追加末位一类 `ErrFatalClient` / `ErrKindFatalClient`
  （⚠ **只追加不插入** —— 两处都有测试钉住取值稳定，且两套枚举必须同序）。
- `internal/loomy/errorclassifier.go`：加 `markerInvalidRequest`（`invalid_request_error`）
  与 `markerInvalidRequestCN`（`请求参数错误`）⇒ 归 `ErrKindFatalClient`。
  ⚠ 两个形态都列：loomy 的报文里英文 type 与中文 message **同时出现**。
- `internal/upstream/client.go`：加 `fatalClientMarkers`（上下文超限 + 参数错误两类）。
- 三处镜像/对齐测试同步（`workbuddy` 的 `knownUpstreamKinds` 与翻译、`defaultupstream_test.go`、
  `upstream_kind_mirror_test.go` 的 `probeFor`）—— ⚠ 它们**会拦住只改一边**。
- 出口层：`status >= 400` 段里加 `kind == ErrKindFatalClient` ⇒ 回 400 + decision `fatal_client:client`。

**实测证据（新二进制起 7864 打坏 body）**：

```
HTTP 503（4.5s）… upstream **fatal_client** …      ← ⚠ 分类确实生效了（原来是 client）
```

⇒ ⚠ ⚠ **但请求仍试遍 44 个号** —— 说明**这条请求走的不是 `status >= 400` 段**，
   而是 `terr != nil` 段（报文格式 `upstream fatal_client (http 400): …` 正是 `chatError.Error()`）。

**⚠ 下一次该从这里继续（别再从零试）**：

1. **先加一行诊断**：在 `terr != nil` 段里打印 `%T` 与 `ce.Kind`（若能 `errors.As` 到），
   ⚠ 一眼就能确认这条路径上 `terr` 到底是什么、Kind 是什么 —— 我已经用 `ce.Kind` 判过一次
   而**未命中**，所以"它是不是 `*chatError`"这件事必须先确认，不该再猜。
2. 若 `terr` **不是** `*chatError`：查 `internal/loomy` 的 provider 在非 2xx 时把什么放进了 err
   （`client.go` 那条 `return io.NopCloser(bytes.NewReader(raw)), status, raw, nil` 说明
   **非 2xx 本该走 status**；⚠ 与实测"走 terr"矛盾 —— 这本身就是个待解释的观察）。
3. ⚠ **不要**再用 `ce.Status` 判（③ 就是这么失败的）。

⚠ 触发条件：只在**故意构造的不合法 body**上复现，正常请求不受影响 ⇒ 优先级可低于线上故障。

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
