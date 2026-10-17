# R2 · 对抗性复核（边界与口径）—— 报表区 `renderStatsReport` / `trendRange`

> 角色：对抗性复核者（**推翻**优先）。复核对象：`internal/server/webui.html`（只读片段）
> + 其数据源 `internal/admin/admin.go` `aggregateChatLog` / `series.go`（只读）。
> ⚠ **未改任何源码、未 commit**。7864 自起自停（§0）。

## 0 · 环境与复现

* 7864：**我自己**起 `bin\wb2api-server.exe -config config-7864.json`（pid 29212，隔离目录 auths-7864/data-7864），
  跑完 `Stop-Process`，收尾实测 `7864 cleaned = True` · `7863 未触碰 = True`。
* ⚠ 两个坑，先说，因为它们差点让我交出错结论：
  1. 我先发现 7864 已有**别的代理起的**实例（pid 12932，5:15:25），它在我测到一半时消失（**不是我杀的**，我从未 kill 它）
     ⇒ 第一版 A7/C/D/E 段的 DOM 是"上一条 payload 的残留"，那批结论**全部作废**，已重做（v2 每条 payload 一次 reload）。
  2. `.r2run.ps1` 用 LF 行尾会被 Windows PowerShell 5.1 解析崩（`Unexpected token`）⇒ 必须用 pwsh 7 跑。
* 探针（可复跑）：`docs/report/R2-probe/r2probe.js` + `r2run.ps1`，原始输出 `docs/report/R2-probe/r2probe.out.txt`。
  Playwright 1.63（`D:\project_GIT\test\.pwtools\node_modules`），浏览器用 `chromium-1200\chrome-win64\chrome.exe`
  显式 `executablePath`（1.63 默认要 1243，本机没装）。
  打法：`page.route` 只 stub `/admin/stats` 与 `/admin/stats/series`，其余走真服务；
  每条 payload 一次 `reload` 落定，页内重渲染则等 `#refresh:not([disabled])` 再点。

---

## 面 1 · 零除 / 离群值（空 · 全 0 · 单键 · 负数）

**结论：不成立**（无 `NaN` / `Infinity` / `undefined` / 空串逃逸）。**但有两处措辞级的假话（成立）**。

依据（代码）
* `calls()` 7829-7833 → `numNonNeg`（7658-7677：非有限数/负数 ⇒ `null`）⇒ NaN/Infinity 在入口就被吃掉。
* Top 榜：7841 `uidMax`、7846 `uidMax > 0 ? … : 0`、7847 `uidSum > 0 ? … : '—'`；环形：7864 `!provs.length || psum <= 0` 早退。
* 趋势：8781 `Math.max(1, …)`、8782 `p.total ? … : null`。

依据（实测，5 组构造数据，全部无 JS 异常）
* 空：`topRaw="暂无数据" donutCenter="" donutBg="" hint=""`（**无 NaN/Infinity**）。
* 单键：`by_uid{a1:5}` → `bar=100% val=5 pct=100.0%`；`by_provider{loomy:26012}`（**裸数字老形状**）→ 环 `26012请求数 / 100.0%`。
* 全 0：`by_uid{a1:0,b2:0} by_provider{p1:0}` → 行 `{"bar":"0%","val":"0","pct":"—%"}` ×2。
* 负数：`by_uid{neg:-5}` `by_provider{neg:-3}` → `neg` 被归 0，`val=0 pct=0.0%`，环仍按 `good:4` 画（**不炸、不出现负数**）。

两处成立（轻微，但确实是假话/丑话）
1. **`—%`**：占位符 `NODATA='—'` 与 7854 的 `${pct}%` 直接拼接 ⇒ 实测 `"pct":"—%"`（两个键都是）。应为 `—` 或 `0%`。
2. **0 也画条**：7846 `Math.max(2, …)` 保底 2% ⇒ 实测 `{"name":"b2","bar":"2%","val":"0"}`、`neg` 同；
   数值 0 却有一根可见条形，视觉上像"有量"。建议 0 直接给 `width:0`。

⚠ 我没验：真实后端产不出「by_uid 全 0 / 负数」（`admin.go:2841-2843` 只在 uid 非空时建键、每键 ≥1），
所以上面两条是**渲染层**行为，不等于"线上已发生"；也没截真机截图（证据是 DOM 快照，见 `shots/R2-zero-state.png`）。

## 面 2 · 形状兼容（裸数字 vs `{calls,ok,fail,credit}` vs 更深嵌套）

**结论：部分成立 —— `calls()` 只认 `calls` 一个键，认不出就静默 0，且 `by_uid` 分支会渲染出「0 / —%」的假行。**

依据（代码）
* 7829-7833：`const raw = (v && typeof v === 'object') ? v.calls : v;` —— **只有一条键名通路**。
* 后端真形状：`providerStat{Calls json:"calls"}`（`admin.go:2720-2725`）⇒ 今天 `by_provider` 靠 `calls` 是对的；
  `by_uid` 是 `map[string]int`（`admin.go:2823/2841`）⇒ 裸数字也对。**当前两种形状都被兼容**。

依据（实测）
* A5：`by_provider{p_req:{req:7,…}, p_calls:{calls:4}}` → `legend = [p_calls=4, **p_req=0**]`；
  `by_uid{u1:{calls:9}, u2:{req:7}}` → `u1=9, **u2=0**`（`bar=2% pct=0.0%`）。
* A6（更深嵌套）：`{calls:{n:7}}` → `by_uid` 渲染出假行 `u1 0 —%`；`by_provider` → 环清空成"暂无数据"。
  ⇒ **同一形状，一处静默 0 + 假行、一处"暂无数据"，两处口径不一致。**

⚠ 直指你们下一步：后端富化版的字段名是 **`req`**（`dimStat{Req json:"req"}`，`admin.go:2756-2763`；
`by_uid_detail` / `by_provider_detail` 都是这套），而 7816 的注释写着「后端正在补带 tokens 的富化版；届时只换这里的取值」——
**若真按那行注释把取值换成 detail，`calls` 立刻全 0**（本次 A5 就是这个结果，而且不报错）。
建议：`calls()` 加 `v.req`/`v.n` 兜底，或在换 detail 时同步改键名 + 加一条"和 < 期望值就 throw"的断言。

⚠ 我没验：后端富化版最终会被前端接哪一支（A 代理的活还没合并到消费点）；也没验 `credit` 字段在报表区的任何消费（当前确实没用）。

## 面 3 · 「界面说假话」逐句过

| # | 界面上那句话 | 结论 | 依据 |
|---|---|---|---|
| 1 | 面板标题「Top 账号 / 上游占比 **按请求数**」 | 不成立 | 与后端口径一致：`calls` = 请求数（`admin.go:2862 ps.Calls++`）；环心 7899 写死「请求数」，实测 `26012请求数` |
| 2 | 趋势 caption「24 小时 · 逐小时 / 7 天 · 逐日」 | 不成立 | `series.go:89-96` `7d→bucket=24h`、`24h→bucket=1h`，确实逐日/逐小时 |
| 3 | 同一句 caption 在竞态下 | **成立（最严重）** | 见面 4：caption 读**全局** `trendRange`（8802），画面可能是**另一个 range 的数据** |
| 4 | 按钮 `24h/7d` 与卡片/Top 榜/环的关系 | **成立（措辞）** | 实测切 range 后 `卡片同= true | 榜行数 1 → 1`（确实只切趋势，符合代码注释）；**但 DOM 里没有「趋势范围」这个可见标签**：实测 h3 = `调用统计 （…） （趋势：请求数 + 成功率） 24h 7d` —— 663-667 注释里那句「标签只能叫趋势范围」只写在注释里，**没落到界面上**。按钮就贴在「调用统计」标题行内，用户读成"整块统计的范围"完全合理 |
| 5 | 卡片 30s 无感更新 vs Top 榜/环 | **成立（两速）** | 9167-9172 只调 `pollPoolValues/pollStatsCards/loadTrendSeries/pollAPIKeys`，**没有** `renderStatsReport` ⇒ 榜/环只随整页 refresh。实测 D：换桩后等 28s `卡片= 总请求=99`，而 `榜行= ["a1"]`、`hint=「共 1 个账号有请求」`、`环=5请求数` **全是旧值** ⇒ 同一块「调用统计」面板里两个数字互相矛盾 |
| 6 | 「共 N 个账号有请求」 | **成立（早退导致陈旧）** | 7864-7869 提前 `return`，跳过了 7892-7894 的 hint 更新。实测 A7：先 `by_uid{a1:5}`（hint=`共 1 个账号有请求`），页内重渲染 `by_uid{a1:5,b2:5}` + `by_provider{}` ⇒ `rows=2` 而 hint 仍是 **`共 1 个账号有请求`**；A3 也印证（2 个键、`hint=""`） |
| 7 | 趋势「最新 MM-DD **00:00**」 | 不成立但无用 | 7d 天天桶按天 Truncate，`label = ts.slice(11,16)` 恒为 `00:00`（8787） |

⚠ 我没验：真机上"用户会以为 Top 榜跟着 range 变"是推测（我用 h3 文案 + 切 range 前后数值不变做的判据），没有真人可用性测试。

## 面 4 · 切换 range（24h→7d→24h）

**结论：无序号守卫 ⇒ 成立（迟到的旧 range 响应覆盖新 range 的图，caption 与按钮同时说谎）。**

依据（代码）
* 8750-8760：`if (b.dataset.range === trendRange) return;`（点当前项不重取 ✓）、`trendSeries._last = ''`（清 digest ✓）、
  `try { loadTrendSeries(); } catch {}`（**不 await、无代际序号**）。
* 8762-8774：`loadTrendSeries` 用**全局** `trendRange` 拼 URL 与 digest，**没有**把本次 range 捕获成局部量，
  也没有"只认最后一次请求"的守卫 ⇒ 两次并发请求谁后到谁覆盖。

依据（实测：7d 响应延迟 2500ms，中途点回 24h）
* 点回 24h 后 0.4s：`最近 24 小时 · 逐小时 … 最新 10-18 05:00 · 请求 7 | L= 24`（正常）
* 7d 迟到落地后：`最近 24 小时 · 逐小时 … 最新 10-18 00:00 · 请求 **999999** | L= 7 | 按钮= ["24h*","7d"]`
  ⇒ 画面是 **8 个天桶**（L=7）的 7d 数据 + 999999（我塞的 7d 标记值），而 **caption 说 24 小时逐小时、按钮高亮 24h**。
* 「请求发了图没变」：**本轮未观测到**（digest 前缀带 range + 切 range 清 `_last`，判据是对的）；
  观测到的是**动了但动错**（比不动更糟）。
* 另一个实测到的连点吞：整页刷新按钮走 `busyRun`（3296-3304 `if (btn._busy) return undefined`），
  刷新期间再点会被**静默丢弃** —— 我的第一版探针就栽在这上面（payload 没生效，DOM 停在上一轮），
  这是**测试陷阱**，不是本次缺陷；但顺手记一条：报表区**唯一**的更新入口是个会被吞掉的按钮，且整页 refresh 在本机实测 7s+（8920 注释）。

⚠ 我没验：range 按钮（不走 busyRun）与 `refresh()` 收尾那行 `loadTrendSeries()`（8931）并发时的时序；也没验手动改 URL/多标签页共用。

## 面 5 · DOM 缺失（老版本页面）

**结论：不成立（守卫齐全）。代价：三者缺一，Top 榜与环**一起**不画。**

依据
* 7821-7822 一次取三个容器并判空即整块 `return`；7893 `if (hint)`；8777 `if (!host) return`；8778-8779 空序列降级；
  8750 `querySelectorAll('#trendRange button')` 空集只是循环 0 次。
* 实测 E：删掉 `#statsTopUid/#statsDonut/#statsDonutLegend/#statsTopHint/#trendRange/#statsTrend` 后点刷新
  ⇒ **新增 JS 异常 0 个**（新增的 2 条 console 是我这个实例 `/healthz` **503**（池里没号）的网络错误，不是 JS 异常）。
* 副作用（轻微）：`#statsDonut` 缺失会让 Top 榜也一起不画（返回点在两个区块之前）——老页面丢的只是"新增内容"，不误导。

⚠ 我没验：真·老版本 `webui.html`（旧 JS + 无新 id）的组合——我只有当前工作区一份，做的是"同一份 JS + 运行期删元素"。

## 面 6 · `esc()` 覆盖 / XSS 面

**结论：机制成立（已复现）——`esc` 只转义 `& < >`，不转义 `"`，而 uid / provider 名被插进**双引号属性**；
可达性**未证实**（今天不由 HTTP 客户端控制）⇒ 记为纵深防御缺口，不是现成 RCE。数值插值不成立。**

依据（代码）
* 1232-1233 `esc`：仅 `&`→`&amp;`、`<`、`>`。**没有** `"` / `'`。
* 属性上下文两处：7850 `` title="${esc(uid)}" ``、7880 `` title="${esc(name)}" ``；
  同文件 7942（模型 chip）`` data-id="${esc(name)}" `` 也是这个模式。
* 数值插值 `${n} ${w}% ${pct}%`（7852-7854）与 legend 的 `${n} ${pct}`（7883-7884）全部来自 `Math.round/toFixed` 的 **number** ⇒ 不可注入。

依据（实测 B：uid = `x" onmouseover="window.__XSS1=1`，provider = `y" onclick="window.__XSS3=1`）
* 实际渲染出：`<div class="toprow" title="x" onmouseover="window.__XSS1=1">`（快照 `attrs:["onmouseover"]`，`onmouseoverEls: 1`）
* legend 同理拿到 `onclick`（`onclickEls: 1`）——**双引号逃逸成立，且是真能被浏览器解析成事件处理器的属性**。
* 标签注入被挡住：uid 里的 `<img src=x onerror=…>` → `imgEls: 0`（`<` 被转义），只多出一个无害的属性名。
  ⇒ 洞只在**属性上下文里的 `"`**，不在 HTML 文本上下文。

可达性（我的判断，附反向证据）
* `uid`：来自凭证目录（`internal/auth` / `*creds.go` 的 `UID:`），不由 HTTP 客户端控制。
* `provider`：`h.providerFor(peek.Model)` 解析出的**注册表 ID**（`handler.go:1309-1313`，**未知前缀直接 400**）⇒ 也不可控。
* 所以当前是"需要本地凭证/配置里出现 `"` 才触发"。修法一行：`esc` 增 `"`→`&quot;`、`'`→`&#39;`（会影响全站所有 `${esc(x)}` 插值点，需一起看）。
* ⚠ **我没验（建议另派只读探针）**：`renderStatsModels` 7942 把 **model 名**写进 `data-id="${esc(name)}"`，
  而 model 名**是客户端可控字符串**（`model:"loomy/<任意>"`，前缀已知即过 400，日志行在出口必写、`logging.go:262-268`）
  ⇒ 这条链比 uid/provider 短得多，**很可能真能打通**，但那属于"模型明细"面板，不在我这次的报表区范围内，我没有构造那条日志行。

---

## 结论摘要（给主脑的取舍建议）

1. 必须修（有实测、会误导）：**面 4 竞态**（caption/按钮与画面不一致）、**面 3-#5 两速**（卡片动了榜不动）、
   **面 3-#6 hint 早退陈旧**、**面 1 的 `—%` 与 0 也画 2% 条**。
2. 换 detail 前必须改（否则静默 0）：**面 2** —— `calls()` 只认 `calls`，富化版字段是 `req`。
3. 界面文案：**面 3-#4** —— 把「趋势范围」这个字样真正画到界面上（或把按钮挪进趋势图容器）。
4. 纵深防御（不是今天就能打）：**面 6** —— `esc` 补 `"` / `'`；连带查 7942 的 model 名链。
5. 不需要动：面 1 的防零除主体、面 5 的 DOM 守卫 —— 实测都成立。

## ⚠ 本次没验的（诚实清单）

* 真机（7863）真实数据的数字正确性 —— 不是我的面，且我没做独立求和比对。
* `esc` 缺口的**可达性**（uid/provider 含 `"` 的生产路径）；7942 model 名链**整条未验**。
* 真·老版本 `webui.html` 组合；多标签页 / 手动改 URL / range 与 `refresh()` 并发时序。
* 移动端/窄屏下 `.duo` 退化与条形最小 2% 的可读性（只看代码，没看渲染）。
* 面 1/2 的"异常数据"都是我用 stub 构造的 —— 作用是证明**渲染层**行为，不能当"线上已发生"。
