# v2.0.0 — 2026-11-09

> 格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。
> 次版本号从 1 提到 2：这一版改了**日志统计的存储层**（新增 `log_daily` 表）与
> **前端资源的加载形态**（CSS/JS 改为外置文件）。无破坏性变更，但升级后形态确实变了。

本版三件大事：**第十四上游 Codar 接入**、**签到统一到一处**、
**热力图终于有 365 天数据**。

---

## 新增

### Codar（科搭 / SpeedAI）—— 第十四家上游

协议全部来自 2026-11-08 的真机实测，不是推断。一个上游跑两套协议：

| 模型族 | 通道 | 鉴权 |
|---|---|---|
| openai 系 | `POST /v1/chat/completions` | `Authorization: Bearer <token>` |
| claude 系（4 个） | `POST /v1/messages` | `x-api-key` |

- 请求体**字节级透传**（保留客户端的扩展字段与数字精度），只强制 `stream:true`
  并按上游上界收敛 `max_tokens`。
- SSE 必须带 `Accept-Encoding: identity` —— 压缩流会坏帧。
- 模型目录 `GET /v1/codar/models` **免鉴权**，目录刷新不需要凭证。
- 控制台：余额（三桶口径）/ 今日签到 / 邀请码绑定 / 免费模型池 / 令牌自动续期。
- 登录方式：手机号 + 短信验证码（含阿里云 captcha 与 IP 二次验证）、微信授权、
  桌面端 `~/.codar/config.json` 直接导入。
- 已在 `config.example.json` 里给出可复制的配置段。

### 统一签到（跨上游）

改造前 8 个上游都实现了签到能力，但**只有 2 个**（trae / workbuddy）注册了签到任务
⇒ 另外 6 个的自动签到**静默失效**，用户看到的现象是"只能手动签"。

- 一处注册 `core-daily-checkin`，运行时遍历注册表发现所有实现者
  ⇒ 以后新增上游只要实现接口就自动被管，**零注册代码**。
- **幂等由调度侧保证**：今天已成功扫过的上游，连 `CheckinAll` 都不调
  （不是"调了内部跳过"）。上游面板明确写着"已有风控记录，请低频使用"，
  所以不能指望上游自己幂等。
- 重试下限 30 分钟：`Due` 对失败上游恒为 true 会让它每个调度 tick 都重试 ——
  实测 8 分钟里扫了 12 轮、约 4300 次请求/天。
- 凭据类签到失败 ⇒ 账号自动禁用；网络抖动 / 上游维护**不触发**
  （那会把好号踢下线）。启用后仍可用「今日签到」列核对每个号的结果。

### 365 天热力图：日级聚合 `log_daily`

- 明细日志 `logs` 按保留期裁剪（排障用），日聚合 `log_daily` **永不裁剪**。
  体量：原始日志 400+ MB/年 vs 日聚合 ≈ 43 KB/年。
- 7d / 30d / 365d 天桶改读 `log_daily`，响应里 `source: "log_daily"`；
  24h 小时桶仍走明细（一天一行装不下小时），日聚合为空时自动回落。
- 服务启动时幂等回填（覆盖式取 `logs` 的真相，不是相加），升级后历史格子会自己亮起来。

---

## 变更

### 请求体上限默认 8 MiB → 64 MiB

8 MiB 时一张 4K 截图 base64 后就可能越界，而用户只看到一句 413、
**看不出是自己图太大**。64 MiB 够 8–10 张 4K 图或一份 200k token 的长上下文。
写**负数**（如 `-1`）= 不限制，上限交给反代/防火墙。

```json
"server": { "max_body_mb": 64 }
```

### 控制台 CSS/JS 外置

`webui.html` 从 1.5 万行降到千行级，拆出 `/ui/app.css` 与 `/ui/app.js`
（ETag + `max-age=300, must-revalidate`）。
⚠ 已知取舍：改一次 CSS/JS 需等 5 分钟或强刷才生效（还没做内容哈希文件名）。

### 额度刷新并发 20

原来完全串行：106 个账号约 106 秒 → 现在约 6 秒。
三条路径（手动单上游 / 手动全部 / 后台分层刷新）共用同一上界。
不再往上加的理由：codar 面板写着"低频使用"、loomy 有 45 个号 —— 20 是
"明显变快"与"不给上游压力"之间的折中。

### 其他

- **趋势图不再裁掉尾部空桶**：右端恒等于"现在"。原来最近几小时没请求时，
  最后一格会退到 17 小时前，看着像"时间是 05:00–12:00"。
- 日志页**状态码筛选下推到服务端**（原来只筛当前页 30 条，翻页后 chip 上的数字就变），
  同时给出全库状态计数，与筛选结果逐条对齐。
- 服务状态页把 `warnings`（影响 verdict）与 `notes`（配置事实）**分开显示**。
  原来混在一起同色渲染，实测让人把"Redis 未启用"当成整页 critical 的原因，
  而真凶是磁盘 97%。
- 完整凭据响应统一 `Cache-Control: no-store, no-cache, must-revalidate, private`
  （此前 `/admin/apikeys` 只有 `no-store`，与令牌页不一致）。
- Playwright profile 不再落到系统临时目录（实测每个约 525 MB、攒了 24 个 ≈ 12 GB），
  并加"进程启动时长"的孤儿判定 —— mtime 判不了这件事，孤儿会自己刷新 mtime。
- 共享件收敛：`ExpiryFromMillisOrJWT` / `ContainsAny` / `ShortUID` /
  `NormalizeBodyForStreaming` / 手机号登录页模板各只留一处实现。

---

## 修复

### Codar 的注册块寄生在 MonkeyCode 的条件里（本版自己的缺陷）

`registry.Register(cd)` 整段被误放进 `if mk != nil && cfg.MonkeyCodePoolAccounts` 内部
⇒ monkeycode 关着（或 `pool_accounts=false`）时 codar **完全不注册**：
注册表里没有它、日志里连一句"已启用"都不打，用户看到的现象是
"这版新增的上游根本不存在"。

已移到函数顶层，并加源码级守卫 `TestCodarRegistrationIsTopLevel`
（嵌套 / 块缺失两种错法都实测会转红）。

### CI 的 gofmt 门禁会红，Release 会一个资产都不发

90 个文件的**仓库字节**不符合 gofmt（工作树的 CRLF 让 `git diff` 看不见，
本地怎么查都以为没问题）。打 tag 后 release 会卡在 verify，
构建与资产上传一步都不会跑。已按"仓库里存的字节"校验并逐字节写入索引，现为 0。

### `TestBackfillLogDailyIsIdempotent` 是时间敏感的 flaky

基准取"三天前此刻"再加 0–4 小时，本地时间 20:00 之后必然跨自然日，
断言"1 天"就红（CI 跑 UTC 时恰好不红）。改为锚定当日零点。

### 其他

- 同名模型被跨渠道去重吞掉 —— 渠道声明的 8 个模型在目录里只剩 3 个。
- 候选渠道超时不换人 —— 卡死一条就把整个请求打死。
- 渠道页「从上游拉取」必失败 —— 前端发 `keys`，后端只认 `key`。
- 7 页 form-labels 清零，顺带修掉 3 个真 bug（重复 id / 缺 viewport）。
- claude 系不传 `max_tokens` 时补模型输出上限（原来写死 4096，长输出被静默截断）。

---

## 验证

| 检查 | 结果 |
|---|---|
| `go build ./...` / `go vet ./...` | 通过 |
| `gofmt`（按仓库 blob 字节，等价 CI 判据） | **0** 个文件 |
| `go test -short ./...` | **50** 个包全部通过，0 失败 |
| `go test ./...`（全量，含网络/进程类用例） | **50** 个包全部通过，0 失败 |
| Codar 注册守卫（变异验证） | 嵌套 / 缺失两种错法均成功转红，恢复后转绿 |

---

## 升级

无破坏性变更，替换二进制后重启即可。首次启动会自动建 `log_daily` 表并回填历史。

- **Docker**：`docker compose up -d --build`
- **裸机**：替换 `wb2api-server` 后重启进程

想用 Codar 的话：

1. 控制台「账号池」→ Codar → 「＋ 添加账号」（手机号 / 微信 / 导入桌面端配置）；
2. 无需改 `config.json` —— codar 段可省略，凭证目录默认 `auths/codar`；
3. 可选：`"codar": { "api_base": "", "pool_accounts": true }`。

---

## 已知问题

- 控制台外置资源的缓存策略还是简单版（`max-age=300`）：改 CSS/JS 后需强刷
  或等 5 分钟，还没用内容哈希文件名。
- `log_daily` 只做日粒度：24h 档仍受明细日志保留期约束，保留期外的历史小时数据不可恢复。
