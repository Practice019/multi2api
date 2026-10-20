# 全项目 Review（2026-10-20，进行中）

> 方法：按 `/review` 的五轴（正确性 / 可读性 / 架构 / 安全 / 性能），以**模式扫描 + 定点取证**为主，
> 不做逐文件通读（359 个生产文件 / 38 个包，通读成本远大于收益）。
> 原则：先证明"这个数/这段代码真的是错的"再动手，不凭直觉加守卫。

## 一、已修的真 bug

### ① `logbuf/sink.go` —— Scanner 超长行静默截断（commit `4223728`）

```go
sc := bufio.NewScanner(f)
sc.Buffer(make([]byte, 0, 64<<10), 1<<20)   // 单行上限 1 MiB
for sc.Scan() { ... }
return out, nil                            // ⚠ 从不检查 sc.Err()
```

`bufio.Scanner` 单行超上限时**循环正常退出**，错误只在 `sc.Err()` 里。

⇒ 一次超长行会让「日志少了一截」**没有任何痕迹**：不报错、不打日志，
HTTP 层拿到的是一份**看起来成功**的不完整数据。这属于"沉默的失败"——真发生时无从排查。

**修法**：`if err := sc.Err(); err != nil { return out, err }`，让 `LoadAll` → HTTP 层
把错误抛给前端，前端能显示"读取失败"而不是展示一份不完整的日志（后者更危险）。

⚠ 实测不触发（单行 300~400 字节，离 1 MiB 有 2500 倍余量）。修它是因为成本一行，
不修的成本是将来一次查不出来的错误报告。

**4 条守卫**：`LoadAllReportsOverlongLine` / `LoadAllNormalFileNoError` /
`LoadAllMissingFileIsNotError` / `ScannerBufferLimitIsOneMiB`

## 二、判定「不是问题」（留档防重复怀疑）

### ① `wire` 包的 SSE 解析没有 64KB 上限

```
wire/sse.go:181, 555        bufio.NewReaderSize(r, 64*1024)
wire/stream_frames.go:64    bufio.NewReaderSize(r, 64*1024)
```

用的是 `bufio.Reader.ReadString('\n')` 而不是 `bufio.Scanner` ⇒ **没有单行上限**，
那个经典的 SSE 帧截断 bug 不存在。

全仓只有 `logbuf/sink.go` 一处用 `Scanner`；`checkinlog` 用别的读法 ⇒ 无同类问题。

### ② `cache_hit_tokens` 的"荒谬值"不是 bug

```
hit  P50 = 175,232   P90 = 462,592   最大 = 1,039,616
```

我一开始判定成累加 bug，加了一道 2M 上限守卫 —— **被自己的测试推翻**
（`1,039,616 < 2,000,000`，守卫根本不生效）。继续推发现判断本身就错：

1. `frameUsage` 只取**最后一帧**（覆盖语义，不是逐帧累加）⇒ 上游单帧就给这个值，我们没算错
2. 最大那条 `hit=1,039,616` 同时旧 `tokens=980` —— **矛盾的是旧的 `tokens`**
   （它信了上游漏算缓存的 `total_tokens`），**不是 `hit`**。新版用 `hit+miss` 反而算对了
3. 加守卫会误伤真实的长上下文请求（上游确实支持 1M 级窗口）

⇒ **不做解析层守卫**。真正该守的在聚合侧（`cacheRowTrusted: hit > tokens*10`），
那里有跨行的相对比较，比单条请求的绝对阈值可靠。

⚠ 报告者说"上游 `cached_tokens` 是累计口径"——**存疑但无证据**：我没拿到同一请求的
多帧 SSE 原始报文，无法证实或证伪。要查清需要抓两帧对比。

### ③ `pool` 的 `provState` 无 delete 不是无界增长

它的 key 是 provider 名（约 10 个上游），**天然有界**。
`byUID` 则是赋值 3 处 / `delete` 3 处，匹配 ✓

### ④ `pool` 的锁内 IO 扫描为误报

`L586` 的 `SetStore` 只是 `p.store = s` 赋值，不是 IO。

### ⑤ `cancelReadCloser` 与 Body 的 Close 规范

```go
// chattool_send.go:169
func (c *cancelReadCloser) Close() error { c.cancel(); return c.rc.Close() }
```

正确（cancel + 关底层）✓

几处非 defer 的 Close（`upstream:620-621`、`trae:252-253`、`loomy:310-311`）
都是 `io.ReadAll` 之后**紧跟** `resp.Body.Close()`，不是泄漏。
（极端情况下 ReadAll panic 会漏，但那不是当前的真实缺陷。）

## 三、两轮 review 的方法论教训

**我在同一件事上犯了两次错**：

| 次 | 做法 | 结果 |
|---|---|---|
| 第一轮 | 看到 `cache_hit_tokens` 数字大就加守卫 | 被自己的测试推翻，判断本身是错的 |
| 第二轮 | 找到 Scanner 问题时先 grep 全仓确认只有一处、先查实测余量 | 一次改对 |

**差别**：前者是"看见数字异常就改"，后者是"先证明它真的是错的才改"。
后者才是正确顺序——**加守卫之前必须先证明那个数真的是错的**。

## 四、进度

```
✅ internal/session/  粘性四层（前缀指纹 / provider 过滤 / 本地兜底 / context 分类）—— v1.9.12
✅ internal/server/   token 三条路径统一（f46cdcd + 5efd4b9 + 767667f）
✅ internal/wire/     SSE 用 Reader 不是 Scanner，无 64KB bug
✅ internal/logbuf/   Scanner 未查 Err —— 已修（4223728）
✅ internal/pool/     无界增长 / 锁内 IO / byUID 平衡 —— 均判定不是问题
✅ internal/upstream/ Body Close 规范、无 fd 泄漏
⬜ internal/admin/    aggregateChatLog 的守卫阈值（待查）
⬜ internal/account/  鉴权与 session 持久化（待查）
⬜ 前端 webui.html    13500 行的内联脚本（待查）
```

## 五、尚未覆盖（诚实标注）

- **没有跑 `-race`**：本机 `CGO_ENABLED` 未开，`go test -race` 无法运行。
  并发相关的结论**全部来自代码阅读，没有数据竞争检测器的证据**。
- **没有覆盖 38 个包里的全部**：`anthroconv` / `openaicompat` / `qoderwasm` 等协议转换包
  只做了接口边界的抽查，没逐个函数读。
- **前端只做了语法门禁**（6 段全过），没有做 DOM 行为 review。
---

## 六、第三轮：`internal/admin/` 的 `cacheRowTrusted` 阈值（验证通过）

**判据**：`cacheRowTrusted := !(Tokens > 0 && CacheHitTokens > Tokens*10)`

**实测（最近 3000 行真实日志）**：

```
有 hit 且有 tokens 的行: 2351 条
被判不可信（hit > tokens*10）: 1672 条 = 71.1%
```

**被判不可信的都是旧算法的产物**：

```
10-19 16:15  tokens=419   hit=154,752  比值 369   ← tokens 漏算了 hit
10-19 16:15  tokens=644   hit=126,080  比值 196
10-19 16:16  tokens=1067  hit=127,744  比值 120
```

① `f46cdcd` 之前 `tokens` 信上游 `total_tokens`（漏算缓存）⇒ 分母极小 ⇒ 比值爆炸
② 这些行的真实处理量是 `hit+miss+out`（上表第三列），与 `tokens` 差几十到几百倍

**新算法（11:20 重启后）零误判**：

```
新算法行数: 60，其中被判不可信: 0  ✓
12:11:03  tokens=49367  hit=24576  比值 0.5
12:11:59  tokens=55353  hit=24576  比值 0.44
12:13:53  tokens=37718  hit=2048   比值 0.05
```

⇒ **守卫判定正确，阈值合理**。不是 bug。

### ⚠ 但它解释了一个现象（值得记）

71% 的历史行被排除 ⇒ **缓存命中率统计的实际样本只有 29% 的行**。

这正是用户最早看到的「四窗 46.66% vs 近 7 天表格 91~98%」差异的来源之一 ——
两个数的**分母不是同一批行**，一个含被排除的旧数据、一个不含。

⇒ 这是**诚实的**（旧数据确实不可信），但界面上看不出来。
**建议**（未实施）：命中率卡片加一行小字「样本 N 条（已排除 M 条量纲异常的历史行）」，
让"为什么两个数不一样"在界面上自解释，而不是留给人猜。
