> Status: current

# Agent Instructions

本文件是**入口协议**，不是全部真相的存放处。业务背景在 `README.md`，
当前状态在 `handoff.md`，历史在 `docs/progress-log.md`。

## 上手前先读（按序）

1. `README.md` — 项目是什么、能力清单、配置说明
2. **本文件 §项目协议** — 命名规范、接缝、扩展点、加上游的完整清单
3. `CHANGELOG.md` — 最近做了什么、为什么（按版本倒序）
4. `docs/progress-log.md` — 时间线（append-only，含 `[决策]`/`[否决]` 标记）
5. `handoff.md` / `docs/open-questions.md` — **按需生长**：文件不存在就是还没触发，
   不要以为漏了（见文末「按需生长文件」）

## 项目协议（另一个 AI 要改这个仓时先读这一节）

本仓是**多上游聚合网关**：核心（账号池/日志/管理台/出口/调度）不认识任何具体上游，
一切上游差异通过 `internal/gateway` 这一条接缝表达。**加一个上游 = 加一个目录 + 实现接口 +
配置加一段 + 注册一行**，核心包一行不改。这不是愿景，是 `internal/gateway/arch_test.go` 用包级依赖图强制的。

### 1. 命名规范（改名前先看这张表）

| 对象 | 规则 | 例子 |
|---|---|---|
| 上游包目录 | `internal/<provider-id>`，**目录名 = provider id**，小写字母/数字/连字符 | `internal/cline` |
| Provider ID | `^[a-z][a-z0-9-]*$`（`validProviderID` 校验），全局唯一，**不得含 `/` `:` 空白** | `workbuddy` `workbuddy-intl` `qodercn` |
| ID 常量 | 包内 `const providerID = "xxx"` + 导出 `const ProviderID = providerID` | 见 `internal/cline/cline.go:38` |
| 模型名前缀 | `<provider-id>/<上游模型名>`，用 `gateway.SplitModel` 拆，**只切第一个 `/`**（模型名自身可含 `/`） | `workbuddy-intl/deepseek-v4.1-flash` |
| 配置段 | JSON 键 **snake_case**，与 Go 字段名 PascalCase 对应 | `"workbuddy_intl"` ↔ `WorkbuddyIntl` |
| 凭证文件 | `<auth_dir 一级子目录>/<filePrefix>*.json`，前缀写死在该上游的 `LoadDir` 里 | `auths/cline/cline-*.json` |
| 源文件切分 | 按职责拆，不按层拆：`provider.go`（契约）/ `client.go`（HTTP）/ `credential.go`（凭证）/ `models.go` / `adminroute.go` / `*_ext.go`（每个扩展点一个文件） | `internal/cline/*` |
| 装配文件 | `cmd/server/<id>creds.go`（并池）+ `<id>credsource.go`（按 uid 取活凭证） | `clinecreds.go` |
| 测试 | 与被测文件同目录 `*_test.go`；契约测试单独叫 `contract_hermetic_test.go`（**不碰网络的伪造凭证**） | `internal/cline/contract_hermetic_test.go` |
| 提交信息 | 中文 + Conventional 前缀 + 范围：`feat(上游):` `fix(渠道):` `docs(运维):` `chore(工具):` | `feat(上游): 打通 workbuddy-intl` |

### 2. 接缝：Provider 只有 4 个方法

```go
// internal/gateway/provider.go
type Provider interface {
    ID() string                                              // 唯一标识，兼作模型前缀
    Caps() Capability                                        // 能力位，声明了就必须真的能用
    Chat(ctx, cred Credential, body []byte) (ChatStream, error)  // body 已是 OpenAI 格式
    Models(ctx, cred Credential) ([]ModelInfo, error)
}
```

- `Credential{Provider, UID, Nickname, ExpiresAt, FilePath, Secret any}` —— 前五个是**跨上游共识**，
  `Secret` 是上游私有结构，**核心不得读它**。
- `ChatStream{Status int, Body io.ReadCloser}` —— 非 2xx 也要正常返回（把上游错误体带回来），
  只有网络层失败才返回 error；`Body` 由**调用方** Close。
- 深模块原则：上游的 header 拼法、签名、base URL、UA、请求体改写**全部关在实现里**。

### 3. 能力位（`Capability`）—— 声明即承诺

`CapChat`（**必须**）· `CapModels` · `CapCheckin` · `CapGrowth` · `CapTravel` · `CapWelfare` ·
`CapQuotaProbe` · `CapTasks` · `CapInvite` · `CapImport`

契约测试会验证「声明了 `CapModels` ⇒ `Models()` 真的非空」「声明了需要管理端点的能力位 ⇒
`AdminExt` 存在且路由非空」。**少报是安全的，多报会被当场抓住**；前端按能力位显隐，
多报等于画一个点了必然失败的按钮。

### 4. 扩展点（28 个接口，`gateway.ExtOf[T](p)` 类型断言发现）

**只实现自己有的**，不实现不算错（核心回落默认行为）。加第 N 个扩展点只改 `gateway`，不改核心。

| 扩展点 | 方法 | 用途 |
|---|---|---|
| `AdminExt` | `AdminRoutes() []AdminRoute` | 上游自己的管理端点（`Method/Path/Handler/Capability/Title/Hidden`） |
| `JobExt` | `Jobs() []Job` | 定时任务（`Name/Interval/Run/Due`） |
| `LoginFlow` | `Start/Poll/Configured` | 页内「添加账号」。`Configured()` 是**假按钮防护**：部署没配 OAuth 就别让前端画入口 |
| `ManualLoginExt` | `ManualLogin() (path, instructions)` | 服务器部署下回调打不进本机时，粘贴回跳 URL |
| `QRLoginExt` | `QRLoginSVG(authURL) string` | 扫码登录 |
| `AuthDirExt` | `AuthDir() string` | 我的凭证目录在哪（读与写必须问同一个目录） |
| `CredentialLoader` | `LoadCredentials(dir) ([]Credential, error)` | 重载 auths 时怎么解析我的凭证 |
| `CredentialSecretLoader` | `LoadCredentialsWithSecrets(dir)` | 加强版：连活 secret 一起给（**必须与上面同源**） |
| `DisplayNameExt` | `DisplayName() string` | 界面中文名（不实现就显示裸 id） |
| `NoticeExt` | `Notice() string` | 分组里的一句提醒 |
| `AccountColumnsExt` | `AccountColumns() []string` | 账号池显示哪些列（只报 id，标题/渲染是前端的事） |
| `AccountImportExt` | `ImportCredentials(pasted)` | 批量粘贴导入 |
| `QuotaExt` | `RefreshQuota(uid) (QuotaView, bool)` | 主动额度探测 |
| `ResetPolicyExt` | `ResetAt(cred) (time.Time, bool)` | 额度耗尽后什么时候能再用 |
| `RefreshSkewExt` | `RefreshSkew(cred) (time.Duration, bool)` | 多早算"该刷新凭证" |
| `CredentialExpiryExt` | `TokenExpiry(cred) (int64, bool)` | 凭证到期时刻（**Unix 秒**，不是毫秒） |
| `CredentialLifetimeExt` | `NeverExpires(cred) bool` | 凭证**设计上**不过期（拿不准必须返回 false） |
| `CredentialTokenExt` | `HasToken(cred) bool` | 这份凭证有没有 access token |
| `ErrorClassifier` | `Classify(status int, body string) ErrorKind` | **我的**错误怎么分类（见 §5） |
| `HealthProbeExt` | `ProbeHealth(ctx, cred) error` | 主动健康检查 |
| `ImageGenExt` | `GenerateImage(ctx, uid, body) ([]byte, int, error)` | 生图 |
| `ImageModelExt` | `ImageModels() []ImageModel` | 生图模型目录 |
| `ChatToolExt` | `ChatTools() / ExecuteChatTool(...)` | 对话时可注入的工具 |
| `DailyActionExt` | `DailyActions() []DailyAction` | 「签到/保活/领福利」这类每日动作（前端按它画按钮） |
| `DailyCheckinExt` | `CheckinAll(ctx) DailyCheckinReport` | 全量执行每日动作 |
| `CandidateTierExt` | `CandidateTiers(model, uids) map[string]int` | 同批候选里我更想先用谁 |
| `SoftRateExt` | `SoftRateReset(status, body) (time.Time, bool)` | 限流是否精确到"某模型某时刻" |
| `ModelMultiplierExt` | `ModelMultipliers(ctx, cred) (map[string]float64, error)` | 模型成本倍率（必须是官方值） |

⚠ **扩展点靠类型断言发现，不是访问器方法**。写成 `func (p *Provider) LoginFlow() (LoginFlow, bool)`
**不会被发现** —— 实测有 4 个上游因此在前端一直没有「＋ 添加账号」按钮。
正确写法：把方法集直接挂在 `*Provider` 上（`func (p *Provider) Start() ...`），
让 `p` 本身满足接口。

### 5. 错误分类（`ErrorKind`，跨上游中立词汇）

`ErrKindNone` · `ErrKindHardCredit`（长冷却到下次重置）· `ErrKindSoftRate`（短冷却）·
`ErrKindSessionDead`（**永久禁用，需人工重登**）· `ErrKindNotFound`（短冷却、不累计错误）·
`ErrKindAuth`（可自动续期，**只换号不罚**）· `ErrKindServer`（喂熔断）·
`ErrKindClient`（只换号不罚）· `ErrKindContentBlocked`（不罚号，换提示词重试）·
`ErrKindFatalClient`（确定性错误：**不换号、直接结束**）

- **只追加，不插入** —— 取值会写进历史日志，插入会让所有既有值位移（有测试钉住）。
- 不实现 `ErrorClassifier` 不算错，核心回落 `DefaultErrorKind`（按状态码）。
- 判例：codearts 的 `insufficient quota` 必须归 `HardCredit`；workbuddy 的裸数字 `12153`
  曾被拿去判 codearts 的错误体，把**健康账号永久禁用**——上游的错误码只能由上游自己解释。

### 6. 配置段契约（有反射测试自动守着）

给上游加配置段时**三件套缺一不可**，少一件都不会编译失败、不会报警，只会静默不生效：

1. `Config` 里加结构体字段，至少含 `auth_dir`（`json:"auth_dir"`）
2. `normalize()` 里加解析块：`<Name>Enabled`（bool）、`<Name>AuthDir`（必须是 `AuthsBase` 的**一级**子目录）、`<Name>PoolAccounts`
3. 启动日志逐个上游说明它为什么开着/关着

- 缺省语义：**段缺席 = 启用**（`upstreamEnableDefault = true`，用户明确要求"默认全部启动"）。
- `cmd/server/config_provider_sections_test.go` 用反射枚举「所有含 `auth_dir` 的段」并逐条断言，
  所以新增段忘了写解析块**会自动变红**；它的 `len(out) < 8` 是枚举机制的存活检查，不是上游清单。

### 7. 加一个上游：完整清单（照着做，一步都不要跳）

```text
1  internal/<id>/            建包：provider.go 实现 4 个方法 + providerID/ProviderID 常量
2  internal/<id>/credential.go   LoadDir / ParseCredential / 投影出 UID
3  internal/<id>/contract_hermetic_test.go
       func TestContract(t *testing.T) { gateway.RunProviderContract(t, NewProvider) }
4  cmd/server/config.go       加配置段（含 auth_dir）+ normalize 解析块
5  cmd/server/<id>creds.go    syncXxxAccounts(p, dir)：LoadDir → 投影 (auths, secrets) → SyncToDirWithSecrets
6  cmd/server/main.go         装配 + 一行 registry.Register(xxx)（这是**唯一**允许的核心改动点）
7  internal/gateway/arch_test.go
       若该包不依赖 gateway（纯 SDK），登记 discoveryExemptPackages；
       依赖 gateway 就什么都不用做（自动被发现），并被约束测试看住
8  README.md / CHANGELOG.md  上游清单 + 能力位 + 一句"它和别家不同在哪"
9  验证：go build ./... && go vet ./... && go test ./... -count=1
10 真机：起隔离实例（另选端口 + 独立 config/auths/data）跑一条真实 /v1/chat/completions
```

### 8. 硬约束（`arch_test.go` 用 `go list -deps` 判，不是文本匹配）

- 核心包（`gateway/pool/logbuf/admin/server/scheduler/checkinlog/clientlogin`）**不得**依赖任何具体上游
- 上游**不得**依赖核心业务包，上游之间**不得**互相依赖
- 判据是**包级依赖图**，不是源码字面串 —— 历史上文本判据被 import 别名绕过两次，别再改回文本
- `discoveryExemptPackages`（`auth/checkinlog/clientlogin/logbuf/oauth/redisstore/session/upstream`）
  是"**不可能是上游**"的证明义务：谁开始 import `gateway` 就立刻红
- `upstream` 是 workbuddy 的**私有 SDK**（`workbuddyPrivateSDK`）：除 workbuddy 外任何上游
  依赖它都会红 —— 否则一个上游的反指纹改写会作用在另一个上游的流量上
- 被 ≥2 个上游依赖的包必须**真的是共享的**

### 9. 易踩的坑（都是实测踩出来的）

- `New()` 通常是"建 HTTP 客户端"，Provider 构造函数要叫 `NewProvider()` / `NewWithConfig()`，同名会编译冲突
- 池子投影**只给 `{UID, Nickname, FilePath}`**，别顺手抄 `ExpiresAt`：投影是启动快照，
  活 secret 续期后投影不会跟新，喂一个会过期的假值比 0 更糟。过期权威只走 `CredentialExpiryExt`
- 凭证 `expiresAt` 常见是 **Unix 秒**，按毫秒读会把 2027 年的号全判成"已过期"（实际踩过）
- `filepath.Glob(filepath.Join("", "xxx*.json"))` 在 `dir` 为空时退化成**相对 CWD** 的模式，
  会读到工作目录下的同名文件 —— 空目录要先判空
- `*.md` 在 `.gitignore` 里是全局忽略的：新增**交付物文档**必须逐条 `!docs/xxx.md` 放行；
  被 `go:embed` 的 `.md` 忘放行会导致 **CI 干净签出构建失败**（本机看不到）
- `Models()` **必须能在无有效凭证时返回非空列表**：契约测试默认拿伪造凭证跑，
  真实上游会因凭证无效报错 —— 一上来 `return nil, err` 会被判成"声明了 CapModels 却没实现"。
  正确做法是 `a, _ := authOf(cred)` + 远端失败时回兜底目录（就是 §10 骨架里那两行）
- 依赖铁律：`go.mod` 直接依赖只有 `go-redis`（`wazero` 是 qoder WASM 的唯一破例），
  引入新依赖前先想清楚会不会破坏「Release 下载即用」

### 10. 最小可运行骨架（复制这份就能编译）

```go
// internal/example/example.go
package example

// providerID 会被用作模型名前缀（"example/xxx"）、配置 key、统计维度。
// 格式受 gateway 约束（^[a-z][a-z0-9-]*$），有契约测试守着。
const providerID = "example"

// ProviderID 导出给装配层（cmd/server）用 —— 避免 cmd 里再写一遍字面量。
const ProviderID = providerID

// DefaultAPIBase 上游基址。留成变量而不是写死在 client 里，方便测试与自建反代。
const DefaultAPIBase = "https://api.example.com"
```

```go
// internal/example/provider.go
package example

import (
    "bytes"
    "context"
    "errors"
    "fmt"
    "io"

    "workbuddy2api/internal/gateway"
)

type Provider struct {
    client  *Client
    authDir string
}

// Config 全部可缺省：零值 Config 必须给出一个"能跑契约测试的最小实例"。
type Config struct {
    Client  *Client // nil → New()
    APIBase string  // Client 为 nil 时用它构造
    AuthDir string  // 供 gateway.AuthDirExt
}

// NewProvider 契约测试入口。⚠ 不能叫 New：本包的 New() 是 HTTP 客户端。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

func NewWithConfig(cfg Config) *Provider {
    c := cfg.Client
    if c == nil {
        c = New()
        if cfg.APIBase != "" {
            c.APIBase = cfg.APIBase
        }
    }
    return &Provider{client: c, authDir: cfg.AuthDir}
}

func (p *Provider) ID() string { return providerID }

// Caps 只声明**可验证**的。多报会被契约测试当场抓住（画假按钮）。
func (p *Provider) Caps() gateway.Capability {
    return gateway.CapChat | gateway.CapModels
}

// AuthDir 实现 gateway.AuthDirExt（读与写必须问同一个目录）。
func (p *Provider) AuthDir() string { return p.authDir }

// Chat 转发一次对话。**只带流回来，不写 ResponseWriter**（编码是出口层的事）。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
    a, err := authOf(cred)
    if err != nil {
        return gateway.ChatStream{}, err
    }
    if err := ctx.Err(); err != nil { // 契约：ctx 取消要尽快返回
        return gateway.ChatStream{}, err
    }
    rc, status, respBody, err := p.client.ChatStream(ctx, a, PrepareBody(body))
    if err != nil {
        // 上游返回了错误体 = 业务错误，仍按流返回（让核心分类器判）
        if len(respBody) > 0 {
            return gateway.ChatStream{Status: status, Body: io.NopCloser(bytes.NewReader(respBody))}, nil
        }
        return gateway.ChatStream{}, err
    }
    return gateway.ChatStream{Status: status, Body: rc}, nil
}

func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
    // ⚠ 两个"必须"（我按最小骨架跑契约测试，这两条各红过一次）：
    //   ① 无凭证也要能返回目录（契约默认用伪造凭证跑）→ a, _ := authOf(cred)
    //   ② 远端目录拿不到时必须回**非空**兜底目录，返回空列表会被判"声明了却没实现"
    a, _ := authOf(cred)
    ms, err := p.client.Models(ctx, a)
    if err != nil {
        return nil, err
    }
    if len(ms) == 0 {
        return []gateway.ModelInfo{{ID: "example-default"}}, nil
    }
    out := make([]gateway.ModelInfo, 0, len(ms))
    for _, m := range ms {
        out = append(out, gateway.ModelInfo{ID: m.ID, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxTokens})
    }
    return out, nil
}

// authOf 把不透明 Secret 断言回本包的凭证类型。断言失败给**明确错误**，不 panic。
func authOf(cred gateway.Credential) (*Auth, error) {
    if cred.Secret == nil {
        return nil, errors.New("example: 凭证为空（Credential.Secret 未设置）")
    }
    a, ok := cred.Secret.(*Auth)
    if !ok {
        return nil, fmt.Errorf("example: 凭证类型不对，期望 *example.Auth，实际 %T", cred.Secret)
    }
    if a == nil || a.Token == "" {
        return nil, errors.New("example: 凭证缺少 token（唯一鉴权材料）")
    }
    return a, nil
}
```

```go
// internal/example/contract_hermetic_test.go
package example

import (
    "testing"

    "workbuddy2api/internal/gateway"
)

// TestContract 行为契约。**不碰网络**：用伪造凭证跑，网络失败被容忍。
func TestContract(t *testing.T) {
    gateway.RunProviderContract(t, NewProvider)
}
```

```go
// cmd/server/examplecreds.go —— 并池（上游包不依赖 internal/pool，装配层做投影）
package main

import (
    "log"

    "workbuddy2api/internal/auth"
    "workbuddy2api/internal/example"
    "workbuddy2api/internal/pool"
)

func dedupeExampleByUID(list []*example.Auth) ([]*auth.Auth, map[string]any) {
    auths := make([]*auth.Auth, 0, len(list))
    secrets := make(map[string]any, len(list))
    for _, ea := range list {
        if ea == nil || ea.UID == "" {
            continue // 没有 uid 的号进池子只会变成幽灵号
        }
        secrets[ea.UID] = ea
        // ⚠ 只投影 {UID, Nickname, FilePath}：别顺手抄 ExpiresAt（见 §9）
        auths = append(auths, &auth.Auth{UID: ea.UID, Nickname: ea.Nickname, FilePath: ea.FilePath})
    }
    return auths, secrets
}

func syncExampleAccounts(p *pool.Pool, dir string) int {
    list, err := example.LoadDir(dir)
    if err != nil {
        log.Printf("example: 读取凭证目录失败（账号池未并入）: %v", err)
        return 0
    }
    if len(list) == 0 {
        p.SyncToDirWithSecrets(example.ProviderID, nil, nil) // 空 = 全集为空，会剔掉已删除的号
        return 0
    }
    auths, secrets := dedupeExampleByUID(list)
    p.SyncToDirWithSecrets(example.ProviderID, auths, secrets)
    return len(auths)
}
```

## 验证命令（CI 三件套，本地与 CI 完全一致）

```bash
go build ./...
go vet ./...
go test ./... -count=1
```

- `go test -race` 在 CI 里是 **advisory**（`continue-on-error: true`）。
  本机无 gcc，实测 zig 当 C 编译器时能编译但**链接失败**
  （mingw 导入库缺 `WaitOnAddress`）——**跑不了，别假装跑过**。
- 前端静态套件：`node tests/frontend/run_frontend_suites.js`
- 真浏览器 e2e：`node tests/frontend/accounts_e2e.js` 等（raw CDP，需本机 Chrome）

## 铁律（全部由 `internal/gateway/arch_test.go` 可执行断言守住，不是约定）

- **核心包不得依赖任何具体上游** — `corePackages` 含 gateway / pool / logbuf /
  admin / server / scheduler / checkinlog / clientlogin
- **上游不得依赖核心业务包**，也**不得依赖另一个上游**（上游之间必须独立）
- **新增上游不需要登记任何名单** —— 上游由包级依赖图**自动发现**（只要它 import 了
  `internal/gateway`）。`arch_test.go:45` 的 `upstreamPrefix` 是**遗留变量，已无人使用**，
  别照着它去"登记"（历史上有过三版名单/文本判据，全被绕过，见该文件 268 行的复盘）
- 加新上游的正确姿势：`cmd/server/main.go` 里**只加一行 `registry.Register(...)`**，核心包一行不改
- 判据是**包级依赖图**（`go list -deps`），不是源码文本匹配 —— 历史上文本判据被
  import 别名绕过过两次，别再改回文本匹配
- 被 ≥2 个上游依赖的包必须**真的是共享的**（`TestSharedInfraIsGenuinelyShared`）

## 依赖铁律

- `go.mod` 直接依赖只有 **go-redis**。`wazero` 是 qoder WASM 加密签名的唯一破例
  （纯标准库无法执行 WASM，无替代），已做架构隔离：`internal/qoderwasm` **不依赖**
  `internal/gateway`，否则会被架构测试误判为上游
- 引入新依赖前先想清楚会不会破坏「Release 下载即用」

## 前端视觉层（v3 · MAXIMALISM）

- **唯一真相是 `internal/server/theme-max.css`**，六个页面里的同款 CSS 是
  `node tools/theme_sync.js` 内联进去的（幂等；`--check` 只校验）。
  手改页面里的那段 CSS 会在下次同步时被覆盖 —— 改视觉只改那一份。
- 改完必须重建 `bin\wb2api-server.exe`：六个页面走 `//go:embed`，不重建
  浏览器拿到的还是旧页面（最容易被误判成"前端 bug"）。
- 分层口径：**外壳吵、数据静**。表格内容区 / 表单 / 日志行只许墨线与等宽字，
  不铺色块、不叠阴影；颜色与纹理只上在页头、侧栏、分区标题带、统计卡、分组头、弹窗。
- **动任何颜色/底色都必须跑全量对比度巡检**：
  `AUDIT_URL=<实例>/ui AUDIT_USER=.. AUDIT_PASS=.. node tests/frontend/audit_contrast_pages.js`
  期望 0 处、exit 0。抽样判据（contrast_e2e 只量 5 个元素）**证明不了整页看得清** ——
  亮底上的次要文字正是从这条缝里漏出去的（实测 1.49:1）。
- 三条会被判红的边界：亮色块上的文字必须用墨（`#120F0D`）不用白；深色主题下
  `.card` 必须仍是深底（`final_visual`）；侧栏 active 底色只能是 `--acc`（`p4_badge_check`）。

## 运维：二进制替换与 7863 重启（**已获长期授权，直接做，不用问**）

用户 2026-10-21 明确授权：「以后你自己替换 exe」。

标准动作（改完代码、验证通过后一次做完）：

```powershell
cd D:\project_GIT\workbuddy2api
# 1) 先构建到**不被占用**的名字：直接 -o bin\wb2api-server.exe 会因进程占用而失败，
#    而失败后脚本若继续跑，"验证"用的还是旧二进制（2026-10-21 实际踩过，见 progress-log）
go build -p 1 -o bin\wb2api-server-next.exe ./cmd/server
# 2) 停 → 换 → 启
$p = (Get-NetTCPConnection -State Listen -LocalPort 7863).OwningProcess
Stop-Process -Id $p -Force; Start-Sleep -Seconds 2
Move-Item -Force bin\wb2api-server-next.exe bin\wb2api-server.exe
Start-Process cmd.exe -ArgumentList '/c','start','start.bat' -WorkingDirectory 'D:\project_GIT\workbuddy2api'
```

- 启动必须走 `start.bat`（它 cd 到仓库根）：直接跑 `bin\wb2api-server.exe` 会让工作目录变成
  `bin\`，config.json / auths / data 全部落空。
- 换完**必须实测**一条真实请求（`/v1/chat/completions`）再报告完成 —
  "进程起来了"不等于"改动生效"。
- 7863 是用户自用实例；其余端口（7864 等）仍按"用完即停"。
- **go:embed 的页面、出站请求头、模型目录都在二进制里**：不重启就永远看不到改动，
  这一步不能省。

## 沉淀规则

较大迭代后（口径/结论变更、新增或重构脚本、重要决策或否决、新的数据坑、
交付物大版本更新）按此格式沉淀：

**决策 + 为什么 + 被否决的替代方案 + 如何复验** —— 否决也要记，否则会被重新提议。

- 稳定事实 → `handoff.md`；时间线 → `docs/progress-log.md`（只追加，不重写）
- 沉淀动作顺序：progress-log 追加一条（含 `[决策]`/`[否决]` 标记）→ 同步 handoff
  受影响段落 → 若引入新铁律则更新本文件
- **Next Steps 只写用户明确确认过的事项**。agent 自行推断的候选动作一律进
  `docs/open-questions.md` 标「建议·未确认」—— 否则下一个 agent 会把猜测当既定任务执行

## 冲突仲裁

同一事实在不同文件冲突时，信任顺序：
**脚本实算 > 代码/测试 > `handoff.md` > `README.md` > `docs/progress-log.md`（历史）**

能被脚本重新算出来的数字，不要在散文里硬编码。

## 按需生长文件（不预建空壳）

| 文件 | 触发条件 |
|---|---|
| `docs/open-questions.md` | 出现第一个影响结论的未决问题 |
| 架构 / 接口契约笔记 | 代码新增多个模块或对外接口 |

## 边界

- 凭证、token、cookie、私钥**一律不进任何记忆文件**
- 读写只在授权根内；路径引用超出范围时记为「未核」，不自动跟随