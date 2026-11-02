# v1.12.1

**修掉一个会让整台机器卡死的真缺陷：loomy 额度查询失败时退避从未生效，
导致 `drainRecheck` 陷入无界死循环。**

---

## 事故现象

用户报「服务器 CPU 近乎 100%、卡爆」，重启后数小时**复发**（当天重启两次）。

真机实测：

| 指标 | 数值 |
|---|---|
| `wb2api` CPU | `%usr=61.96 + %system=28.11 = **90.07%**`（单核），峰值 124% |
| 全机 CPU | `%Cpu(s): 67.6 us, 14.7 sy, **11.8 id**` |
| `%st`（steal） | 0.0 → **排除**宿主机争抢，是本机进程真吃 |
| 日志速率 | **1187 行/秒**、15 MB/分钟、≈**900 MB/小时** |
| 日志构成 | 取样 3000 行里 **2999 行（99.97%）** 是同一对错误 |
| 已累积日志 | **8.9 GB**（占 Docker 日志总量 9.0 GB 的 96%）→ 系统盘 80% |
| 连锁表现 | 持续磁盘写 I/O → 整机卡死（用户体感）

## 根因：`noteFail` 定义了却从未被调用

`internal/loomy/freepool.go` 的退避写入函数**全仓库零调用点**：

    $ grep -rn 'noteFail' --include='*.go' internal/ cmd/
    internal/loomy/freepool.go:224:  // noteFail 记一次查询失败（失败退避）。
    internal/loomy/freepool.go:231:  func (f *freePool) noteFail(uid string, now time.Time) {

`f.failAt[uid]` 的**唯一写入点**就在它内部，于是：

    失败路径从不写 failAt（也不写 checkedAt）
      -> dueForRecheck 的两道退避闸门恒不成立
        -> 失败账号每一拍都被判定「该重查」
          -> drainRecheck 的裸 for {} 永不退出
            -> 无限重试 + 每失败打 2 行日志
              -> 1187 行/秒 + 90% 单核

`noteFail` 自己的注释写着「**不删 needRecheck 标记** …… 退避由 `freeFailBackoff`
控制（`dueForRecheck` 里判），标记要留着」—— 设计意图明确就是失败时要调它，**实现漏了**。

**放大因素**：`drainRecheck` 在 `candidate_tier.go:278`（`go` 起来）与 `freepool.go:358`
被多处启动 ⇒ 多个死循环并发；失败日志无限流；`.gitignore` 之外，
`/etc/docker/daemon.json` 也没有 `log-opts` ⇒ 日志无上限。

**触发条件**：8 个 loomy 账号 session 失效（HTTP 200 + `code=100002` `登录已失效`）。
注意 `shortUID()` 只取前 8 位，日志里它们都显示成 `uid=26092700` ——
看起来像"同一个号被刷 20 遍"，会误导排查方向。

## 修复

**1. 根治 —— 失败分支补上退避调用**（`internal/loomy/quota_ext.go`）

    p.pool().noteFail(uid, time.Now())

**2. 护栏 —— `drainRecheck` 加轮次上限**（`internal/loomy/freepool.go`）

新增 `freePoolDrainMaxRounds = 50`，并在触顶时**留一条日志**。
理由：这次的教训是"无界循环 + 静默" —— 退避一旦再被漏掉，至少要**有限**且**可见**。

**3. 日志限流 —— 同一 uid 的失败日志 5 分钟最多一条**（`logQuotaFail` + `sync.Map`）

第二道闸，防的是将来又出现别的高频失败路径。

**4. 部署侧加固**

- `docker-compose.yml` 给 `wb2api` 加 `logging` 上限（`max-size: 50m` × `max-file: 3`）
- 新增 `/etc/cron.d/docker-log-cap`：每 5 分钟截断超 200 MB 的容器日志（兜底，覆盖所有容器）
- 新增 `/etc/cron.d/wb2api-health` + `wb2api-healthcheck.sh`：
  每 5 分钟记录 CPU / 日志 / 磁盘，异常打 `WARN` ——
  此前**没有任何监控**，全靠用户发现"卡爆"才暴露

## 验证

- `go build ./...` exit 0
- `go vet ./...` exit 0
- `go test -short ./... -count=1` => **48 包 ok / 0 FAIL**
- `gofmt -l` 对改动文件无输出

**真机部署后当场复测**（巡检日志自证，18:05:14 部署）：

    2026-11-02 17:50:29 WARN cpu=171.4% log=205MB   |  修复前
    2026-11-02 17:55:07 WARN cpu=176.6% log=1MB     |
    2026-11-02 18:00:07 WARN cpu=103.4% log=72MB    |
    2026-11-02 18:05:06 WARN cpu=139.8% log=157MB   |  ← 18:05:14 部署
    2026-11-02 18:08:29 OK   cpu=0.8%  log=0MB      |  修复后

| 指标 | 修复前 | 修复后 | 改善 |
|---|---:|---:|---:|
| 日志速率 | 1187 行/秒 | **0.63 行/秒** | ↓ 99.95% |
| `wb2api` CPU | 90 ~ 124% | **3.27%** | ↓ 96% |
| 全机 `%idle` | 11.8% | **90.6%** | ↑ 7.7 倍 |
| 日志文件 | 8.9 GB | **48 KB** | ↓ 99.999% |
| 磁盘 | 80% | 63% | 释放 7 GB |

## 已知遗留

**8 个 loomy 账号 session 仍失效，本次未重登**（用户明确本次不做）。
`internal/gateway/errorclassifier.go:120` 写明「会话已死 → 永久禁用（**需人工重登**）」；
现有登录只有「本机客户端登录态」（`client_data_dir` 为空，不可用）与
「手机号 + 短信验证码」两条路，需逐个收码。

⇒ 这些号仍会周期性报 `100002`，但**已被退避与限流约束**（每 5 分钟最多一条日志），
不再形成洪泛。**这不是回归，是已知遗留。**

## 顺带修正

Release 说明的取用路径此前踩过坑（`artifacts/` 里 `[ -f RELEASE_NOTES.md ]` 恒假 →
静默回退模板）。本次说明按约定写在仓库根 `RELEASE_NOTES.md`，
流程未改动，仅确认取用正常。
