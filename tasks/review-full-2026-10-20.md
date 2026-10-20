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
---

## 七、第四~五轮：`wire/payload.go` 的字段白名单 + 全仓 `_ =` 扫描

### ① `supportedFields` 白名单（24 个字段）—— 判定不是问题

```go
"model", "messages", "stream", "tools", "tool_choice", "temperature", "top_p",
"max_tokens", "frequency_penalty", "presence_penalty", "parallel_tool_calls",
"stop", "seed", "user", "n", "logprobs", "top_logprobs", "response_format",
"reasoning_effort", "reasoningEffort", "thinking", "functions", "function_call"
```

**处理顺序（`prepareBodyOptWithLimits`）**：

```
L39  obj["stream"] = true
L50  injectThinking            ← 必须在 whitelist 之前（否则刚注入就被剔）
L58  normalizeMaxTokens        ← max_completion_tokens → max_tokens
L61  applyFieldWhitelist       ← 剔除未知字段
L63  sanitizeMessages
```

**为什么白名单而不是"透传+清洗"**（原注释已说明，这里留档）：
上游对未知字段**直接 400**，而网关会把参数错误误判成账号故障去冷却账号 ⇒
**一个客户端带的新字段就能毒化整个账号池**，表现为所有客户端都 503。

### ⚠ 一个值得记的"疑似缺口"，但我判定不改

**`stream_options` 不在白名单**，而它是 `include_usage: true` 的载体 ——
被剔除意味着**客户端要求"流末返回 usage"这件事被吞了**。

看起来像一个真缺口（少了 usage ⇒ token 只能估算），但**证据否掉了**：

1. 真实日志里 `tokens` 有精确值（`tokens=49367` / `hit=24576`）⇒ 上游确实返回了 usage
2. `internal/upstream/desktop.go:448` 那条路径**网关自己注入** `stream_options.include_usage=true`，
   而它**不经过** `prepareBodyOptWithLimits` ⇒ 该有的地方有

**为什么仍然不把它加进白名单**：原注释记录过一个真踩过的坑 ——
`stream_options` 是让账号池被毒化的三兄弟之一（`max_completion_tokens` + `store` + `stream_options`）。
盲目加进白名单，等于把"某个上游不认它就 400"的风险放回来，
而那类 400 会被误判成账号故障。

⇒ **这是设计权衡，不是缺陷**。要改它需要先知道"每个上游是否认这个字段"，
那需要真机逐上游验证，收益（精确 usage）与现状（已经能拿到 usage）不成比例。

### ② 全仓 `_ =` 扫描（146 处，逐个人工判断）

```
明显可吞 62 处（Body.Close / Encode / Flush / Print / SetDeadline…）
需人工判断 84 处：
  ✓ 判定可吞 8 处
  ✓ 已修 2 处（loomy 面板快照 / clientlogin 归档凭证，commit 8e98b9a）
  ⬜ 低价值未修 4 处：
      clientlogin:864/1083  writeFileAtomic 写备份文件 —— 备份失败不影响主流程
      pool.go:2642 / settings_store.go:437  os.MkdirAll —— 后续 WriteFile 会报错
```

### ③ 「沉默的失败」—— 本轮 review 的主线发现（四例）

| commit | 位置 | 症状 | 严重度 |
|---|---|---|---|
| `4223728` | `logbuf/sink.go` | Scanner 未检查 `sc.Err()` → 日志静默截断 | Major |
| `a0fcaf0` | `account/session.go` | `saveLocked` 的 err 被吞 7 处 → **Revoke 重启后复活** | **Critical（安全）** |
| `8e98b9a` | `loomy/adminpoints.go` | 面板快照三处早退全静默 → 缓存静默不更新 | Minor |
| `8e98b9a` | `clientlogin/clientlogin.go` | 归档写入失败被吞 → 账号静默未归档 | Minor |

**共同点：失败时一个字都不留，用户只会觉得"数据莫名其妙不对"。**

⚠ 唯一够得上 Critical 的是 `a0fcaf0` 里的 `Revoke` ——
用户点"退出登录"看到成功，而那个 token 在进程重启后又能用了。
---

## 八、第六轮：前端 `webui.html`（13500 行内联脚本）

### ① XSS / 注入 —— 判定不是问题

```
innerHTML 赋值 119 处
esc() 辅助存在（L1967）：const esc = s => String(s == null ? '' : s)
用模板串拼 innerHTML 但整行没走 esc 的：2 处
```

那 2 处逐行看过，**都是 SVG 拼接，内容全是数字格式化**：

```
L10560  <line x1="${PADL}" y1="${y.toFixed(1)}" ...>     坐标
L10562  <text ...>${(rv * 100).toFixed(1)}%</text>        百分比
L10564  <text ...>${fmtT(tokenMax * (1 - i / 4))}</text>  格式化后的 token 数
L10729  <rect x="${(cx(i) - barW / 2).toFixed(1)}" ...>   柱状图坐标
```

时间标签来自后端 series 的 `ts` 字段（后端生成），不是用户输入。

⇒ **没有用户数据流入 innerHTML，XSS 判定不是问题。**

### ② `setInterval` 重复注册 —— 判定不是问题

```
L13123  setInterval(() => { pollPoolValues(); pollStatsCards(); loadTrendSeries(); pollAPIKeys(); }, 30000)
L13526  setInterval(() => { if (sectionActive('logs')) loadLogHistory(); }, 10000)
```

**关键判据：它们在函数内还是顶层？** 若在渲染函数内，每次重绘都会注册一个新 interval ⇒ 泄漏。

用大括号配平算过：`renderDailyTrendSVG` 是 **L10658 → L10792**，
而这两处 interval 在 L13123 / L13526 ⇒ **函数外、顶层** ⇒ 只注册一次 ✓

⚠ 我第一版的作用域检测（"向上找最近的 function 声明"）**误判**成
`renderDailyTrendSVG` 内部 —— 因为它只往上找最近的 `function`，
没有确认那个函数是否已经闭合。改用配平才看对。

### ③ `panelPolls` 的轮询守卫 —— 写得好，留档作为正例

```
L6857  const timer = setInterval(async () => {
L6858    if (panelPolls[key] !== timer) { clearInterval(timer); return; }   ← 防重复注册
L6859    if (!host || !host.isConnected) { clearInterval(timer); delete panelPolls[key]; return; }  ← 防已卸载
L6860    if (pendingCount() <= 0) { clearInterval(timer); delete panelPolls[key]; return; }        ← 任务完成即停
L6861    if (++rounds > MAX_ROUNDS) { clearInterval(timer); delete panelPolls[key]; return; }       ← 防无限
```

四条退出条件一个不缺。这是本项目里**唯一**完整的轮询生命周期管理 ——
其他几处 `setInterval` 都靠"顶层只执行一次"这个隐含前提。

⇒ 不是缺陷，但值得记：**如果将来有人把这些 interval 挪进函数，就会立刻泄漏。**
---

## 九、第七轮：并发证据（`-race` 替代方案）

### `-race` 确实跑不了 —— 已尽力验证

```
gcc / cc / clang / x86_64-w64-mingw32-gcc  全部 ✗
go env CGO_ENABLED = 0, CC = gcc
go test -race ./internal/wire/
  → cgo: C compiler "gcc" not found: exec: "gcc": executable file not found in %PATH%
```

这是**环境限制**，不是跳过。要跑起来需要装 MinGW-w64 或 TDM-GCC。

### 替代方案：`go vet` 全绿

`go vet` 的 `copylocks` 与 `loopclosure` 检查覆盖了两类最常见的竞态近亲：

```
go vet ./...              exit=0  ✓ 无告警
go vet -copylocks ./...   exit=0  ✓ 无锁按值拷贝
go vet -loopclosure ./... exit=0  ✓ 无循环变量捕获
Go 1.22.5（loopvar 语义已按迭代变量）
```

**这能证明的**：没有 `sync.Mutex` 被按值拷贝（那会让两把锁保护同一份数据）、
没有经典的 `for` 循环变量捕获。

**这不能证明的**：逻辑层面的竞态（两个 goroutine 用同一把锁但更新顺序错、
读-改-写之间释放了锁）。那类只有 `-race` 或人工推理能发现。

### 顺带修了一个本地工作区污染

```
go vet ./... 原本 exit=1：
  docs\agent2api-repo\...\login_vectors_test.go:19
    no required module provides package github.com/mmqz/cpa-multi-plugins/...
```

`docs/` 下有 2 个从参考项目移植来的 `_test.go`（41 KB），引用不存在的模块。
它们**不在 git 里**（`.gitignore: docs/*`），所以**别人克隆不会有这问题** ——
但在我本地，`go test ./...` 和 `go vet ./...` 会失败，看起来像项目坏了。

**已修**：给这两个文件加 `//go:build ignore` + 说明注释，留在磁盘作参考但不参与编译。

**修后**：

```
go build ./...  exit=0
go test ./...   exit=0   ok=39 FAIL=0
go vet ./...    exit=0
```

⚠ 这个修复**不进 git**（文件本身不被跟踪），只在本地生效。
将来若有人在本地也放了参考代码副本，会遇到同样症状，届时按同样办法处理。
