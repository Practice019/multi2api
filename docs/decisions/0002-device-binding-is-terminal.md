---
name: ADR-0002 二次注册是终态冲突，不是可重试错误
whenToUse: 当你要改设备注册 / 换密钥 / 多账号池的逻辑，或看到 `first_device_owner`、`disabled_account`、`desktop_installation_conflict` 时读这一份 —— 它解释**为什么**"冲突后重试"会封号，以及为什么反代不能与桌面客户端共用账号。
真源: internal/phanthywork/client.go（注册响应判定与 reason_code 出口）、internal/phanthywork/provider.go（熔断与状态落盘）、internal/phanthywork/upstream_test.go（终态断言的强制守卫）
---

# ADR-0002: 二次注册是终态冲突，不是可重试错误（一个账号只允许一台设备）

## Status

Accepted

## Date

2026-11-14

## Context

`POST /api/oauth/desktop-installations/register` 用于把一台设备绑定到账号。
本包原先的判定是"非 2xx 即失败"，并额外把 `409` 当成设备绑定冲突的终态。

2026-11-14 真机实测推翻了"冲突 = 409"这个前提：

```
09:08:5x  同一账号 /v1/messages                        → 200
09:19:33  用全新 identity seed 去注册第二台设备
          → 服务端回 HTTP 200 + {"reason_code":"first_device_owner","reward_status":"denied"}
09:20:08  同一账号 /api/oauth/profile、/api/phanthy_code/models  → 403 disabled_account
```

凭证文件里留下了那一行：`{"registered_at_ms":…,"reason_code":"first_device_owner"}`。

也就是说：**服务端用 200 表达拒绝**。旧判定把它当成功 ⇒ 写 `RegisteredAtMS` ⇒
后续拿一把服务端根本不认的 key 去换推理令牌、发推理（一路 403/500）⇒ 账号进入 `disabled_account`。

这条路径的代价不是"一次请求失败"，而是**账号被封**：
后台池里 7 个账号在 2026-11-13/14 全封，机制与此一致（客户端在用一个账号、反代又注册一台）。

## Decision

1. **注册响应里 `reason_code` 非空即终态冲突**，与 `409` 走同一条出口
   （调用方用 `errors.Is(err, ErrRegistrationConflict)` 判）。
2. 冲突时**必须**记 `ConflictAtMS` 与 `ReasonCode`，**绝不写** `RegisteredAtMS`，
   并在熔断窗口内**不再发注册请求**。
3. **identity seed 一经生成永久固定**；只有 inference（会话）密钥随每次注册更换。
4. "一个账号只允许一台设备"是**协议事实**，不是可绕过的限制 ——
   反代与桌面客户端**不得共用同一账号**（设备私钥在客户端 Rust 侧，不可导出）。

## Alternatives Considered

### 只按 HTTP 状态码判定（现状）

- Pros: 实现最简单。
- Cons: 服务端用 200 表达拒绝，这条路径**必然**走向封号。
- **Rejected**：这是本次要修掉的判定本身。

### 收到 `reason_code` 后换一把新 key 再试（"重试即自愈"）

- Pros: 看起来能自动恢复。
- Cons: 实测就是**继续重试把账号打进 `disabled_account`**；这是加速封号，不是修复。
- **Rejected**：代价不对称（一次重试 = 一个账号）。

### 反代复用桌面客户端已注册的那台设备

- Pros: 不新增设备，理论上一劳永逸。
- Cons: 设备 identity 私钥由 Tauri Rust 侧持有（`sign_desktop_device_binding` /
  `sign_desktop_activity` / `desktop_installation_identity`），**不可导出**；
  Windows 凭据管理器里那几条 `daily-activity-ed25519-v4:<sha256(accountScope) base64url>`
  经实测**不是**它（拿它签 `v1` 只会得到 `invalid_desktop_signature`）。
- **Rejected**：不可实现，且已验证过两次。

## Consequences

- 反代必须使用**专属账号**：该账号首次注册由反代完成，且**不要用桌面客户端登录它**。
- 首启若命中冲突，该账号进入熔断而不是重试 —— 需要人工换号，机器不再反复敲门。
- `ReasonCode` 必须落盘，好让日志与界面能说清"是同一账号已被别的设备占用"，
  而不是一句笼统的 `device_binding_conflict`。
