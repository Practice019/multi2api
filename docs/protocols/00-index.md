---
name: 项目协议总索引
whenToUse: 任何一次新的会话 / 新的 Agent 接手本项目时**先读这一份**；它回答"我现在该读哪一份协议"。以及：当你不确定某类问题该查哪里时。
真源: 本目录（docs/protocols/ 与 docs/decisions/）自身；每份文件的 `真源` 字段指向它管的那部分代码
---

# 项目协议总索引

> 这份索引存在的理由：Agent 每次都是"新来的"。没有索引，它会读到不该读的、或重复问已经写死的事。

## 目录约定（与 §21 的关系，以及为什么放在 `docs/` 下而不是项目根）

规范默认位置是 `<项目根>/protocols/`。本项目**已有约定**是所有文档都在 `docs/` 平铺
（`docs/` 下 20+ 份 .md，`.gitignore` 也是按 `!docs/xxx.md` 逐条放行的）。
两者取折中：**协议集中到 `docs/protocols/`，决策记录到 `docs/decisions/`** —— 既拿到"集中",
又不打破 `docs/` 的既有聚集习惯与 .gitignore 机制。

`docs/` 根目录下**不再新增**协议文件；新协议一律进这两个子目录。

---

## ① 我要做的事 → 读哪一份

| 我要做的事 | 读 | 类型 |
|---|---|---|
| 接一个**新上游** | [`protocols/上游接入协议.md`](上游接入协议.md) | How-to |
| 改 phanthywork 的**签名 / 推理**相关代码 | [`protocols/推理面诊断表.md`](推理面诊断表.md) 先读，再读 [`protocols/phanthywork-inference-protocol.md`](phanthywork-inference-protocol.md) | Reference |
| 排查 phanthywork 的 **500 / 403 / 封号** | [`protocols/推理面诊断表.md`](推理面诊断表.md) | Reference |
| 想知道**为什么**某条 canonical 是那样拼的 | [`decisions/0001-phanthywork-canonical-separators.md`](../decisions/0001-phanthywork-canonical-separators.md) | ADR |
| 想知道**为什么**二次注册不能重试 | [`decisions/0002-device-binding-is-terminal.md`](../decisions/0002-device-binding-is-terminal.md) | ADR |
| 项目整体怎么跑、怎么改、有哪些规矩 | [`../AGENTS.md`](../../AGENTS.md)（项目级） | 约定 |
| 已知问题与规避 | `../known-issues.md` | Reference |

## ② 协议清单

| 文件 | 类型 | `whenToUse` 摘要 |
|---|---|---|
| `protocols/上游接入协议.md` | How-to | 要接一个新上游，怎么证明做完了、做对了 |
| `protocols/推理面诊断表.md` | Reference | 出现 500 空体 / 403 / 封号 / `invalid_desktop_signature` / `first_device_owner` 时 |
| `protocols/phanthywork-inference-protocol.md` | Reference | 新旧内核（`/v1/chat/completions` vs `/v1/messages`）对照与抓包原文 |
| `decisions/0001-phanthywork-canonical-separators.md` | ADR | 三条 canonical 的分隔符与 nonce 长度为什么各不相同 |
| `decisions/0002-device-binding-is-terminal.md` | ADR | 二次注册为什么是终态冲突（封号机制） |

## ③ 判据在哪（"会失败的命令"汇总）

每条协议条款都必须绑一个能失败的命令。全项目的判据入口：

| 判据 | 命令 |
|---|---|
| 架构边界（上游 ↔ 核心包） | `go test ./internal/gateway/ -count=1` |
| phanthywork 签名链（分隔符 / nonce / session 头 / 时钟 / 注册终态） | `go test ./internal/phanthywork/ -count=1` |
| 全仓 | `go test ./... -count=1` |

## ④ 维护规则

- **同一件事做到第 3 次**才写协议（不是越全越好）。
- 每份协议必须有 `name` / `whenToUse` / `真源` 三字段 —— 缺 `whenToUse` 等于永远不会被加载。
- **只指不抄**：协议里指向真源（文件:行号 / 命令），不复述代码。抄一份就会有两份，两份就会漂移。
- ADR **只增不改**：决策变了就写新的，用 `Superseded by ADR-XXXX` 指向旧的；不删旧 ADR。
- 新增协议后，**回来更新本索引**（这是唯一的"我该读哪份"入口）。
