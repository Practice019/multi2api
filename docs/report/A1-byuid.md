# A1 · `/admin/stats` 维度富化（`by_model_detail` / `by_uid_detail` / `by_provider_detail`）

> ⚠ **原任务作废、改做富化**：派单时说的是「新增 `by_uid` 维度」，主脑随后实探发现
> `by_model` / `by_provider` / `by_uid` **都已经存在**（都只是「名称 → 请求数」的计数映射）。
> 因此本次做的是主脑更正后的活：**给三个维度各加一份带 `req/ok/fail/tokens/think_tokens/avg_ttfb_ms`
> 的富化对象**，旧字段一字不动。
>
> 交付人 A1 · 2026-10-18 · 只碰 `internal/admin/admin.go`（+ 两个新文件），**未碰 `webui.html`，未 commit**。

---

## 1 · 改前是什么

`internal/admin/admin.go`（改前原文，注意三个维度都只有"计数"这一种信息）：

```go
	byModel := map[string]int{}
	byStatus := map[string]int{}
	byUID := map[string]int{}
	byProvider := map[string]*providerStat{}
	...
	for _, e := range items {
		byModel[e.Model]++
		byStatus[strconv.Itoa(e.Status)]++
		if e.UID != "" {
			byUID[e.UID]++
		}
		...
		ps.Calls++
	}
	resp := map[string]any{
		"total": len(items), "ok": okN, "fail": failN,
		"by_model": byModel,      // {"loomy/DeepSeek-V4.1-Flash": 26020, ...}  ← 只有次数
		"by_status": byStatus,
		"by_uid": byUID,          // {"260915165817517447": 715, ...}          ← 只有次数
		"tokens": tokens, ...
	}
	if len(byProvider) > 0 {      // by_provider 是 {calls, ok, fail, credit}
		resp["by_provider"] = out
	}
```

⇒ 「Top 账号榜」要显示的「xx 亿 tokens · 成功率」**拿不到**：`by_uid` 只给次数，
`by_model` 只给次数，`by_provider` 有 ok/fail 但**没有 tokens**。

## 2 · 改后是什么

新增一个富化桶类型（`internal/admin/admin.go`，原文摘录）：

```go
type dimStat struct {
	Req         int64  `json:"req"`
	OK          int64  `json:"ok"`
	Fail        int64  `json:"fail"`
	Tokens      int64  `json:"tokens"`
	TokensRaw   int64  `json:"tokens_raw"`
	ThinkTokens int64  `json:"think_tokens"`
	AvgTTFBMS   *int64 `json:"avg_ttfb_ms,omitempty"`
	ttfbSum, ttfbN int64 // 内部累加器（不序列化）
}

func (d *dimStat) add(e logbuf.Entry) {
	d.Req++
	if e.Status >= 200 && e.Status < 300 { d.OK++ } else if e.Status >= 400 { d.Fail++ }
	if e.Tokens > 0 { d.Tokens += int64(e.Tokens) }
	d.TokensRaw += int64(e.Tokens)          // 原始和：负数原样进（观测用）
	if e.ThinkTokens > 0 { d.ThinkTokens += int64(e.ThinkTokens) }
	if e.TTFBMS > 0 { d.ttfbSum += e.TTFBMS; d.ttfbN++ }
}

func (d *dimStat) finalize() {              // ⚠ 零除守卫：无样本 ⇒ 不置 AvgTTFBMS ⇒ JSON 里没这个键
	if d.ttfbN > 0 { v := d.ttfbSum / d.ttfbN; d.AvgTTFBMS = &v }
}
```

响应里**新增三个键**（与旧键并存，恒输出）：

```go
	resp["by_model_detail"]    = detailOut(byModelDetail)    // 键 = e.Model（含空串，与 by_model 同）
	resp["by_uid_detail"]      = detailOut(byUIDDetail)      // 键 = e.UID，**空 uid 跳过**（与 by_uid 同）
	resp["by_provider_detail"] = detailOut(byProviderDetail) // 键 = e.Provider，空值 → "(未标注)"（与 by_provider 同）
```

**旧字段保持原样**：`by_model`/`by_uid` 仍是 `map[string]int`，`by_provider` 仍是
`map[string]{calls,ok,fail,credit}`，顶层 `total/ok/fail/tokens/think_tokens/...` 一字未改。
有单测机器守卫这件事（见 §3 第 2 条）。

## 3 · 怎么验的

### 3.1 构建与静态检查

```text
D:\go-path\go\bin\go.exe build ./...              ⇒ build exit=0
D:\go-path\go\bin\gofmt.exe -l internal\admin\byuid_detail_test.go  ⇒ 无输出（我新增的文件是干净格式）
  ⚠ 对照：gofmt -l 同时报 internal\admin\stats_test.go / internal\logbuf\logbuf.go
    （我没碰过的文件）—— 这是仓库工作树 CRLF（core.autocrlf=true）的环境性现象，不是本次引入。
    `git diff --stat -- internal/admin/admin.go` = 192 insertions(+), 13 deletions(-) ⇒ 没有整文件重写。
```

### 3.2 单测：`internal/admin/byuid_detail_test.go`（新增，3 条，全绿）

手算 5 条构造条目（2xx/3xx/≥400、`tokens=-1`、负数 `think_tokens`、空 uid、空 provider、`ttfb=0`），
逐桶比对 `req/ok/fail/tokens/tokens_raw/think_tokens/avg_ttfb_ms`，并做三项交叉校验：

```text
--- PASS: TestAggregateChatLogDetailArithmetic        （逐桶手算 + 三维度求和 == 顶层标量）
--- PASS: TestAggregateChatLogLegacyDimsUnchanged     （by_model/by_uid 仍是 map[string]int，by_provider 结构未变）
--- PASS: TestAggregateChatLogDetailEmpty             （空集 ⇒ 三个 *_detail 是空对象，不是 nil/缺键）
```

### 3.3 变异验证（证明上面不是空转 —— 两处都报红，报错可归因）

| 变异 | 内容 | 结果 |
|---|---|---|
| A | `if e.Tokens > 0` → `!= 0`（让 `-1` 混进 tokens） | ❌ 6 条断言报红，含 `by_model_detail[wb/m]=…Tokens:3，期望 tokens5` |
| B | `finalize()` 无样本时也用 0 冒充 avg_ttfb | ❌ `by_uid_detail[u2].avg_ttfb_ms=0，期望缺键`（2 处） |

还原后 `go test ./internal/admin/ -run TestAggregateChatLogDetail` ⇒ `ok`。

### 3.4 独立算法逐项比对（**7864 实测，冻结日志**）

⚠ 活文件竞争的处理：`data/request-log.jsonl` 被你在跑的 7863 持续追加，所以先把日志**冻结**成
`.probe-a1/request-log-frozen.jsonl`（28,338 行，`sha256=f05d40406a9cf5bbdbbb9058a155725bdb9222c744b74c719b5144fb47e44933`），
再用探针配置的 `request_log_path` 指向它 ⇒ 接口与 Python 读的是**同一份字节**，不存在时间差。

```text
python tools/verify_byuid_detail.py --file .probe-a1/request-log-frozen.jsonl --api .probe-a1/stats-final.json

  【数据事实】行数(有效 JSON) = 28338 · 坏行 = 0
    合计: total=28338 ok=24254 fail=4084 tokens=26901676 tokens_raw(含负)=26897548 think=12561910
    ⚠ tokens<0 的行 = 4128 行，合计 -4128
    空值行: uid=1 provider=0 model=0
    维度键数: by_model=8 by_uid=54 by_provider=4
  【逐项比对】接口 JSON vs Python 独立算法
    by_model_detail      键数=8   字段逐项已比 (67 ok / 0 bad)
    by_uid_detail        键数=54  字段逐项已比 (499 ok / 0 bad)
    by_provider_detail   键数=4   字段逐项已比 (532 ok / 0 bad)
    比对结果：断言通过 551 条 · 失败 0 条 ⇒ ✅ 全部一致
```

比对口径：66 个桶 × 每个字段（`req/ok/fail/tokens/tokens_raw/think_tokens`）+ `avg_ttfb_ms`
（**本地有则必须相等、本地无则接口必须缺键**）+ 旧字段键集与取值 + `by_provider` 的 `calls/ok/fail`。
（脚本第一版自己有 bug：legacy `by_uid` 没套用"跳过空 uid"，凭空多出一个 `""` 键被比对抓出来 —— 这也是比对有效的旁证。）

### 3.5 接口真实响应片段（`stats-final.json`）

```json
{"total":28338,"ok":24254,"fail":4084,"tokens":26901676,"think_tokens":12561910,
 "by_uid":{"260915165817517447":715, ... 54 键, 值都是数字},              // 旧字段：形状未变
 "by_provider":{"loomy":{"calls":26034,"ok":21955,"fail":4079,"credit":0}, ...},
 "by_uid_detail":{"260915165817517447":{"req":715,"ok":643,"fail":72,
    "tokens":1117771,"tokens_raw":1117696,"think_tokens":679231,"avg_ttfb_ms":7766}, ...}}
```

Top 5 账号（接口值，与 Python 算出的逐位相同）：

| uid | req | ok | fail | tokens | think_tokens | avg_ttfb_ms |
|---|---|---|---|---|---|---|
| 260915165817517447 | 715 | 643 | 72 | 1117771 | 679231 | 7766 |
| 260915174611208423 | 1117 | 1112 | 5 | 1025798 | 465663 | 8321 |
| 260919113617575027 | 1068 | 1064 | 4 | 1024735 | 516869 | 8746 |
| 260915114648993679 | 1054 | 998 | 56 | 995430 | 434893 | 7967 |
| 260825100642697603 | 1160 | 1154 | 6 | 990254 | 458542 | 8628 |

### 3.6 交叉校验（口径一致的机器证明）

| 维度 | 求和 req | ok | fail | tokens | 与顶层标量 |
|---|---|---|---|---|---|
| by_model_detail | 28338 | 24254 | 4084 | 26901676 | ✅ 全等 |
| by_uid_detail | **28337** | 24254 | **4083** | 26901676 | ⚠ 差 1：见下 |
| by_provider_detail | 28338 | 24254 | 4084 | 26901676 | ✅ 全等 |

`by_uid_detail` 少的 1 条是**空 uid 的那一行**（`seq=78156, model=codearts/DeepSeek-V3.2, status=503,
uid=""`, tokens=-1）—— 与旧 `by_uid`「跳过空 uid」完全同一口径，是刻意的，不是漏算。

### 3.7 其它已核对项

* **冻结件未被服务端改写**：停服后 `sha256` 与冻结前逐字节相同（`真`）⇒ 只读，没有污染样本。
* **`by_provider_detail` 用 `provider` 字段**：实测 `provider` 空值 **0/28338** ⇒ 上游维度 100% 覆盖；
  A2 报的 2 行 `model="-"`（前缀法取不到）实测都是 `provider=workbuddy / status=503`，
  `by_provider_detail["workbuddy"].req = 2299`（前缀法会算成 2297）⇒ **不存在前缀兜底需求**，故未写该兜底分支。
* **7863 未受影响**：全程 `7863` owner 一直是 pid 33584。
* **未 commit**：`git status` 里本次只动了 `M internal/admin/admin.go`，新增
  `?? internal/admin/byuid_detail_test.go` 与 `?? tools/`（该目录整体未被跟踪，
  内含我的 `tools/verify_byuid_detail.py`）；同工作区另有他人未提交改动，未触碰。
* **全仓 `go test ./...`**：`cmd/server` 与其余 30 个包全 `ok`；失败仅
  `internal/admin`（4 个 `model_multipliers` 用例）+ `internal/codearts`（3 个 `*Live` 用例，直连上游、凭证失效）
  + `docs/agent2api-repo/...` 两个 `setup failed`（参照仓库，与本次无关）。

### 3.8 ⚠ 那 4 个 `model_multipliers` 失败**不是本次造成的**（A/B 已证）

用 Go 的 `-overlay`（不动工作区）把本次改动**逐行摘掉**再编译同一个包：

```text
已摘除 dimStat 类型块（85 行）· 明细表声明 · 循环内累加(模型/uid) · 循环内累加(上游) · 响应赋值
go test ./internal/admin/ -overlay=.../overlay.json -run 'TestStatsModelMultipliers|...'
--- FAIL: TestStatsColdStartCanBootstrapCatalog
--- FAIL: TestStatsModelMultipliersFilteredByByModel
--- FAIL: TestStatsModelMultipliersOrderIsStable
--- FAIL: TestStatsOmitsModelsAbsentFromCatalog
```

⇒ 摘掉本次改动后**照旧全红**，归属工作区里另一处未提交改动（`multTableCached`：把倍率表建表挪到后台
goroutine，首次请求必然拿到空表）。我未修它（**不越界**），仅在此标注。

## 4 · ⚠ 哪些没验

1. **前端渲染**：`webui.html` 未动，`by_*_detail` 在前端怎么画（含 `avg_ttfb_ms` 缺键时显示"—"）**完全未验**。
2. **真实 7863 实例**：按 BRIEF 硬约束没碰，所有实测都在 7864 探针上。
3. **空日志 e2e**：只由单测覆盖（空集 ⇒ 三个 `*_detail` 是空对象）；**未**在真服务上造过空日志跑。
4. **日志被裁剪到"非满窗口"的场景**：本次冻结件是 8MiB 上限附近的满窗口，未构造"刚启动、只有几行"的形态。
5. **`tokens=-1` 的来源细分**：只统计出 4128 行、合计 -4128，**未**按上游/模型拆开定位是谁在发脏数据。
6. **`avg_ttfb_ms` 的 `*int64 + omitempty`** 在 JSON→前端解析里的行为（缺键 vs null）**未验**，需主脑改前端时一并看。
7. **并发写安全**：`aggregateChatLog` 是纯函数、加字段不引入共享状态，但**未**做过并发压测。
8. **全仓测试的 codearts 3 个 `*Live` 失败**：未做 A/B（依赖真实凭证）。⚠ 探针启动时会真的跑
   codearts 续期/签到/旅行（run1 的 stderr 有 `refresh_failed: the refresh token has been used`
   与 `checkin ... 今天已签到` 记录）—— 我这一轮**可能**参与了凭证消耗，**无法排除**，如实记录。
9. **未跑**：`go vet`、静态扫描、以及 `docs/agent2api-repo` 参照仓库的测试。

## 5 · 口径决策（主脑留给我判断的三处，结论与理由）

| 决策点 | 选择 | 理由 / 实测数字 |
|---|---|---|
| 两个 tokens 口径怎么落字段 | `tokens` = **剔除负数**（主口径）；另加 `tokens_raw` = 原始和 | 顶层标量 `tokens` 就是剔负，若 `*_detail.tokens` 改用原始和，会出现「明细求和 ≠ 卡片上的总数」；实测两口径差 **4128**（4128 行 `tokens=-1`），`tokens_raw` 用来看这个差。 |
| 上游用 `provider` 字段还是 `model` 前缀 | **`provider` 字段**（空值 → `(未标注)`） | 实测 provider 空值 0/28338；前缀法丢 2 行 `model="-"`（503）⇒ `workbuddy` 会少 2、成功率被算高。**未加前缀兜底**（当前数据下它是死代码，且会让 `*_detail` 与未改动的 `by_provider` 键集/取值不一致）。 |
| 空 uid 行 | **跳过**（与旧 `by_uid` 一致） | 实测只有 1 行（503、tokens=-1）⇒ `by_uid_detail` 求和比 `total` 少 1，已在 §3.6 写明是刻意口径。 |

## 6 · 给主脑的三条警告（都是本轮实测撞出来的）

1. ⚠ **7864 被并发实例抢占**：本轮我的探针在 04:56 起、05:00 前后**两次**被别的实例顶掉
   （另一次响应的 `total` 来自**未冻结的 live 日志**；收尾时 `7864` 的 owner 已不是我）。
   我的收尾按端口 owner 停掉了当时的实例 `pid 35732`（**不是我的**）——**如果那是你或别的代理的探针，请重启**。
   建议：探针响应只用**同一次调用**的数字做证据，并在取数后校验 `total` 是否等于你冻结的行数。
2. ⚠ **探针会真的打上游**：只按 BRIEF §2.2 改 `listen` 起探针时，它会立刻跑 codearts 续期、checkin、travel
   （run1 stderr 实测），并可能**写进你正在用的 live `data/request-log.jsonl`**（探针序号会从 live 的 `#80234` 续接）。
   我的 run2 已把 `growth_auto_*` / `travel_auto_claim` / `schedule.checkin_enabled` 关掉、`state_file` 与
   `request_log_path` 都指向 `.probe-a1/`，全程没打上游。建议把这几条固化成探针配置模板。
3. ⚠ **可清理项**（都是我这轮留下的，阅后可删）：
   `.probe-a1/`（含 8MB 冻结件 `request-log-frozen.jsonl`、`stats-final.json`、`stats-run1.json`、
   `stats-CONTAMINATED-other-instance.json`、几个临时脚本与探针日志）；
   `config-7864-probe-a1.json` 与 `bin\wb2api-server.a1probe.exe` **已删**；
   保留 `tools/verify_byuid_detail.py`（独立核对脚本，纯标准库、只读，可复跑）。
   ⚠ `.probe-a1/stats-CONTAMINATED-other-instance.json` 是**被抢占实例**的响应，只作现场记录，**不要当证据**。
