# 上游工具自动注入（对话里直接生图 / 联网）

> 状态：**已实现并实测通过**（2026-10-03）
> 相关：`docs/loomy-local-toolchain.md` 第十节（实测数据来源）

## 一、它做什么

用户原话：

> 我希望只能在使用 Loomy 上游任意一个模型时，启用该上游对应的工具。
>
> 我只要使用 Loomy 的任意一个对话模型，如果我要生图的话，它自动调用生图模型。

效果：客户端**零改动**。用任何一个对话模型，说一句"帮我画只猫"，
就能拿到图。

```
POST /v1/chat/completions
{"model":"loomy/qwen3.8-flash","messages":[{"role":"user","content":"帮我画一只戴墨镜的柴犬"}]}
```

响应里 `message.content` 是：

```markdown
图已生成：一只戴着黑色墨镜、嘴角带笑的柴犬，夏日阳光下浅景深草坪背景，酷劲十足 🕶️

![一只可爱的柴犬，戴着一副黑色时尚墨镜…](https://aigc-output-image-….png?q-signature=…)
```

同时响应顶层多一个 `loomy_artifacts` 字段（结构化产物）：

```json
[{"type":"image","url":"https://…png","prompt":"…","size":"2304x1728",
  "model":"doubao-seedream-5-lite","points_consumed":110}]
```

搜索同理：

```
{"messages":[{"role":"user","content":"最近 AI 圈有什么大新闻？"}]}
→ 模型自动联网，回复里带 [1][2] 来源编号，artifacts 里 hit_count=30
```

## 二、为什么"只能在使用某上游时启用它的工具"是**结构上**成立的

这条作用域**不需要在核心写任何 `if provider == "loomy"`**。

工具定义与执行都由**上游自己**提供，核心只驱动循环：

```
请求路由到 loomy     → 拿到 loomy 的 Provider → 问它 ChatTools()
请求路由到 workbuddy → 那个 Provider 没实现该扩展点 → 一个工具都不注入
将来加个新上游       → 它自己声明自己的工具 → 核心零改动
```

`workbuddy/auto` 永远拿不到 loomy 的生图工具 —— 不是靠判断，
是**结构上就拿不到**。这与 `QuotaExt` / `ImageGenExt` / `AccountImportExt`
完全同构。

之所以必须这么设计：工具背后是**上游的端点**（`/images/generations`、
`/search/tencent` 都实打实打在 `loomyad.xunfei.cn` 上），
换个上游这些路径根本不存在。做成"核心内置工具"就等于让核心
假装知道所有上游的端点 —— 那正是本项目一直在拆的那种耦合。

## 三、分层与文件

```
internal/gateway/chattool_ext.go    扩展点接口（上游实现它来声明工具）
internal/loomy/chattool.go          两个工具的定义与执行（工具是上游私有的）
internal/loomy/search.go            Client.Search（搜索端点）
internal/server/chattool.go         循环的判据、注入、消息追加、产物挂载、SSE 编码
internal/server/chattool_loop.go    循环本体
internal/server/chattool_send.go    循环内的非流式发送
cmd/server/chattool_router.go       装配层适配（按上游 ID 找 Provider 执行）
```

扩展点形状：

```go
type ChatToolExt interface {
    ChatTools() []ChatTool
    ExecuteChatTool(ctx context.Context, uid, name string, args json.RawMessage) (ChatToolResult, error)
}
```

`ChatToolResult` 里有三个出口，各服务一个对象：

| 字段 | 给谁 | 用途 |
|---|---|---|
| `Content` | **模型** | 喂回为 `role:"tool"` 消息，让它能收尾 |
| `Markdown` | **人** | 追加到回复正文（客户端直接渲染出图） |
| `Artifacts` | **程序** | 挂在响应顶层的结构化产物 |
| `OK` | 模型 | 失败时置 false，`Content` 里写失败原因 |

## 四、循环流程

```
① 判据：该不该接管？（见第五节）
② 注入 tools 到请求体
③ 发一次**非流式**请求
④ 看 finish_reason / tool_calls
     没有 → 这就是最终回复，跳出
     有   → 执行工具，把 [assistant(tool_calls), tool(...)] 追加进 messages，回到 ③
⑤ 把产物的 markdown 追加进正文、结构化产物挂到顶层
⑥ 按客户端要的形态发射（要流式就编成 SSE，否则 JSON）
```

**轮次上限 3**（`maxToolRounds`）。必须有上限：模型可能陷入
"反复调同一个工具"的循环，没有上限会一直烧积分 —— 那是用户看不见的损失。
到顶时不再执行工具，把已完成的结果交给模型做最后一次收尾
（这样用户至少拿到已经生成好的东西 + 一句解释，而不是一个报错）。

### 4.1 为什么循环内部走**非流式**

工具循环要求"先知道模型要不要调工具，再决定给客户端什么"——
而流式是边读边发的，第一帧出去后状态码与内容都收不回来了。
若边流边看，客户端会先收到 `tool_calls` 帧（那是**协议内部**的东西，
客户端不懂就会显示成乱码），然后才收到最终答案。

所以内部用非流式跑完循环（实测上游支持 `stream:false`），
最后按客户端要的形态发射。

**代价**：带工具的这一轮没有逐字打字机效果（内容是整块出现在一帧里）。
这是"客户端零改动"必须付的代价，且只在真的要生图/搜索时才发生 ——
普通聊天路径**逐字节未变**。

### 4.2 为什么要自己编 SSE

绝大多数客户端默认 `stream:true`。若工具循环只能返回 JSON，
那些客户端会因为"要的是事件流、拿到的是 JSON"而解析失败 ——
而它们**没有任何错**，是网关这边形态没对齐。

`writeAsSSE` 编出标准 OpenAI 流式帧：首帧（role + 全部内容）、
收尾帧（finish_reason + usage）、然后 `[DONE]`。

## 五、接管与否的判据

全部满足才接管：

| # | 判据 | 理由 |
|---|---|---|
| ① | 装配层提供工具能力 | 单上游部署没有这个概念 |
| ② | 被路由到的上游**确实有**工具 | 上游没实现扩展点 → 不注入 |
| ③ | 请求里**没有**非空的 `tools` / `tool_choice` | 见下 |
| ④ | 未配置 `disables_chat_tools` | 留一个退回开关 |

### ③ 为什么"客户端自带工具就不接管"

客户端自带 `tools` 说明它**懂**这个协议 —— 它期望拿到 `tool_calls`
自己去执行（agent 框架都这么干）。网关再插一脚替它执行，两边会打架：
客户端按自己的期待仍在等 `tool_calls`，却收到了"已经执行完的结果"。

所以：

- 不带的（普通聊天客户端）→ 网关接管
- 带的（agent 客户端）→ 只转发，行为与改造前一致

⚠ 判据是"**非空**容器"：`{"tools":[]}`、`{"tool_choice":{}}`、
`null` 都算**没带**。有的 SDK 初始化后就是空数组；
若按"字段存在"判定，这些客户端就永远拿不到自动生图，
而它们的 JSON 看起来"带了 tools"，排查时极难看出原因。

## 六、实测结果

四类场景都跑过（真实凭证、真实上游）：

| 场景 | 结果 |
|---|---|
| 普通聊天（不该触发） | 2.1s，无 `loomy_artifacts` 字段，回复正常 **✓ 行为未变** |
| 一句话自动生图 | 28.7s，2 轮（调工具 → 收尾），正文含 markdown 图，`points_consumed=110` |
| 自动联网搜索 | 24.7s，回复带 `[2][6]` 来源编号，`hit_count=30` |
| 流式客户端（`stream:true`） | 3 帧、含 `[DONE]`、图片 markdown 在首帧 |
| 客户端自带 `tools` | 返回 `finish_reason=tool_calls` + 它自己的工具名，**无** artifacts **✓ 未接管** |

生成的图片下载验证过：真实 PNG，1360 KB，1024x1024，肉眼看内容正确
（戴墨镜的柴犬）。

## 七、开发中踩到的两个**静默失败**

两个都没有任何报错，只有实测能发现。都加了测试钉死。

### 7.1 `tool_calls` 的类型是 `[]map[string]any`，不是 `[]any`

`wire.Aggregate` 把 tool_calls 放进 message 时用的是
**`[]map[string]any`**（见 `wire/sse.go`），而不是 JSON 反序列化后的 `[]any`。

第一版只断言 `.([]any)`，后果：

```
上游确实回了 tool_calls（37 KB 流、finish_reason=tool_calls）
聚合也成功（message 里躺着 1 个调用）
**但断言失败 → 0 个调用 → 循环判成"模型没调工具" → 直接收尾**
```

表现是"客户端收到 `finish_reason=tool_calls` 却没有任何工具结果"——
看起来像"上游不支持工具"，真相只是一个类型写错。

测试：`TestToolCallsOfAcceptsBothShapes`（**两种形状都必须认**）。

### 7.2 markdown 被赋值了却没人读

`ChatToolResult.Markdown` → `toolExecResult.markdown` 存下来了，
但循环里只收集 `artifacts`，**markdown 没往下传** ——

于是图片 URL 只出现在结构化字段里，回复正文一句图都没有。
用户要求的是「写进回复内容（markdown）」，那条要求就落空了。

一个字段被赋值但没人读，编译器不会报。测试：
`TestAppendArtifactsToResponse` / `TestAppendArtifactsEmptyContentUsesMarkdown`。

### 变异验证

两条测试都做过变异验证（注入缺陷 → 断言变红 → 还原）：

- 删掉 `toolCallsOf` 的 `[]map[string]any` 分支 → 两个测试红
- 删掉 content 的 markdown 追加 → 两个测试红

## 八、边界与不做的事

- **只接了 loomy**。工具是上游私有的，别的上游要接就自己实现
  `ChatToolExt`，核心与出口层零改动。
- **工具失败不让整个请求失败**。上游 5xx / 积分不足时把失败写进
  `Content`（`OK=false`），让**模型**去向用户解释（它知道上下文），
  而不是回一个 502 错误页。只有"未知工具名"才返回 error ——
  那是核心传错了名字，是本仓的 bug，必须显式暴露。
- **不替上游纠正 `n>1`**。上游忽略 `n`（只给一张图、只扣一次费），
  网关如实透传。自我纠正意味着网关自己猜"用户其实想要两张"并自行
  发两次请求 —— 那是在计费上做出未经同意的决定。
- **不做工具结果的智能裁剪**。搜索的 `content` 按 800 字节截断
  （上下文预算），生图结果原样给全 —— 不是按"重要性"挑内容。
- **没有 WebUI 入口**。目前只能通过 HTTP API。
- **`loomy_config` / `loomy_soul` / `loomy_glasses` 没有搬**。
  它们依赖 Loomy 客户端本地的状态与硬件（本地配置文件、眼镜设备），
  搬过来是空壳。见 `docs/loomy-local-toolchain.md` 10.7。

## 九、配置

```json
{
  "loomy": { "enabled": true, "auth_dir": "./auths/loomy" }
}
```

**不需要额外配置** —— 工具自动生效（默认开启）。
"配了 loomy 账号就等于配了这两个工具"，因为两者的鉴权都是账号的 session。

关闭（退回改造前行为）：

```json
{ "disables_chat_tools": true }
```

## 十、调用方怎么用

### 只想说话

照常调，不用管这个功能。

### 想要图

**在消息里说要图**即可 —— 模型会自己调工具：

```
{"model":"loomy/qwen3.8-flash",
 "messages":[{"role":"user","content":"帮我画一张赛博朋克风格的城市夜景"}]}
```

图在 `choices[0].message.content` 的 markdown 里，URL 在
`loomy_artifacts[0].url`。

### 想要更直接的生图

走 `/v1/images/generations`（见 `docs/image-generation.md`），
一次调用直出图，不经对话模型 —— 更快、更省（少一次模型往返）。

**两条路的取舍**：

| | `/v1/images/generations` | 对话里说一句 |
|---|---|---|
| 延迟 | 一次生图（约 20-30s） | 生图 + 2 次模型往返 |
| 积分 | 110 | 110 + 对话 token |
| 好处 | 直给、可控 | 模型会替你**润色提示词**、失败了会解释、能多轮追问 |
| 适合 | 提示词已想好 | 随口说一句、要模型自己发挥 |

### 自己是 agent 框架（想自己驱动工具循环）

照常发 `tools` —— 网关**不会**接管，只转发，你会拿到标准 `tool_calls`。
