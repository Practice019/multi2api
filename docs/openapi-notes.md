# docs/openapi.yaml 说明（契约基线）

本文件是 `docs/openapi.yaml` 的配套说明。契约由**源码**抽取而成，不手写凭印象的字段。

## 覆盖范围

- **对外 v1（4 条，字段级详写）**：`POST /v1/chat/completions`、`POST /v1/images/generations`、`GET /v1/models`、`GET /v1/model`
- **探针 / 状态（2 条，详写）**：`GET /healthz`、`GET /status`
- **管理端核心组（字段级详写）**：`/admin/settings`（GET + PUT）、`/admin/system-status`、`/admin/channels`（GET + POST 及 delete/status/reveal/discover/keys/*/test）、`/admin/accounts`、`/admin/models/preview`、`/admin/stats`
- **管理端其余端点（存在性占位）**：路径从源码 `h.register("METHOD /admin/...")` 抽取，只列 path + summary + 通用错误响应；字段级契约尚未逐项核对
- **控制台 JSON 取数**：`GET /ui/models/data` 等列了占位；`/ui`、`/ui/`、`/{$}`、`/favicon.ico`、`/favicon.svg` 是 **HTML 页面 / 静态图标**，不属于 API 契约，故不收录

## "端点存在性占位"是什么意思

该操作的 `description` 里带这句话时，**只保证路径与 HTTP 方法存在**（从源码路由表抽取），
请求/响应字段未逐项核对，因此响应只声明为对象，不虚构 schema。
要升级为字段级契约：读对应 handler 的 `writeJSON` 与请求结构体后再补。

## 字段来源（真实性纪律）

- 请求/响应字段全部来自 Go struct 的 json tag 或 handler 里 writeJSON 的字面量，
  例如 `channelReq`（internal/admin/channels.go）、`channelstore.Channel`、
  `writeOpenAIError`（`{"error":{message,type,code}}`）与 `admin.writeError`（`{"error":"..."}`）
- 错误信封因此是 `oneOf`：**v1 用对象形态、管理端用字符串形态** —— 两种都真实存在

## 契约与实现之间的已知不一致 / 易误读点

1. `GET /v1/model` 与 `GET /v1/models` 在源码里指向**同一个 handler**：
   `/v1/model` 并不是"按 id 取单个模型"，名字容易被读成那样。
2. `POST /admin/channels` 的 `keys` 在**请求体里是字符串数组**，
   而落盘后的结构是对象数组（含 status/reason/disabled_at）—— 两者形状不同。
3. `GET /admin/settings` 在设置存储未接线时返回 **501**（不是 500）。
4. `POST /v1/chat/completions` 与生图端点都会返回 **413**（请求体超过 `server.max_body_mb`），
   契约里显式声明了它。
5. `/healthz` **恒无鉴权**且 503 时**带响应体**；`/status` 走控制台鉴权（管理令牌也可）。
6. 管理端未登记字段是 **fail-closed**：`POST /admin/channels` 带了没登记的字段会 400 并列出字段名，
   所以 `ChannelSaveRequest` 标了 `additionalProperties: false`。

## 治理结果（api-governance MCP，openapi 组 469 条规则）

- **error = 0**、**warn = 0**（唯一那条 warn 是 `oas3-unused-component`：
  `components.parameters.CacheControl` 没被引用 —— 已挂到 3 个 GET 端点上，不是删组件糊过去）
- info 数量见交付回报；info 全是**教育性**规则，其中一部分要靠**编造**才能清零（例如
  `RateLimit` / `RateLimit-Policy` / 关联 ID 响应头、`x-agentic-access`、
  每个 operation 必须写成 `components.pathItems` 的 $ref 形态）。本项目没有实现这些响应头，
  所以**不写进契约**：契约要描述现实，不能为了让 lint 变绿而虚构。

## 复现命令

```bash
python -c "import yaml;yaml.safe_load(open('docs/openapi.yaml',encoding='utf-8'))"   # YAML 可解析
go build ./...                                                                        # 未改任何 Go 代码
```
