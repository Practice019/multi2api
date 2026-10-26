# 分组（Group）机制改造计划

> 目标：把「渠道 → 模型 → 分组 → ApiKey」这条链路做成 New API 那样，
> 让 ApiKey 只看到、只能用它被分配的分组里的模型。

## 0. 你要的四件事（我复述一遍，确认理解）

1. **加渠道**：填 URL + API Key + 名称，勾选**一部分**模型（不是全部）
2. **建分组**：把「我选的这部分模型」放进一个分组
3. **建 ApiKey**：给这把 key 分配**一个或多个**分组
4. **该 key 只能扫到、只能调用**那些分组里的模型

## 1. New API 是怎么做的（读源码得到的机制）

### 1.1 分组不是一个独立实体

这是最关键的一点。New API 里**没有「分组表」**：

```go
// model/ability.go
type Ability struct {
    Group     string  // ← 分组只是 ability 上的一个字段值
    Model     string
    ChannelId int
    Enabled   bool
    Priority  *int64
    Weight    uint
    Tag       *string
}
```

分组的**存在性**由两处决定：

| 来源 | 作用 |
|---|---|
| `ability.Group` 里出现过的值 | 这个分组有哪些模型 |
| `setting/ratio_setting.group_ratio` 配置 | 这个分组叫什么倍率（也用它校验合法性） |

默认配置：

```go
defaultGroupRatio = map[string]float64{
    "default": 1,
    "vip":     1,
    "svip":    1,
}
```

### 1.2 渠道可以是「多分组」

```go
Channel.Group = "default,vip"   // 逗号分隔
```

建/改渠道时 `AddAbilities` 对 **models × groups 做笛卡尔积**，
每个组合生成一条 ability：

```go
for _, model := range models {
    for _, group := range groups {
        abilities = append(abilities, Ability{Group: group, Model: model, ChannelId: ...})
    }
}
```

### 1.3 ApiKey 选一个分组（不是多个）

```go
// middleware/auth.go:622-639
userGroup := userCache.Group        // 用户自身的分组
tokenGroup := token.Group
if tokenGroup != "" {
    // ① 必须在「该用户可用的分组」里
    if _, ok := GetUserUsableGroups(userGroup)[tokenGroup]; !ok { 403 }
    // ② 必须存在于 group_ratio（= 是个真实分组，不是垃圾值）
    if !ContainsGroupRatio(tokenGroup) && tokenGroup != "auto" { 403 }
    userGroup = tokenGroup          // 覆盖用户分组
}
```

**注意**：`token.Group` 是**单值**（`Group string`），不是多选。
多分组是**渠道**的属性，不是令牌的。

`auto` 是个特殊值：表示「按顺序试用户可用的所有分组」。

### 1.4 模型目录 = 分组内模型

```go
// controller/model.go:240-262
models := service.GetGroupsEnabledModels(ownerGroups)   // 该分组的全部模型
for _, modelName := range models {
    if modelLimitEnable && !tokenModelLimit[modelName] { continue }   // 再交白名单
    userModelNames = append(userModelNames, modelName)
}
```

即：**分组决定可见范围，白名单只能再缩小**。

## 2. 本仓现状

| 项 | 现状 |
|---|---|
| `Channel.Group` | **单值，且在选路里真的生效**（`channelroute/route.go:70`：`ch.Group != "" && ch.Group != group` 才跳过）。默认 `DefaultGroup` |
| `Ability.Group` | 有字段，`rebuildAbilitiesLocked` 写入；`select.go:74` 按 group 过滤 |
| `Ability` 查询 | `EnabledModels(group)` 已实现（`store.go:319`） |
| `apikey.Key` | **没有 Group 字段**（确认：`apikey.go` 里搜不到 Group） |
| `Models` 白名单 | 存**内部名**，独立于分组手工维护 |
| 结果 | ApiKey 看到的是「所有渠道模型 ∩ 白名单」，**与分组无关** |

**结论修正**：分组机制在**选路层已经存在且生效**（渠道 → group → 按 group 选路），
缺的是**令牌 → 分组**这一半 —— 即「这把 key 能用哪些分组」。
这也意味着改造比原估计更小：不需要新建「分组」概念，只需要把它接到 key 上。

**所以缺的不是字段，是：令牌 → 分组 → 模型 这条判定链。**

## 3. 改造方案

### 3.1 数据模型（最小改动）

```diff
// channelstore.Channel
- Group string  `json:"group"`                              // 单值
+ Groups []string `json:"groups"`                            // 多值（对齐 new-api 的逗号分隔）

// apikey.Key
+ Groups []string `json:"groups,omitempty"`                   // 分组 ID 列表（空 = 不限制）
- Models []string  `json:"models,omitempty"`                  // 废弃：与分组语义重复，见 Q3
```

**注意**：`Models` 是**废弃**而不是删除字段 —— 存量文件里还有它，
读的时候要走迁移（见 Q3「迁移」），写的时候不再产生。

**为什么令牌用 `[]string` 而不是 new-api 的单值**：

你的原话是「分配一个或者多个分组」。New API 是单值，但你明确要多选。
多选是**更宽**的能力（单值是多选的特例），且出口判定只是多一层循环，
没有额外代价。所以这里**故意偏离** new-api。

### 3.1.1 一个重要发现：分组机制已经存在一半

我核实了代码，本仓**已经有分组**，只是缺另一半：

```go
// internal/channelstore/store.go:45
const DefaultGroup = "default"

// internal/channelroute/route.go:70 —— 按分组选渠道，真的生效
if ch.Group != "" && ch.Group != group { continue }
```

现有三条渠道的 group 都是 `default`（实测 `auths/channels.json`）。

而 `route.go:44` 的注释写着：

```
// group 目前固定 channelstore.DefaultGroup（多分组留到以后）。
```

**所以「分组」在选路层已经跑通了，缺的是：**

| 缺什么 | 影响 |
|---|---|
| 令牌侧的 Group 字段 | key 无法限定可见范围 |
| 分组作为「模型集合」的对象 | 现在 group 只是 ability 上的字符串，没有「往分组里加模型」这个操作 |
| 多分组（渠道属于多个 group） | 一条渠道只能进一个组 |

这比我最初估计的**小**：不需要发明新概念，只需把它接到 key 上。

### 3.1.2 「一个模型名 → 多个渠道」这件事已经有了（实测）

你问的「把多个渠道的同一个模型统一成一个，外部调一个名字就轮询」——
**这个能力现在就有，不需要新建机制。**

#### 机制

两条渠道声明**同一个上游模型名**、`priority` 相同 ⇒ `channelroute.Plan` 会返回两条候选，
`pickCandidate` 在同一档内**按权重加权随机**挑一条：

```go
// channels_forward.go:203-256
// 分出档位（按 priority 降序，相邻相等即同档）
t := tiers[ti]              // attempt 递增 ⇒ 下一档
n := rand.Intn(sum)         // 同档内按 weight 加权随机
```

实测验证（我建了两条渠道都声明 `轮询测试/probe-x`，prio 都是 10）：

```
=== abilities 索引 ===
default | 轮询测试/probe-x | ch-3c936c48 | enabled=true   ← 渠道 B
default | 轮询测试/probe-x | ch-d9940b84 | enabled=true   ← 渠道 A
```

同一个模型名有两条 ability 记录 ⇒ 选路时会在这两条之间挑。

#### 所以「负载均衡」不用新做，但要补三件事

| 缺什么 | 现状 | 影响 |
|---|---|---|
| **可见性** | 一个模型名对应几条渠道，界面上看不出来 | 你不知道轮询会不会真的发生 |
| **失败切档语义** | `attempt` 递增切下一档，但同档内的另一条渠道**不会**重试 | 渠道 A 挂了 → 直接报错，不会自动试渠道 B |
| **权重/优先级的配置入口** | 字段有（`priority`/`weight`），但界面里没有 | 无法把流量按 7:3 分 |

### 3.1.2.1 一个真实缺陷：同档失败不换人（需修）

我核实了 `forwardViaChannel`：

```go
// channels_forward.go:90-92 —— 只试一次
func (h *Handler) forwardViaChannel(ctx context.Context, publicModel string, body []byte) (channelForward, error) {
    return h.forwardViaChannelAs(ctx, publicModel, publicModel, body)   // attempt 恒为 0
}
```

`attempt` 只由 `forwardViaChannelAttemptAs` 的调用方递增，而渠道分支在 handler 里
**只调一次** `pickCandidate(cands, 0)` 拿到一条，失败就 `writeChannelError` 返回。

后果：

```
模型X 由 渠道A(权重7) + 渠道B(权重3) 提供
随机挑到 A → A 挂了(502) → 直接报错给客户端
                          ↑ 不会自动试 B
```

而账号池路径是有轮转的（`maxAttempts` + `fail(uid); continue`）。
**两条路径的容错能力不一致** —— 渠道模式反而更脆。

这是本次要顺手修的：渠道分支失败时，若同档还有**未试过的**候选，换一条再试。
（只在**同一档内**换，不跨档 —— 跨档是降级，语义不同。）

### 3.1.3 关于「分组 = 渠道集合」这个说法

我原文写「不建议把分组做成渠道集合」，指的是：

```
✗ 分组 { 渠道A, 渠道B, 渠道C }   ← 分组直接引用渠道
```

因为你要的是「**模型**集合」——「我要这个模型」，至于它由哪条渠道提供、
要不要在多条渠道间轮询，那是**选路层**的事，已经由 priority/weight 解决了。

```
✓ 分组 { 模型X, 模型Y }        ← 分组引用模型（本计划的做法）
       ↓ 选路时
  模型X 由 渠道A(权重7) / 渠道B(权重3) 提供  ← 现有能力，自动加权随机
```

两层职责分开：**分组决定「有哪些模型」，选路决定「用哪条渠道」**。
如果分组直接指向渠道，这两层就糊在一起了 —— 同一个模型想换渠道要改分组配置。

### 3.2 分组从哪来（新增一层）

关键决策：**分组要不要做成独立实体（可增删改）？**

| 方案 | 做法 | 优点 | 缺点 |
|---|---|---|---|
| **G1 派生**（new-api 的做法） | 分组名直接写在渠道上，存在性由 ability 推出 | 零新表、零同步 | 改分组名要改所有渠道；空分组无法存在 |
| **G2 实体** | 新增 `groups.json`（名字+描述+倍率），渠道引用它 | 可增删改名、可空 | 多一层同步（删分组要清渠道与 key 的引用） |

**我建议 G2（实体）**，理由：

1. 你要「往分组里添加我选择的模型」——这是个**独立操作**，G1 里没有「分组」这个对象可操作
2. G1 下「建一个还空着的分组」不可能（它靠 ability 反推）
3. 我们刚踩过「删渠道后 key 里悬空」的坑，G1 会让悬空更难清理（分组名散落在渠道上）

代价是要做引用完整性 —— 但这个我们已经有一套做法了（`PruneModels`）。

```go
// 新文件 internal/groupstore/store.go
type Group struct {
    ID          string   `json:"id"`          // g-xxxx
    Name        string   `json:"name"`        // 展示名（唯一）
    Description string   `json:"description"`
    Models      []string `json:"models"`      // 该分组包含的内部模型名
    CreatedAt   string   `json:"created_at"`
}
```

**注意**：分组存的是**模型名清单**，不是「渠道引用」。

为什么：你选的是「我想要的这部分模型」，而不是「这条渠道」。
一个模型可能由多条渠道提供，分组只需要知道「要这个模型」，
至于走哪条渠道由既有的选路逻辑决定。

### 3.3 判定链（这是核心）

```
请求进来
  ↓
① 鉴权 → 拿到 key
  ↓
② key.Groups 为空？  ──是──→ 不限制（管理密钥 / 未分配的 key）
  │ 否
  ↓
③ 该请求的模型在这些分组里吗？
   union(key.Groups[].Models) 包含它？  ──否──→ 403
  ↓ 是
④ 交给现有的 providerFor / channelServes / 账号池
```

**只有三步** —— 因为白名单并入了分组（Q3），不再有第 ④ 步的交集运算。

```
```

**判定点只有一处**：`modelAllowedForRequest` 里加一段分组校验。
不在目录过滤、渠道合并处各写一遍（那正是 S4 漂移的来源）。

### 3.4 目录（/v1/models）怎么算

```
可见模型 =
  key.Groups 为空 ? 所有可用模型（现状）
  : ( ∪ key.Groups[].Models ) ∩ 真实可用目录
```

**注意最后那个 `∩ 真实可用目录`**：分组里写了的模型，如果所有渠道都挂了
（或上游下线了），仍然不显示 —— 分组是「允许范围」，不是「保证可用」。

然后**再与真实可用的目录求交**（模型在分组里但所有渠道都挂了 → 不显示）。

这一步复用现有的 `filterModelsForRequest`，只改它的输入。

### 3.5 引用完整性（吸取删渠道的教训）

| 删除对象 | 必须级联清理 |
|---|---|
| 删渠道 | 现有：清理 key 白名单/别名（已做） |
| 删分组 | 从所有 key.Groups 里移除（该 key 若因此变空 = 不限制，要提示用户） |
| 删模型（上游下线） | 从所有 group.Models 里移除 + 告警 |

## 4. 迁移（存量数据怎么办）

**关键原则：升级后现有行为一个字节都不变。**

```
存量 key 的 Groups = 空  → 走现有逻辑（白名单）→ 行为不变
存量 channel 的 Groups = 空 → 视为 ["default"] → 与现在等价
```

所以**不需要一次性数据迁移**。你可以慢慢把 key 切到分组模式。

## 5. 分阶段实施

| 阶段 | 内容 | 可独立验证 |
|---|---|---|
| **P1** | `groupstore`（分组 CRUD + 原子落盘）+ 管理接口 | 建/改/删分组，重启后还在 |
| **P2** | `apikey.Key.Groups` + 判定链（`modelAllowedForRequest` 加分组校验） | 给 key 分配分组后，越界模型 403 |
| **P3** | `/v1/models` 按分组收窄 | 列表只剩分组内模型 |
| **P3.5** | **存量 `Models` 迁移成自动分组**（幂等） | 升级后老 key 能用的模型一模一样 |
| **P4** | `Channel.Groups` 多值 + ability 笛卡尔积 | 一条渠道可进多个分组 |
| **P4.5** | 同档失败换渠道（3.1.2.1 的缺陷） | A 挂了自动试 B，不再直接报错 |
| **P5** | 引用完整性（删分组/删模型级联）+ 一致性检查 | 删分组后 key 里无悬空 |
| **P6** | 控制台界面：分组管理页 + Key 的分组选择器 | 全流程点得通 |

**P1~P3.5 就能完整满足你的诉求**，且升级无感（行为不变）。

| 阶段 | 为什么这个顺序 |
|---|---|
| P1 先做 | 没有分组对象，后面全是空谈 |
| P2 紧接 | 判定链是核心价值；先让它生效，界面后补 |
| P3.5 在 P2/P3 之后 | 迁移要依赖分组已存在、判定链已生效，否则迁过去也没用 |
| P4 靠后 | 「渠道多分组」是增强，不影响单人使用 |
| P4.5 独立 | 它修的是既有缺陷，与分组无关，可随时插入 |

P6 界面之前，所有能力都能用 curl / 配置文件验证 —— 不必等界面。
## 6. 需要你拍板的四个问题

### Q1（已定）分组是**独立实体**（`data/groups.json`）

已确认走 G2（实体）。理由见 3.2：你要「往分组里添加我选择的模型」，
这需要一个可操作的分组对象 —— New API 那套派生机制下没有这个对象。

### Q2（已定）ApiKey 分配**多个**分组

已确认多选（`[]string`）。这与 New API（单值）**刻意不同**：
它那边分组主要用来算计费倍率，单值够用；我们这边分组纯逻辑，多选才有意义。

### Q3（已定）白名单与分组：**语义完全重复，白名单并入分组**

你的判断是对的。我核实了代码：

```go
// apikey_models.go:123 —— 白名单就是「这把 key 能用哪些模型」
func (h *Handler) allowedModelsForRequest(r *http.Request) ([]string, bool) {
    models, ok := h.cfg.APIKeys.ModelsOf(kid)
    if !ok || len(models) == 0 { return nil, false }   // 空 = 不限制
    return models, true
}
}
```

而分组要做的是同一件事：**限定这把 key 可见/可调的模型**。
两个机制并存 = 两处配置表达同一意图 ⇒ 必然不一致（这正是我们反复踩的坑）。

#### 三个方案

| 方案 | 做法 | 评价 |
|---|---|---|
| A. 交集 | 分组定上限，白名单可再缩小 | **否决**：多一层无用抽象，两处要同步维护 |
| B. 分组取代白名单 | `Key.Models` 废弃，只留 `Key.Groups` | **采纳** |
| C. 二选一互斥 | 配了分组就不能配白名单 | 否决：互斥判据本身就是复杂度 |

#### 采纳 B：分组是唯一的「模型范围」声明

```diff
// apikey.Key
- Models []string   // 白名单：内部模型名
+ Groups []string   // 分组 ID 列表（空 = 不限制，兼容存量）

// ModelMap 保留（它做的是「改名」，不是「限定范围」，职责不同）
  ModelMap map[string]string
```

**为什么 ModelMap 保留而 Models 废弃**：

| 字段 | 职责 | 会不会重复 |
|---|---|---|
| `Models` | 限定**能用哪些** | **与分组重复** → 废弃 |
| `ModelMap` | 把对外的名字**换成**内部名 | 不重复（分组不管改名） |

#### 迁移：存量 `Models` 怎么办

存量 key 有 `Models` 没有 `Groups`。策略：**自动建成一个分组**。

```
升级时（读 apikeys.json 发现某 key 的 Groups 为空且 Models 非空）：
  ① 建一个分组，名字取「key 名 + 的模型」
  ② 分组的 Models = 该 key 的 Models
  ③ 该 key 的 Groups = [这个分组的 ID]
  ④ 清空该 key 的 Models（保留 ModelMap）
}
```

这样升级后**行为不变**（能用的模型一模一样），但配置收敛到一处。

⚠ 迁移要做成幂等的：重复跑不会建出多个同名分组（按 key ID 记一个迁移标记）。

#### 好处（这是 B 比 A 强的实质理由）

| 场景 | 方案 A（交集） | 方案 B（取代） |
|---|---|---|
| 给两把 key 同一批模型 | 两处各配一遍，改一个漏一个 | 都指向同一个分组，改分组即可 |
| 排查「这把 key 为什么调不了 X」 | 要查两处 | 查一处 |
| 判定点数量 | 2 处（分组 + 白名单） | **1 处** |

你要求「往分组里添加我选择的模型」——这句话本身就说明：
**模型的选择本就应该发生在分组上**，而不是在每把 key 上重复一遍。

### Q4（已定）分组**不带**倍率/额度，纯逻辑分组

已确认。本仓无计费，加了是死配置。分组只回答一件事：**这个分组里有哪些模型**。
new-api 的 group 主要用途是**计费倍率**（`group_ratio`）。
我们目前没有计费。所以：

| 选项 | 说明 |
|---|---|
| **A. 纯逻辑分组**（我建议） | 只用来圈模型，不带倍率 |
| B. 预留倍率字段 | 现在不用，留着以后接计费 |

## 7. 我不建议做的

| 不做 | 原因 |
|---|---|
| 照搬 new-api 的 `group_ratio` 计费倍率 | 本仓无计费，加了是死配置 |
| 让 ApiKey 选「用户分组」（`user.Group` → token 覆盖） | 本仓没有多用户分层，多一层是空转 |
| `auto` 分组语义 | 那是给「按倍率挑最便宜分组」用的，我们无倍率 |
| 把分组做成「渠道集合」 | 你要的是「模型集合」，渠道选路已有既有逻辑 |

## 8. 交付物

```
internal/groupstore/store.go        分组存储（CRUD + 原子落盘）
internal/groupstore/store_test.go
internal/apikey/apikey.go           + Groups []string；Models 标废弃
internal/apikey/migrate.go          存量 Models → 自动分组（幂等）
internal/server/apikey_models.go    判定链改为查分组（唯一判定点）
internal/admin/groups.go            分组管理接口（list/create/update/delete）
cmd/server/main.go                  注入 groupstore 到 server.Config
webui.html                          分组管理页 + Key 的分组多选器
```

每个阶段独立 commit，可单独回滚。

## 9. 不做的事（明确列出，避免后人对齐错方向）

| 不做 | 原因 |
|---|---|
| 保留 `Key.Models` 与分组并存 | 语义重复，两处配置必然不一致（见 Q3） |
| 分组带倍率/额度 | 无计费（Q4） |
| `auto` 分组 | 那是按倍率挑最便宜组用的 |
| 分组直接引用渠道 | 分组管模型、选路管渠道，两层职责分开（3.1.3） |
| 令牌级「用户分组 → 令牌覆盖」 | 本仓无多用户分层 |
