# 渠道与 ApiKey 设计对比报告：workbuddy2api vs New API

> 对象：`workbuddy2api`（本仓）对比 `QuantumNous/new-api` @ `upstream/main`（`33c61a2`）
> 结论先行：**渠道侧的设计是对的（含能力索引、级联删除）；问题在 ApiKey 侧——
> 我们把「权限」和「展示名」两种语义压在了同一组字段上，导致删渠道之后出现悬空引用。**

## 1. 两边的结构

### 1.1 渠道（Channel）

| 关注点 | 本仓 | New API |
|---|---|---|
| 主键 | `id string`（`ch-xxxx`） | `id int`（自增） |
| 上游模型清单 | `Models []string` | `Models string`（逗号分隔） |
| 上游名 → 对外名 | `ModelMap map[string]string` | `ModelMapping *string`（JSON） |
| 分组 | `Group string`（单值） | `Group string`（**逗号分隔的多值**） |
| 优先级/权重 | `Priority int` / `Weight int` | `Priority *int64` / `Weight *uint` |
| 状态 | `Status int` | `Status int`（另有 AutoBan 等） |
| 多 key | `Keys []ChannelKey`（**带每把 key 的状态与原因**） | `Key string`（逗号分隔，无单 key 状态） |
| 参数覆盖 | `Overrides.Param map[string]any` | `ParamOverride *string` |
| 元信息 | `ChannelInfo` | `ChannelInfo` + `OtherSettings` |

**两边共有且思路一致的：能力索引（Ability）。**

```go
// 本仓                                   // new-api
type Ability struct {                     type Ability struct {
    Group     string                          Group     string
    Model     string                          Model     string
    ChannelID string                          ChannelId int
    Enabled   bool                            Enabled   bool
    Priority  int                             Priority  *int64
    Weight    int                             Weight    uint
    Tag       string                          Tag       *string
}                                         }
```

两边都把 Ability 当**渠道的派生投影**，而不是独立事实：

| 动作 | 本仓 | New API |
|---|---|---|
| 建/改渠道 | `rebuildAbilitiesLocked`（先删后建） | `UpdateAbilities`（先删后建，带事务） |
| 删渠道 | `Delete` 内级联删 abilities | `DeleteAbilities`（`WHERE channel_id = ?`） |
| 停用渠道 | 不产生能力（`Status != Enabled` 直接返回） | `Enabled = channel.Status == Enabled` |

**这一层我们做对了**，与 new-api 同构。

### 1.2 API Key（Token）

| 关注点 | 本仓 `apikey.Key` | New API `model.Token` |
|---|---|---|
| 主键 | `ID string`（`sk-xxx`，即 Bearer 值本身） | `Id int` + `Key string`（Bearer 值） |
| 归属 | `OwnerID string`（`u-xxxx`，空=管理员） | `UserId int` |
| 启用 | `Enabled bool` | `Status int` |
| 额度 | `Limit int64`（0=不限）+ `Used` | `RemainQuota int` + `UnlimitedQuota bool` |
| 限速 | `RPM int`（0=不限） | 无（走分组/用户级） |
| 过期 | **无** | `ExpiredTime int64`（-1=永不过期） |
| IP 白名单 | **无** | `AllowIps *string` |
| 分组 | **无**（不能按令牌选分组） | `Group string` + `AutoGroups` + `CrossGroupRetry` |
| 模型限制 | `Models []string`（**nil=不限**） | `ModelLimitsEnabled bool` + `ModelLimits string`（逗号分隔） |
| 模型别名 | `ModelMap map[string]string`（**对外名 → 内部名**） | **无此概念** |
| 统计 | `Requests` / `Fails` / `Used` | `AccessedTime` / `UsedQuota` |

**最大的差异：new-api 的令牌没有「模型别名」；我们有。**

这是本仓的**增量能力**，不是缺陷——用户原话要的就是「对外的模型应该叫什么」。
但它是问题的来源，见下文。

## 2. 模型限制的判定口径差异

### New API（`middleware/distributor.go:57-74`）

```go
if modelLimitEnable {
    if !TokenModelLimitAllows(tokenModelLimit, modelRequest.Model) {
        abortWithOpenAiMessage(c, http.StatusForbidden, MsgDistributorTokenModelForbidden)
    }
}
```

要点：

1. **白名单是独立字符串集合**，与渠道/能力表**零耦合**——它不查目录，只比对名字。
2. `/v1/models` 的过滤是「能力表 ∩ 白名单」（`controller/model.go:240-262`），仍然是集合交集。
3. **没有别名层**，所以不存在「白名单名字与目录名字不一致」的问题。

### 本仓（`internal/server/apikey_models.go`）

```go
func (h *Handler) modelAllowedForRequest(w, r, model string) bool {
    allowed, restricted := h.allowedModelsForRequest(r)
    ...
    // ① 别名 key 拒绝上游真名
    if h.keyUsesAliases(r) && !isAliasForm(m) { ... }
    // ② 白名单里存内部名 → 先把别名换回内部名再比
    if resolved := h.resolveModelAlias(r, m); resolved != m { m = resolved }
    if modelAllowed(allowed, m) { return true }
    ...
}
```

要点：

1. 白名单里存的是**内部名**（`csg/deepseek-v4.1-flash-1017`）。
2. 客户端发来的是**对外名**（`002/deepseek-v4.1-flash-1017`），要先映射再比。
3. 于是同一条规则要在**三个地方**各实现一次：请求准入、目录过滤（`filterModelsForRequest`）、渠道目录合并（`channelPublicNames`）。

### 差异汇总

| 维度 | New API | 本仓 | 评价 |
|---|---|---|---|
| 白名单存什么 | 对外名（无别名概念） | **内部名** | 本仓更绕，因为要支持改名 |
| 别名在哪一层 | 只有渠道级 ModelMapping | 渠道级 + **令牌级** | 本仓多一层 |
| 判定点数量 | 2（准入 + 目录） | 3（准入 + 目录 + 渠道合并） | 本仓易漂移 |
| 悬空引用 | 存在（白名单不随渠道删） | 存在（本次已修） | 两边都有 |

## 3. 真正的问题：三重身份压在同一组字段上

本仓的 `Models []string` 同时承担了三种语义：

```
① 权限       —— 这把 key 能调哪些模型
② 目录选择   —— 编辑界面里勾了哪些（决定 /v1/models 显示什么）
③ 路由依据   —— 名字必须能与「内部名」对得上，否则解析失败
```

New API 只承担 ①（外加 /v1/models 的过滤），所以它没有这类耦合。

### 由此产生的四个具体症状（都已实测）

**S1 删渠道后白名单悬空**（本次已修）

```
删渠道 → 目录里没了 → 但 key.Models 还留着
后果：① 403 错误里「授权了 N 个」比实际多 ② 管理界面那条是死配置
```

根因不是「忘记清理」，而是**白名单被当成了独立事实**。
在 new-api 里这不是问题，因为它的白名单本来就只是名字集合，不需要与路由对齐。

**S2 目录与白名单可能长期不一致**

```
上游下线某模型 → 目录消失 → key.Models 仍留着 → 同样悬空
（本次只修了「删渠道」这一条路径，上游侧下线仍会悬空）
```

**S3 别名可以指向任意名字，包括不存在的**

```
model_map: {"003/intern-latest": "shusheng/intern-latest"}
而 shusheng 既不是注册上游也不是渠道
→ 目录里被过滤掉（看不见）
→ 但请求进来时会先过白名单（通过），再报 unknown_provider（绕一圈）
```

**S4 三个判定点各实现一次，容易漂移**

`modelAllowedForRequest` / `filterModelsForRequest` / `channelPublicNames` 三处
都在做「别名 ↔ 内部名」的换算与比对。任何一处改了，另外两处不会自动跟上。
实测已经踩过：`modelAllowedForRequest` 曾拿解析后的内部名去判，导致
「拒绝上游真名」那条判据把**用别名的合法请求**也拒了。

## 4. 该不该改？三条路线

### 路线 A：保持现状，只补清理（最小改动）

本次已做：删渠道时级联清 key 引用。

- 优点：零风险，不改数据结构
- 缺点：S2/S3/S4 仍在；本质是「让用户手动维护两份清单」

### 路线 B：白名单存对外名（对齐 new-api 的心智）

```diff
- Models []string  // 内部名：csg/deepseek-v4.1-flash-1017
+ Models []string  // 对外名：002/deepseek-v4.1-flash-1017（无别名时即内部名）
```

- 优点：准入判定退化成纯集合比对（与 new-api 一致），S4 消失
- 缺点：**破坏性变更**，存量 key 的白名单全是内部名，需要迁移
- 迁移代价：读时若发现某条目是内部名且能反查到对外名，就换成对外名

  **存量实测**（本机 `data/apikeys.json`）：

  ```
  sk-1bcb3d9f…  models=9  有别名=9  无别名=0
  ```

  全部 9 条都是「内部名」（别名映射的 value 侧）。也就是说路线 B 是
  **全量迁移**，没有「大部分已经是新形态」的便宜可占 —— 迁移代码必须处理
  「一条都还没换」这个最常见的情形。

### 路线 C：白名单只存「是否限制」，勾选项来自目录

即「白名单 = 目录的子集快照」，每次保存时用目录校验。

- 优点：彻底消灭悬空（结构上不可能）
- 缺点：目录抖动（上游 5xx）期间保存会把白名单改小 —— 比悬空更糟

## 5. 我的建议

**短期（建议现在做）**：

1. ✅ 删渠道级联清理（已完成）
2. 加一个「目录一致性检查」动作：列出 key 里**不在当前目录**的条目，界面上标黄；
   提供「一键清理」，但**不自动跑**（避免目录抖动误删）
3. 保存 `model_map` 时校验目标名是否在目录里，不在就给警告（不阻止保存）

**中期（如果要对齐 new-api 心智）**：

4. 走路线 B：白名单改存对外名。这是唯一的**结构性**修法，能让 `modelAllowedForRequest`
   从「映射 + 比对」退化成「纯比对」，三处判定点合并成一处。

**不建议走路线 C**：把用户的选择绑到易变的目录快照上，会把「上游抖动」变成「配置丢失」。

## 6. 一句话总结

渠道侧（Channel + Ability + 级联删除）与 new-api 同构，**没有问题**。
ApiKey 侧的根源是：**我们把「对外展示名」这个 new-api 没有的概念塞进了同一个白名单字段**，
于是「权限判定」不得不依赖「名字能不能映射回内部名」，而映射会因渠道增删而失效。

本次修的悬空只是症状之一。要根治，得走路线 B。
