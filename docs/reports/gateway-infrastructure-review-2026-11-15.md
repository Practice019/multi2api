# 网关基础设施评审：模型广场、渠道、权限分组与令牌

> 日期：2026-11-15；性质：静态研究报告与候选优化方案，不是 ADR、实施规格或已批准计划。
> 主项目基线：`004d42257babfde0657a1ed0ef1fda71322fdef7`；本轮写入前 `git status --short` 为空。
> New API：官方 `QuantumNous/new-api`，`main` 在取证时的固定提交 `b388140acfd80369ec6243b16869daff1b4051f4`。
> 参考提交时间：2026-11-13 17:49:05 UTC；仅通过固定 SHA GitHub API 取证，无本地 clone。

## 1. 执行摘要

四项设施已经具备可复用骨架，主要缺口是语义、执行链和界面承诺没有完全对齐，不宜先重写框架。
模型广场需要从“目录加账号数量”升级为来源、能力、授权和资源状态可解释的目录，但不承诺调用成功。
渠道、账号池、权限分组和令牌是不同实体；本项目分组不能直接套用 New API 的倍率与路由组概念。
P0 候选是 IP 限制承诺、有限零额度边界和非显式密钥披露；均需先补隔离用例再决定实现。
P1 候选是组语义、删除影响、混合别名、目录刷新及能力准确性；P2 才是 URL、主题和检索渲染。
建议优先复用现有表格、模型选择器、toast、busyRun 和共享主题，保留单文件部署及轻依赖。
本报告所有改动路线均未获批准；不新增“已确定 Next Steps”，不恢复用户已删除的供应商探测按钮。

## 2. 范围、证据方法与限制

- 【静态确认】表示已从代码或既有测试文本确认结构、分支或断言，不代表测试在本轮通过。
- 【条件风险】表示特定数据、缓存、候选组合或调用入口下可能触发的问题，不能扩大成普遍故障。
- 【待实测】表示需要隔离 handler、浏览器或真实调用才能确认的行为；本轮没有这些验证。
- 【产品裁决】表示至少存在两种合理产品语义，研究者不替用户批准其中一种。
- 主项目证据结合 T1/T2 研究摘要、主流程独立核验及本轮关键片段复读；未重新全面审计仓库。
- 本轮复读了目录投影、刷新前端、令牌 SQL 投影、准入、分组清理和渠道删除等关键片段。
- New API 证据全部固定到上述完整 SHA，不能用旧报告的行号替代当前版本证据。
- 获取方式为 `gh repo view QuantumNous/new-api --json nameWithOwner,url,defaultBranchRef`。
- 随后读取 `gh api repos/QuantumNous/new-api/commits/HEAD`，再对 contents/tree 请求显式指定固定 SHA。
- `artifacts/` 未设置整目录忽略，目标参考源码也未被忽略；为避免新增参考仓库，未执行 clone。
- 未读取主项目配置凭证、运行数据库、账号文件或真实密钥；未安装依赖、启服务、发推理请求。
- 未运行浏览器、测试、build、lint、typecheck 或会改文件／回源的前端套件；文中的验收均为未来候选。
- 报告沿用 `docs/reports/` 中文评审文档约定；不新增协议、ADR、spec 或 design 文件。

## 3. 实体关系：先区分权限、命名和路由

【静态确认】账号池 Pool 管理注册上游的账号资源，渠道管理另一种接入资源，两者不能互相充当健康事实源。
【静态确认】channelstore 为 SQL 渠道存储；渠道模型、映射和能力投影共同描述可服务名称。
【静态确认】groupstore 为独立 JSON 分组，使用 `g-*` 标识，承载模型集合与对外别名。
【静态确认】tokens SQL 与 apikey 内存索引是同一调用凭据的两种视图，不是两套独立权限主体。
【静态确认】普通令牌的有效分组引用优先取 Groups，空时再取 AutoGroups；有组时使用组模型并集。
【静态确认】组模型并集覆盖令牌旧 model_limits，而不是与其求交；无组时才回退旧白名单。
【静态确认】无组且无旧白名单表示不限；有组但并集为空表示拒绝，二者不能混为“空列表”。
上述语义见 `internal/server/apikey_models.go:126`、`:171`、`:180`、`:208`。
既有行为断言见 `internal/server/apikey_groups_test.go:33`、`:87`，本轮只读，未执行。
【产品裁决】若希望未来改成交集权限，必须显式设计兼容与迁移，不能把当前覆盖语义直接报成 bug。

```text
账号池／注册上游 ─┐
                 ├→ 可服务内部模型 → 分组集合与公开别名 → 令牌授权 → 请求准入
渠道／渠道密钥 ──┘                                      ↓
                                           别名选择与实际资源路由
全局目录 → 控制台目录视图；某令牌 /v1/models → 该凭据上下文下的模型视图
```

【静态确认】渠道转发的已查入口固定使用 DefaultGroup，不由令牌组自动切换：`internal/server/channels_forward.go:75`、`:124`。
【静态确认】渠道 priority 已不参与选路，候选按 ID 排序仅为确定性：`internal/channelroute/route.go:94`。
【静态确认】渠道权重加权随机、非正权重按一处理；不是严格轮询，参见 `internal/server/channels_uniform_test.go:55`。
【静态确认】渠道多 key 的 round_robin、公开别名目标轮询和渠道权重抽签是不同层次。
【条件风险】目录代表值仍按 priority 挑选，不能据此向用户宣称实际流量路由顺序：`internal/server/models_channels.go:295`、`:339`。

## 4. 四项设施与 New API 对照

下表主项目路径省略仓库根；New API 路径均属于参考 SHA，不是本项目文件。

| 设施 | 本项目静态现状 | New API 固定版本证据 | 借鉴边界 |
|---|---|---|---|
| 模型广场 | `/ui/models` 内嵌目录；SQL 非空优先，否则内存聚合；管理目录不是某把 key 的授权结果 | `model/pricing.go:217` 从启用 Ability 聚合，合并模型与供应商；`web/src/features/pricing/index.tsx:60` 多维筛选 | 借鉴来源和能力关联、表格筛选及详情，不直接移植商业价格广场 |
| 渠道 | SQL 接入配置及派生能力；与账号池并存；不按 priority 优选 | `model/channel.go:28` 保存接入配置；`model/ability.go:18` 为 group/model/channel 复合键，`:225` 派生能力 | 借鉴配置与投影一致性、多 key 运维，不恢复本项目已否决的优先级策略 |
| 分组 | 独立模型集合与别名；当前主要是授权及命名组织 | `controller/group.go:14` 枚举倍率配置；`setting/ratio_setting/group_ratio.go:30` 保存倍率及特殊可用组；`service/group.go:95` 解析 Auto 顺序 | 必须区分权限分组与倍率／路由组，不把同名词当同实体 |
| 令牌 | SQL 与 apikey 索引；组优先旧白名单；有额度、到期、RPM 等字段 | `model/token.go:14` 含额度、到期、模型/IP、组及 Auto 字段；`:224` 校验状态、到期及额度 | 借鉴独立限制执行和按需披露，不机械引入多租户充值结算 |

【静态确认】New API `/api/group` 在所查路由只有 GET：`router/api-router.go:372`。
【静态确认】另有独立 CRUD 的 prefill_group，是快捷选择预设，不等于倍率／路由分组：同文件 `:378`。
【静态确认】其价格页前端读取 `/api/pricing`：`web/src/features/pricing/api.ts:28`。
【静态确认】数据由本站模型价格／倍率、计费表达式、插件配置及能力索引组成：`model/pricing.go:381`、`:408`、`:447`。
【静态确认】控制器按用户可用组过滤，并处理组间倍率覆盖：`controller/pricing.go:38`、`:58`。
因此不能把该页面价格描述成“即时上游官方成本”，也不能把模型上架等同于调用成功。
【静态确认】New API IP 限制、用户状态和组资格在已查鉴权路径执行：`middleware/auth.go:591`、`:607`、`:622`。
【静态确认】普通非 pinned 选路检查模型限制：`middleware/distributor.go:54`；不能据此断言所有特殊路径同构。
【静态确认】New API 已支持逐 key 状态、禁用原因与随机／轮询：`model/channel.go:72`、`:219`。
旧文 `docs/channel-token-design.md:19` 的“无单 key 状态”不适用于该参考提交，本报告不修改旧文。

## 5. 前端按任务优化，而不是只补后端

### 5.1 找到可接入模型并复制到客户端

【静态确认】模型页入口见 `internal/server/webui.html:1294`；主体见 `internal/server/webui-models.html:1613`。
【静态确认】`/v1/models` 合并上游、令牌过滤、渠道及分组别名：`internal/server/handler.go:1025`。
【静态确认】管理目录 SQL／内存分支见 `internal/server/models_channels.go:689`，没有某把 key 的上下文。
现状任务断点：用户看见模型行后，难区分“目录存在”“有资源记录”“该令牌获授权”和“请求成功”。
最小候选：保留表格，增加来源、端点能力、快照时间、资源状态口径和公开名详情，不用全卡片重写。
接入路径候选：筛选 → 模型详情 → 选择协议 → 复制公开模型名与无密钥模板 → 跳转令牌管理。
【产品裁决】可选“按令牌预览授权”须由后端按令牌非秘密标识计算，不能先下发 key 再由浏览器试探。
验收候选：复制内容不含密钥；详情解释名称与端点；切换授权预览不改变全局目录事实。

### 5.2 添加资源、理解路由并排查失败

现状任务断点：账号池、渠道、渠道 key、组别名都有“可用／轮询”词汇，用户容易误读为同一策略。
最小候选：渠道表明确显示“权重随机”；保留配置 priority 时标“仅诊断，不参与调度”。
资源状态拆开“有账号记录／当前可选账号／渠道启停／有可用 key／最近错误”，不以单一绿灯替代。
提供渠道 → 受影响公开模型 → 权限分组 → 令牌的关联链接，不要求用户反复手抄名称搜索。
验收候选：仅存在过期账号的夹具不显示“已验证可调用”；多 key 轮询与渠道随机的说明不混用。

### 5.3 授权、改组与删除资源

最小候选：令牌编辑器展示权限来源、组并集覆盖旧白名单的规则，以及保存前后有效模型差异。
组编辑器同时展示内部目标和公开别名，空组明确标记“受限为空，调用拒绝”。
删除前展示影响预览；删除后分开展示主体删除、引用清理、权限变化和失败项，不只弹“成功”。
默认只请求掩码；用户显式复制／显示时才申请明文，关闭弹窗后不长期保存在页面状态。
验收候选：删除唯一组的确认页说明是否放权；清理失败可重试且不重新删除主体；toast 不含完整 key。

### 5.4 页面连续性与候选信息架构

【产品裁决】候选分区：资源管理＝账号池＋渠道；访问控制＝权限分组＋令牌；模型服务＝目录＋接入指引。
这只是任务组织提案，不是用户已确认导航；先用现有入口和关联链接验证，不先搬动全部页面。
【静态确认】导航主要依赖 localStorage：`internal/server/ui/app.js:4519`、`:4649`。
候选：用 URL 保留面板、筛选和选中实体，localStorage 只存个人偏好；URL 不得包含凭据。
【条件风险】主台解析系统主题，而模型页直接设置 system，iframe 同步需验证：`internal/server/ui/app.js:774`；`internal/server/webui-models.html:1576`、`:418`。
验收候选：深链刷新与后退恢复筛选；两标签互不覆盖；light/dark/system 在 iframe 内一致。

## 6. P0 候选：安全承诺与凭据边界

### P0-1 IP 限制的保存与执行链不一致

【静态确认】allow_ips 接受校验并入库：`internal/admin/tokens.go:831`、`:887`。
【静态确认】SQL → Key 投影未携带该字段：`internal/apikey/sql.go:177`；Validate 仅接收 bearer：`internal/apikey/apikey.go:828`。
【条件风险】所查请求链未执行这一 IP 限制；尚未通过真实请求证明绕过，不扩大为所有入口结论。
用户流程前后：从“保存即误以为生效”变为“明确未支持，或保存后在已声明入口强制执行”。
【产品裁决】最小止损是收敛界面承诺；完整方案需确定可信代理链、客户端 IP 解析及 CIDR 语义。
验收候选：隔离 handler 的允许／拒绝 IP、无效 IP、代理头伪造用例；所有声明支持的入口均覆盖。

### P0-2 有限零额度与不限额哨兵冲突

【静态确认】有限令牌的 Limit=Remain+Used；总和为零会形成 Limit=0：`internal/apikey/sql.go:192`。
【静态确认】Validate 仅在 Limit>0 时判额度：`internal/apikey/apikey.go:867`；创建缺省启用：`internal/store/token.go:503`。
【条件风险】有限、remain=used=0、status=enabled 的组合可能被当不限额；不是任意耗尽都绕过，exhausted 状态另有拒绝。
最小候选：在索引中保留“是否不限额”的独立事实，不借总量零表达两种相反权限。
用户流程前后：零额令牌创建／编辑后明确不能消费，而不是显示有限、执行不限。
验收候选：有限零额拒绝、不限额零额允许、正额耗尽拒绝、exhausted 拒绝、SQL 重载前后一致。

### P0-3 非显式明文披露

【静态确认】PruneGroups 将原凭据 k.ID 放入 KeysUnrestricted：`internal/apikey/prune.go:178`。
【静态确认】删除组响应把该列表及 warning 返回：`internal/admin/groups.go:287`；这是管理授权内披露，不是匿名越权。
【静态确认】管理列表返回 admin_key_full/admin_token_full：`internal/admin/apikeys.go:97`，令牌页打开会取相关列表。
【条件风险】两条路径均受管理授权约束；管理凭据列表已有 no-store，但所查删除组响应未见同等禁缓存设置。二者仍可能在非显式取明文操作中向浏览器下发完整凭据；这是管理授权内的披露面风险，不是匿名越权。
最小候选：列表与删除反馈只使用非秘密标识、名称、掩码；明文仅由独立显式操作返回。
验收候选：初始列表、删除响应、toast、日志无完整凭据；显式披露仍验证管理权限、禁缓存及统一限流。
【条件风险】令牌披露限流按 host+URL.Path 分桶，不同 ID 不同桶；渠道披露限流需统一评估：`internal/admin/tokens.go:545`；`internal/admin/channels.go:1488`。

## 7. P1 候选：语义一致性和任务闭环

### P1-1 组授权不等于组路由

【静态确认】转发固定 DefaultGroup；SQL 投影未携带跨组重试策略：`internal/server/channels_forward.go:124`；`internal/apikey/sql.go:177`。
【产品裁决】路线 A：明确组仅负责授权与别名，隐藏未执行的路由承诺；路线 B：定义并实现有序组路由。
最小候选优先是生效预览与说明，不把 New API Auto 语义直接塞入本项目独立分组。
验收候选：预览与实际选路使用同一策略；若选 A，不宣称跨组重试；若选 B，覆盖顺序、耗尽和禁跨组用例。

### P1-2 删除组放权与删除渠道引用清理

【静态确认】PruneGroups 只清 Groups，不清 AutoGroups；唯一手动组清空后可能转为不限：`internal/apikey/prune.go:144`。
【静态确认】已有放权警告，不得表述成“静默无提示”；既有断言见 `internal/admin/groups_prune_test.go:95`。
【产品裁决】保留不限、改为拒绝或要求重分配都影响产品契约，不能未经批准直接禁用令牌。
【静态确认】渠道删除调用 PruneModels 清旧模型引用：`internal/admin/channels.go:1402`；页面忽略清理错误的分支见 `internal/server/webui-channels.html:1919`。
最小候选：前置影响预览、覆盖两类组引用、展示部分失败；保留仍由其他渠道／账号池服务的模型。
验收候选：共享模型不误删；悬空别名可定位；清理失败显示范围及重试入口；确认后权限变化符合裁决。

### P1-3 混合账号池与渠道别名

【静态确认】healthyAliasCandidates 跳过渠道，pickAliasTarget 在健康列表非空时用它替代全部候选：`internal/server/apikey_models.go:830`、`:746`。
【条件风险】同一别名混合渠道与健康账号池目标时，渠道可能退出本轮候选；不是所有渠道调用都失效。
最小候选：按资源类型检查资格后合并，不能以“未参与账号池检查”代表“渠道不合格”。
验收候选：池健康＋渠道可用时两类均可入选；池冷却、渠道无 key、全不可用时结果与解释一致。

### P1-4 目录状态、刷新、端点及动作资格

【静态确认】callCell 根据账号记录数显示可调用：`internal/server/webui-models.html:2098`，不是健康或推理验证。
【静态确认】刷新只更新供应商计数，未替换 state.models：同文件 `:2350`；SQL 非空优先见 `internal/server/models_channels.go:689`。
【条件风险】SQL seed 缺项才补可能使快照、计数和明细不一致：`internal/store/model.go:172`；需要按存储分支构造复现。
【静态确认】目录投影统一填 chat：`internal/server/models_channels.go:415`；上游模型列表已有 image_generation：`internal/server/handler.go:1207`。
【条件风险】GET“绝不回源”的注释不能覆盖内存回退分支；旧缓存后台刷新、冷缓存同步取数见 `internal/server/models_flight.go:72`、`:87`。
【静态确认】纯渠道合成供应商仍显示重拉；非 registry provider 被拒：`internal/server/webui-models.html:2413`；`internal/server/models_channels.go:918`。
最小候选：统一快照版本更新明细和计数，保留真实端点，按后端 action capability 显隐重拉，明确快照读取与回源动作。
验收候选：重拉新增／移除模型后同页明细与计数一致；SQL／内存冷暖分支分别验证；渠道合成行不调用未知 provider。
保留 synthetic 不可批选规则，改变它必须产品裁决：`internal/server/models_channels.go:694`。
不恢复“探测／只看它”按钮，不自动发推理请求；反向断言见 `internal/server/models_channels_probe_test.go:301`。

## 8. P2 候选与分阶段路线（全部未批准）

P2 候选：URL 状态、主题同步、端点／资源／供应商筛选、稳定表格排序、空态与大列表增量渲染。
先复用共享主题与 model-picker、toast、busyRun，不引入 React 重写或付费体系作为前置条件。
验收候选：长列表搜索无明显阻塞；状态改变保持焦点和选择；刷新失败保留旧数据并标过期，不伪装成功。
阶段 A 候选：由用户裁决权限及披露契约，先列有限零额、IP、删除放权的隔离验收矩阵。
阶段 B 候选：落实获批 P0；每个 handler 切片先有失败证据，再实现并验证 SQL 重载与兼容行为。
阶段 C 候选：落实获批 P1，优先令牌生效预览、删除影响和目录刷新，逐页完成前后端闭环。
阶段 D 候选：再按任务反馈调整导航、URL、主题、检索性能；必要时才评估前端工具链变化。
每阶段退出条件是对应矩阵通过并留证；本报告未执行这些验收，也未承诺排期、投入或发布。

## 9. 不照搬 New API 的理由与参考索引

本项目偏自用、多上游账号资源聚合；New API 包含多用户计费、倍率、充值及插件计价，目标并不相同。
本项目已有独立权限分组与公开别名，不能替换成同名倍率组；也不能恢复用户已否决的渠道优先级。
可借鉴的是能力投影、限制实际执行、按需披露和任务交互，不是数据库字段或 UI 框架逐项照抄。
New API 当前前端基于组件与查询层，本项目已有内嵌页面及同步机制；迁移成本需单独裁决。
主项目入口约定：`AGENTS.md:17`；报告／协议分类：`docs/protocols/00-index.md:15`。
历史差异文档：`docs/channel-token-design.md:3`、`:19`；其旧提交结论不能替代本报告固定版本。
New API 源：<https://github.com/QuantumNous/new-api/tree/b388140acfd80369ec6243b16869daff1b4051f4>。
参考核心：`model/pricing.go:217`、`model/ability.go:225`、`service/group.go:95`、`model/token.go:224`。
参考交互：`web/src/features/pricing/index.tsx:124`；`web/src/features/keys/lib/api-key-form.ts:141`；`web/src/features/channels/hooks/use-channel-mutate-form.ts:69`。
本次唯一交付为本文；忽略规则 `!docs/reports/**` 已放行该路径，不需要修改 `.gitignore`。
未决项为上述产品裁决和待实测验收；报告没有将任何候选路线转成已批准实施事项。
