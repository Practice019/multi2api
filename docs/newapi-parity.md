# 对照 new-api 的逐模块结论

⚠ 本文件是「把成熟项目的实现抄过来替换我们的」这件事的**决策记录**。
每决定一项就来这里加一行 —— ⚠ 目的：**避免重复对照**，也避免"为抄而抄"。

知识库入口：`python tools/ovq.py <ls|tree|read|grep|find> …`（DSH 的 MCP 断线时用这个直连）
主参照：`viking://resources/repos/QuantumNous/new-api/…`

## 结论表

| 序 | 模块 | 结论 | 依据 |
|---|---|---|---|
| **1** | 选号 + 重试 | ⚠ **我们早抄过了 ⇒ 不换** | `pool.go` 的 `PickForTier` 注释写着"对标 new-api 的 retry→降档"，且已在 `handler.go:1488` **接线**；`service/channel_select.go:114 CacheGetRandomSatisfiedChannel` / `RetryParam` 的语义与我们一致 |
| **2** | 冷却 / 熔断 | ⚠ **我们更细 ⇒ 不换** | 见下面「冷却对照」 |
| **3** | 失败分类 | ⚠ **我们更细 ⇒ 不换**；但**判决留痕该抄、已抄** | `internal/gateway/errorclassifier.go:99` 六类 vs 它的 `ErrorKind`；留痕见 `logbuf.Entry.Decision`（依据 `service/request_policy.go:19` + `api.ts:7`） |
| **4** | 额度 / 配额视图 | ⚠ **价值低 ⇒ 不换** | 它是**多用户计费网关**（user/token/channel 三层额度 + 预扣 + 结算 + 充值）；我们是**单用户自用**（只有账号额度一层）。真正相关的只有"**怎么判断一个渠道还有额度可用**"，而那条**第 1 项已查过**（`abilities` 表 ⇒ 从候选消失） |
| **5** | 请求日志 / 统计 | ⚠ 未查 | — |
| **6** ⚠ | **请求体转换失败的处理**（清单外，从 `relay/` 目录发现） | ⚠✅ **该抄** | 见下面「转换失败对照」 |

## 转换失败对照（第 6 项 · ⚠ 已确认我们缺）

new-api（`relay/convert_request_error.go` · `relay/param_override_error.go`）：

- 转换失败一律用**专门错误码**（`ErrorCodeConvertRequestFailed` /
  `ErrorCodeChannelParamOverrideInvalid`），而不是笼统的 500
- ⚠⚠ **一律带 `types.ErrOptionWithSkipRetry()`** —— "这类错误重试没有意义"是**显式声明**的
- ⚠ 若损失可诊断（`types.ConversionLossError`）⇒ `info.RecordConversionDiagnostics(c, loss.Diagnostics)`
  把诊断记下来，再返回 400

我们（`internal/minimax/provider.go:164` · `client.go:438`）：

```go
anthroBody, err := anthroconv.OpenAIToAnthropic(body)
if err != nil {
    return gateway.ChatStream{}, fmt.Errorf("minimax: 请求体转换失败: %w", err)   // ⚠ 裸 error
}
```

⇒ ⚠ 出口层分类靠 `classifyErr(...)`（按 **status code + body**），而转换失败是**本地**失败、
**没有 status code** ⇒ ⚠ 可能落到"未分类"⇒ 被当**可重试** ⇒ ⚠ **每次请求白换 N 个号**（MaxRotate 次）。

⚠ 我们**有**"终态"机制，但只在**另一个域**：`internal/cline/errors.go` 的 `ErrRefreshExpired`
（凭证**续期**的终态）。⇒ **两个域都有"终态"概念，而请求转换这个域没标。**

**该做的**：给转换失败打一个明确分类（送进 `ErrorKind` 体系，与 `ErrKindContentBlocked`
那种"不该换号"同类），⚠ 并在判决留痕里体现（`convert_failed:system`）。

## 冷却对照（第 2 项）

new-api（`service/channel.go` 等）：

- 状态只有三档：`ChannelStatusEnabled` / `ManuallyDisabled = 2` / `AutoDisabled = 3`
  （`common/constants.go:269-270`）
- `DisableChannel(channelError, reason)` ⇒ `UpdateChannelStatus(…, ChannelStatusAutoDisabled)`
- `ShouldDisableChannel(err)`（`service/channel.go:72`）**按错误类型**决定封不封
- ⚠ `ShouldEnableChannel`（:94）第一行就是
  `if !common.AutomaticEnableChannelEnabled { return false }`，
  而 `common/constants.go:135  var AutomaticEnableChannelEnabled = false`
  ⇒ **它默认不自动恢复**（要靠探活或人工）

我们（`internal/pool/pool.go`）：

- `until`：即时冷却（`CoolSoft` = 429 短冷却；`CoolHard` = 额度耗尽，到上游给的重置时刻）
- `breakerUntil` + `retryCount`：连续失败 **熔断 + 指数退避**
- `softStreak` + `softRateModel`：**429 的模型级指数退避，且持久化**
  （注释写明：不落盘的话一次重启就把退避指数清零，而上游的限流并未因我们重启而消失）
- `fails`：连续失败计数，跨入口累计，成功 / 熔断 / 统一复活时清零

⇒ **我们在四件事上更强**：瞬时冷却、熔断退避、自动恢复、模型级限流豁免。
⇒ 唯一可借鉴的是"**按标签批量禁用 / 启用**"（`EnableChannelByTag` / `DisableChannelByTag`，
`model/channel.go:819/828`）—— ⚠ 但那是**运维操作**、需要界面入口，不是核心逻辑。

## 原则（写下来免得后人误读）

1. ⚠ **不是所有东西都该换** —— 每项先**证伪我们是否早就有了**（第 1、2、3 项都证明了有，且有的更强）。
2. ⚠ **抄逻辑、不抄数据模型** —— 它 MySQL + GORM，我们内存 + JSON 文件。
3. ⚠ **每处改动在注释里写出处**（文件 + 行号），否则后人不知道依据在哪。
4. ⚠ **抄也要留我们的接缝** —— 例如判决留痕只存 `reason:source`，`action` 交给 `Status` 表达。
