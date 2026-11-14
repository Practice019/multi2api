# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
# 国内网络环境：默认 proxy.golang.org 经常超时，允许通过 --build-arg 覆盖，
# 缺省走 goproxy.cn 镜像。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src

# 依赖下载层：go.mod/go.sum 不变时命中**层缓存**；
# `--mount=type=cache` 把模块缓存与 Go 编译缓存持久化到 BuildKit 外部缓存 ——
# 即使层缓存失效（改了源码导致 RUN 重跑），也不会重新下载/重新编译未变的包。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# 只 COPY 构建所需的最小集（cmd + internal），并放在两个 RUN 之间：
# 源码一改 → 只有 `go build` 层重跑；配合持久化 GOCACHE → 增量编译，
# 只重编受影响的包（改造前是每次全量重编，实测 68s）。
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
USER app
WORKDIR /app
COPY --from=build /out/wb2api /app/wb2api
# ⚠ 不 COPY config.json：里面含明文 api_key，且 compose/运行时总是挂载覆盖它；
# 缺 config 时 main.go 会自动回落纯默认值启动（config 不存在不是致命错误）。
EXPOSE 7863
# ⚠ 用 /livez 而**不是** /healthz。
#
# Docker 的 HEALTHCHECK 语义与 k8s 的**存活**探针相同：不健康 ⇒ 容器被判死并重启。
# 而 /healthz（以及 /readyz）在**账号池全部冷却**时回 503 —— 那是"暂时接不了新活"，
# 不是"进程病了"。拿它当健康检查的后果是：上游账号一耗尽，容器就被反复重启，
# 而在途请求被全部打断、日志被重启噪音淹没，且重启并不能让账号恢复额度。
#
# 分工：HEALTHCHECK → /livez（进程还在就 200）；编排的**就绪**探针 → /readyz。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/livez || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
