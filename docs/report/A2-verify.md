# A2 · 独立核对脚本（只读 `request-log.jsonl`）

> 小目标：**写一个独立核对脚本 `tools/verify_stats.py`** —— 只读 `data/request-log.jsonl`，自己算出
> 总量/成功率/token/**按 model**/**按 uid**/**按上游**分组（Top 10）+ 每项标注字段名 + `--json` 输出。
> ⚠ 未改 `internal/server/webui.html`，未改任何 Go 代码，未 commit。

---

## 1 · 做了 / 改了什么

### 改前
* 仓库里**没有** `tools/` 目录（`Test-Path D:\project_GIT\workbuddy2api\tools` → `False`），
  也**没有**任何"绕开接口、直接读 `request-log.jsonl` 自算"的独立核对脚本 ——
  验收要求「数字必须对得上 · 用独立算法核对」，此前缺少这个独立一侧的实现。
* 简报 §3 给的字段示例是**转述版**（`{"model":...,"status":...,"tokens":...,"uid":...}`），
  且明确要求「先自己看真实字段名，别信我转述」。实际真实字段（`Get-Content ... -Tail 3`）：

```json
{"seq":80093,"at":"2026-10-18T04:49:00.6813843+08:00","model":"loomy/DeepSeek-V4.1-Flash","mode":"stream",
 "status":200,"uid":"260915165817517447","provider":"loomy","ttfb_ms":3593,"tokens":260,
 "total_ms":4609,"credit":0,"think_tokens":17,"cache_hit_tokens":0,"cache_miss_tokens":0}
```

⇒ 与转述的差异（**影响上游口径判断**）：真实数据里**已经有独立 `provider` 字段**，
上游**不必**从 `model` 前缀切；另外还有 `seq/at/mode/total_ms/credit/cache_*`。

### 改后
新增**一个**文件：`tools/verify_stats.py`（纯标准库，**只读**、不写盘、不联网）。
关键实现点：

| 需求 | 实现 |
|---|---|
| 1 总量/成功/成功率 | 字段 `status`；成功 = `200 ≤ status < 300`；`rate()` 里 `if not total: return "N/A（分母 0）"` |
| 2 token 总量 | 字段 `tokens`、`think_tokens` 逐行求和；⚠ 另给「剔除负数后」的数（见 §3 脏数据） |
| 3 按 model | 字段 `model`，三列（请求/成功/tokens）同源累加，Top 10（`--top N` 可调） |
| 4 按 uid | 字段 `uid`，同一套累加与排序 |
| 5 按上游 | **方法 A**：`model.split('/',1)[0]` 前缀，**先把 4 个去重前缀全打印**；**方法 B**：独立字段 `provider`（对照），并打印前缀 vs provider 不一致行 |
| 6 字段名 | 段 `[0]` 探针核对每个字段的真实存在率；每段再单独打印「字段名：\`xxx\`」；段 `[6]` 汇总字段清单 |
| 7 `--json` | stdout 只输出一份 JSON（含 `totals/by_model/by_uid/upstream/field_names/status_breakdown`），供机器比对 |
| 排序可复现 | `(-请求数, -tokens, key)` 三级排序，同数不同次运行结果稳定 |
| 降级不抛 | 坏行/非 JSON/非 dict/字段缺失/类型异常都计数跳过，不让整份报表挂（口径 7） |
| Windows 控制台 | `sys.stdout.reconfigure(encoding="utf-8")`（否则 GBK 下 `⚠` 直接 `UnicodeEncodeError`） |

---

## 2 · 怎么跑

```powershell
cd D:\project_GIT\workbuddy2api
python tools\verify_stats.py                 # 人类可读（默认读 data/request-log.jsonl）
python tools\verify_stats.py --json          # 机器可比对 JSON（stdout 只有 JSON）
python tools\verify_stats.py --top 20        # Top N，默认 10
python tools\verify_stats.py --file <日志>    # 指定文件（用于空文件/坏行/小程序样本回归）
```

无参数、无网络、无 pip 依赖；Python 3.13.5 实测。

---

## 3 · 真实输出（数字为真跑结果）

### 3.1 主报表（`python tools\verify_stats.py`，输出存 `data-test/_a2_final.txt`，共 91 行）

```
=== A2 · 独立核对（只读 data/request-log.jsonl，自算，不调接口） ===
文件：D:\project_GIT\workbuddy2api\data\request-log.jsonl
大小：8,023,101 字节 · mtime=2026-10-18 04:50:35
读入行数：28,219（空行 0 · JSON 解析失败 0 · 非对象 0）
⚠ 读取期间文件被追加 286 字节（实例在跑），以下数字对应「读取起点」的快照。

[0] ⚠ 字段名核对（不照抄简报，按真实数据探针）
  - `status`       出现   28,219 /   28,219  ✅   （状态码（成功率分母/分子））
  - `tokens`       出现   28,219 /   28,219  ✅   （token 总量）
  - `think_tokens` 出现   28,219 /   28,219  ✅   （思考 token 总量）
  - `model`        出现   28,219 /   28,219  ✅   （按模型分组 · 同时是上游前缀的来源）
  - `uid`          出现   28,219 /   28,219  ✅   （按账号分组）
  - `provider`     出现   28,219 /   28,219  ✅   （上游（独立字段，用于核对前缀法））
  - `at`           出现   28,219 /   28,219  ✅   （时间戳（本脚本未做按天聚合））
  - `seq`          出现   28,219 /   28,219  ✅   （行序号（用于判断日志是否在实时追加））

[1] 总量与成功率
  字段名：`status`（成功 = 200 ≤ status < 300）
  总请求数：28,219
  成功数  ：24,137
  成功率  ：85.53%
  status 分布（前 10）：
    status=200      24,137
    status=503       4,082

[2] token 总量
  字段名：`tokens` 与 `think_tokens`（逐行求和）
  tokens 总量           ：26,579,751
  think_tokens 总量     ：12,334,177
  ⚠ tokens 原始和里含负数行 4,126 条（脏数据），剔除负数后：26,583,877

[3] 按 model 分组 · Top 10
  字段名：`model`（请求数/成功数/tokens 三列同源）
  #    key                                      请求数       成功数       成功率         tokens think_tokens
  -------------------------------------------------------------------------------------------------
  1    loomy/DeepSeek-V4.1-Flash             25,901    21,824    84.26%     25,139,269   12,004,367
  2    workbuddy/deepseek-v4.1-flash          2,242     2,241    99.96%      1,423,073      329,792
  3    workbuddy/glm-5v-turbo                    55        55   100.00%          5,080            0
  4    loomy/spark-x                             14        14   100.00%         12,311            0
  5    workbuddy-intl/deepseek-v4.1-flash         2         2   100.00%              2            1
  6    -                                          2         0     0.00%             -2            0
  7    codearts/DeepSeek-V3.2                     2         0     0.00%             -2            0
  8    workbuddy-intl/kimi-k3                     1         1   100.00%             20           17
  （去重后的 model 共 8 个）

[4] 按 uid 分组 · Top 10
  字段名：`uid`（账号维度）
  #    key                                      请求数       成功数       成功率         tokens think_tokens
  -------------------------------------------------------------------------------------------------
  1    260825100642697603                     1,160     1,154    99.48%        990,247      458,542
  2    260915005010945783                     1,151       826    71.76%        908,572      393,861
  3    260915174611208423                     1,117     1,112    99.55%      1,025,792      465,663
  4    260915013257535864                     1,101       747    67.85%        941,277      368,649
  5    260915042314217167                     1,080       803    74.35%        969,037      375,493
  6    260919113617575027                     1,068     1,064    99.63%      1,024,730      516,869
  7    260915114648993679                     1,054       998    94.69%        995,373      434,893
  8    260921105518378126                     1,042       714    68.52%        965,362      396,156
  9    261005204414552894                     1,012       669    66.11%        778,345      312,451
  10   260914005508637663                       963       689    71.55%        940,894      461,410
  （去重后的 uid 共 55 个）

[5] 按上游分组
  方法 A（简报口径）：`model` 的 'provider/name' 前缀 · split('/',1)[0]
  ⚠ 消除歧义前先列出全部去重前缀（判断这招是否可靠）：
    loomy                请求   25,915  成功   21,838     84.27%  tokens     25,151,580  （占全部 91.84%）
    workbuddy            请求    2,297  成功    2,296     99.96%  tokens      1,428,153  （占全部 8.14%）
    workbuddy-intl       请求        3  成功        3    100.00%  tokens             22  （占全部 0.01%）
    codearts             请求        2  成功        0      0.00%  tokens             -2  （占全部 0.01%）
  ⚠ 前缀法失效行数：2 / 28,219
      - 无 '/' 前缀（model='-'） × 2

  方法 B（对照）：独立字段 `provider`
    loomy                请求   25,915  成功   21,838     84.27%  tokens     25,151,580
    workbuddy            请求    2,299  成功    2,296     99.87%  tokens      1,428,151
    workbuddy-intl       请求        3  成功        3    100.00%  tokens             22
    codearts             请求        2  成功        0      0.00%  tokens             -2

  前缀法 vs provider 字段 一致性：
    ✅ 前缀法落在桶内的记录，前缀与 `provider` 字段 100% 一致
    ⇒ 但仍有 2 行无可用前缀（见上），这部分只能靠 `provider` 字段兜底
    结论：⚠ 前缀不完全可靠（有 2 行取不到），上游应以 `provider` 字段为准，前缀法只作兜底

[6] 本脚本用到的字段名清单（机器可读同见 --json）
  总请求数/成功率 → `status`      token 总量 → `tokens`     think 总量 → `think_tokens`
  按模型 → `model`              按账号 → `uid`
  按上游(方法A) → `model` 前缀   按上游(方法B) → `provider`
  时间 → `at`（本脚本不做按天聚合）
```

**上游前缀到底可靠吗（结论 + 证据）**：
* 前缀落在桶内的 **28,217 行**，前缀与 `provider` 字段 **100% 一致**（不一致计数为 0）⇒ 前缀本身没切错。
* 但有 **2 行 `model` 是 `"-"`（无 `/`）**，前缀法取不到 → 归到「未知前缀」而被**漏计**：
  前缀法 `workbuddy` = 2,297，`provider` 字段 = 2,299（差正好这 2 行，均为 `status=503`、`tokens=-1`）。
* ⇒ **上游应以独立 `provider` 字段为准（覆盖 100% 行）；前缀法只作兜底**。
  单靠前缀会使 workbuddy 上游**少 2 次请求 / 2 个错误**，成功率被高估（99.96% vs 99.87%）。
* ⚠ 另外「前缀 = 上游」在本数据里成立的前提是 `model` 命名始终是 `provider/name`；
  这**依赖上游命名习惯**，不是结构保证 —— 上面那 2 行就是反例。

**tokens 脏数据（新发现，接口一侧需注意）**：
* `tokens = -1` 的行共 **4,126** 条，取值全为 `-1`；其中 `status=503` **4,082** 条，
  另 **44 条 `status=200`（成功却带 `tokens=-1`）**。
* ⇒ 直接求和会被这 4,126 个 `-1` 拉低（原始和 26,579,751 vs 剔除后 26,583,877，差 4,126）。
  报表若用原始和，两处口径必须一致（口径 6）。

### 3.2 双算法一致性（⚠ 关键验收：数字必须对得上）

为避免"日志在跑、两次读数不同"的干扰，先把日志**冻结成快照**再比对：

```powershell
Copy-Item data\request-log.jsonl data-test\_a2_snapshot.jsonl -Force
python tools\verify_stats.py --file data-test\_a2_snapshot.jsonl --json > data-test\_a2_snap.json
# 另用一份独立写法（Counter/生成器，另一份代码）在同一个快照上重算
```

真实输出（`data-test/_a2_cross.txt`）：

```
== 冻结快照上的双算法比对 ==
脚本 : 总=28224 成功=24142 率=85.5371 tokens=26584099 think=12336155
独立 : 总=28224 成功=24142 率=85.5371 tokens=26584099 think=12336155
脚本 top1 model=loomy/DeepSeek-V4.1-Flash req=25906  |  独立 top1 model=loomy/DeepSeek-V4.1-Flash req=25906
脚本 top1 uid=260825100642697603 req=1160    |  独立 top1 uid=260825100642697603 req=1160
脚本 top1 prefix=loomy req=25920 |  独立 top1 prefix=loomy req=25920
脚本 top1 provider=loomy req=25920 | 独立 top1 provider=loomy req=25920
```

⇒ 五个核心数与四个维度的 Top1 **逐项一致**。

### 3.3 边界回归（各用 `--file` 指向临时样本，未动真实数据）

| 场景 | 命令 | 真实输出 |
|---|---|---|
| **空文件（分母 0）** | `--file %TEMP%\a2_empty.jsonl` | `总请求数：0` / `成功数 ：0` / `成功率  ：N/A（分母 0）`（不抛异常，退出码 0） |
| 坏行文件（1 空行 + 2 坏 JSON） | `--file %TEMP%\a2_bad.jsonl` | `读入行数：3（空行 1 · JSON 解析失败 2 · 非对象 0）`，`成功率：N/A（分母 0）` |
| 含无前缀 model 的小样本 | `--file %TEMP%\a2_tiny.jsonl --top 3` | `成功率：66.67%`（2/3）；`⚠ 前缀法失效行数：1 / 3`；`结论：⚠ 前缀不完全可靠…应以 provider 字段为准` |
| 文件不存在 | `--file %TEMP%\a2_nope.jsonl` | `⚠ 找不到日志文件：D:\temp\a2_nope.jsonl`，`exit=2` |
| `--json` 空文件 | 同上加 `--json` | `"requests": 0, "success_rate": null, "tokens_sum": 0`（JSON 合法，`null` 而非 NaN） |
| `--json` 真实数据 | `--json > data-test\_a2_final.json` | JSON 可 `json.load` 解析，键含 `totals/by_model/by_uid/upstream/field_names/status_breakdown/seq_range` |

### 3.4 未越界证据

```
$ git status --porcelain
?? tools/          # 只有新增的 tools/ 目录（未 commit）
```
`internal/server/webui.html` 与所有 `.go` 文件**零改动**；未启动任何服务、未碰 7863/3080/7864。

---

## 4 · ⚠ 哪些没验

1. ⚠ **没有和 `GET /admin/stats` 的返回值比对过** —— 本小目标只要求"独立算法自算"，
   我**没有起 7864、没调接口**，所以「接口 `by_model` 与本脚本 `by_model` 数字是否一致」**未验**。
   这正是下一步（有人做接口侧时才成立）。
2. ⚠ **本脚本不做按天聚合（`at` 只用于展示存在率）** —— 简报 §3 说天维度"本批可以不动"，
   故 `DailyEntry` 的补零/activeDays/区间反向/循环上限（口径 2/3/4/5）**全部未涉及**。
3. ⚠ **日志是活的**：`request-log.jsonl` 每几秒被 7863 追加（本次读取期间 +286 字节），
   所以**任何两次运行的绝对数字都不同**（28,211 → 28,219 → 28,224）。
   上表数字只对「该次读取起点的快照」成立；脚本会打印那条 `⚠ 读取期间文件被追加 N 字节` 提示。
   机器比对应固定用 `--file` 指向同一份快照，别直接比两次实时运行。
4. ⚠ **`tokens=-1` 的语义未查清**：只知道 44 条是 `status=200`、4,082 条是 `status=503`，
   但**"成功响应为什么 token 记 -1"** 没有去读上游/写日志的代码确认（属接口侧，超出本小目标）。
   脚本同时给「原始和」与「剔除负数后」，口径选择留给报表侧拍。
5. ⚠ **没有对超大文件的性能做保证**：单遍流式读取，28k 行/8MB 实测秒级；
   但**百万行级别未测**，也没有内存峰值数据。
6. ⚠ **没有校验 `uid` 与账号的映射**：`uid` 在部分行里像 UUID（`0234c548-…`）、
   部分是纯数字（`260913075224031074`），**两种形态是否同一账号体系未确认**；
   Top 榜按字符串精确分组，形态不同即视为不同账号。
7. ⚠ **`mode`（stream/sync）、`credit`、`cache_hit_tokens` 等字段本脚本未聚合**，
   报表若要"缓存命中率"或"按模式分桶"需另行补。
8. ⚠ 未做**交叉验证的第三种算法**（如 `awk` 版）：本次只有脚本 + 一份独立 Python 重算，
   两者共用 `json` 解析这一共同前提。
