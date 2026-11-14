---
name: ADR-0001 三条 canonical 各自持有自己的分隔符与 nonce 长度
whenToUse: 当你要修改 / 质疑 phanthywork 的 canonical 拼装、分隔符、nonce 长度，或看到 `invalid_desktop_signature` / 500 空体时读这一份 —— 它解释**为什么**不能把它们合并成一个常量。
真源: internal/phanthywork/crypto.go（分隔符与 nonce 常量及其拼装函数）、internal/phanthywork/crypto_test.go（逐字节断言的强制守卫）
---

# ADR-0001: 三条 canonical 各自持有自己的分隔符与 nonce 长度，禁止合并成一个常量

## ⚠⚠ 2026-11-14 更正（真机反证：分隔符那半**错了**，nonce 长度仍成立）

**本文"device / v1 用单个空格"是错的。三条 canonical 全部用 LF（0x0A）。**

真机证据链（同一天、同一条代码路径，只改这一个变量）：

```
分隔符 = 空格  → POST /api/oauth/desktop-installations/register → 401 invalid_desktop_signature
分隔符 = LF    → 同一条请求                                     → 200 注册成功
```

官方 sidecar 2.51.0 二进制里 `const LF = "\n"`，三条串都是 `.join(LF)` ——
与第三方插件 magpie-PhanthyCode（标注「抓包 → 逐字符复现 → 200 通过」）的 `src/proof.mjs` 完全一致。

**原文引的那条依据（Tauri 前端 bundle 的 `[...].join(\` \`)`）对不上真机**：
那是前端 UI 侧的另一处拼装，不是上游签名要的两条 canonical。教训是 ——
**「在两处独立产物里找到同一说法」不等于「这个说法对」**，尤其当它从未被真机跑通时。
（真机把空格路径跑了一遍，得到的就是 401。）

- **仍然成立**：nonce 长度 device/v1 = 18 字节、inference = 24 字节（真机 + 二进制双重支持）。
- **不再成立**：分隔符"三条各不相同"。它们**是同一个值**（LF）。
- **常量仍不合并**：理由不再是"有差异"，而是语义分离（将来若真分化，改一处不连累另一处）。

现行真源：`internal/phanthywork/crypto.go` 的 `deviceV1Separator` / `inferenceSeparator`（都是 `"\n"`），
判据 `TestCanonicalSeparatorsArePerString` **按字节**断言 0x0A。
改这个上游的出站请求前请看 [`../protocols/phanthywork-出站请求对照表.md`](../protocols/phanthywork-出站请求对照表.md)。

---

## Status

Accepted（分隔符部分以上方更正为准）

## Date

2026-11-14

## Context

PhanthyWork 的签名链上有三条 canonical 串（device 绑定 / v1 请求头 / inference 证明），
早期文档与实现把它们当成"同一套格式"，于是三条共用一个分隔符常量、一个 nonce 长度常量。

2026-11-14 从两处**互相独立**的官方产物取证，证实这个前提是错的：

- **Tauri 前端 bundle**（`_up_/dist/assets/App-dSHMvpu3.js`）里 device 与 v1 两条串都是
  `[...].join(\` \`)` —— **单个空格**；nonce 来自 `getRandomValues(new Uint8Array(18))` —— **18 字节**。
- **`claude-sidecar.exe`** 的 `jO8()` 里 inference 串是 `[...].join(\`\n\`)`
  （原始字节 `5D 2E 6A 6F 69 6E 28 60 0A 60 29`）—— **LF**；nonce 来自 `Vd3(24)` —— **24 字节**。

两类错误的真机表现**都指不到根因**：

- device / v1 用 LF ⇒ `401 invalid_desktop_signature`
- inference 用空格 ⇒ `500 Internal Error` + **0 字节响应体**（kong upstream-latency 9~10ms）
- nonce 长度写错 ⇒ 同样是签名无效，但报错文案只会说 "signature is invalid"

也就是说：**错了会得到一个指向别处的错误码**。这个仓库已经在 2026-11-13/14 因此烧掉多轮排查。

## Decision

1. **三条串各自持有自己的分隔符常量**，不共享：
   - device / v1 → 空格
   - inference → LF
2. **nonce 长度按串区分**：device / v1 = 18 字节，inference = 24 字节。
3. **两个事实各配一条逐字节断言的测试**，且断言必须**用常量而非字面量** ——
   否则下次改分隔符时，断言会静默变成"永远通过"。
4. 具体常量名、值、拼装函数的位置**以真源为准**（见 `真源` 字段），本 ADR 不复述代码。

## Alternatives Considered

### 三条串继续共用同一个分隔符常量（现状）

- Pros: 代码更短；不需要在调用点选择 nonce 构造函数。
- Cons: 与官方实现不符，**必错**；且错误表现指向别处。
- **Rejected**：这是本次要修掉的假设本身。

### 用一个函数按前缀自动选分隔符（`if prefix == "desktop-inference-v2"`）

- Pros: 调用点更省事，不容易选错。
- Cons: 把"协议事实"藏进分支逻辑，测试无法逐字节钉住；且一旦有人新增第四条串，分支会静默走到默认值。
- **Rejected**：判据会从"两条具名常量"退化成"一个隐含默认值"。

### 只在注释里写清楚，不加逐字节断言

- Pros: 零成本。
- Cons: 渲染工具会把 `0x0A` 显示成「点」、把空格显示成看不见 —— 这个仓库已经因此读错过一次。
- **Rejected**：违反"不可强制的规范 = 不存在的规范"（见项目 `AGENTS.md` §17.2 的同类判据）。

## Consequences

- 调用点必须显式选择 nonce 构造函数（device/v1 与 inference 各一个）—— 多了一层显式性，
  换来的是"选错会立刻被测试抓到"。
- 新增第四类 canonical 串时，**必须**同时给出它的分隔符与 nonce 长度，并补一条逐字节断言；
  没有断言的串不许上线。
- 这条决策把"协议事实"从注释升级为**可执行判据**，代价是常量数量从 2 个变成 4 个。
