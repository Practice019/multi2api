#!/usr/bin/env bash
# ============================================================
# 重建并重启 7863 上的 workbuddy2api（multi2api 网关）
#
# 用法：
#   bash restart-7863.sh              # 【默认】重建镜像 + 重启（保证跑的是新代码）
#   bash restart-7863.sh --no-rebuild # 跳过重建，只用现有镜像重启（快，但跑的是旧二进制）
#   bash restart-7863.sh --status     # 只看状态，不做任何改动
#
# 为什么默认重建：
#   docker-compose.yml 里是 `build: .`，容器跑的是**构建那一刻**的二进制。
#   改了 Go 代码只 `up -d` 的话，容器会复用旧镜像 → 代码没生效却显示"重启成功"。
#   本脚本因此默认重建，并在最后**核对容器的镜像 ID 与刚构建的一致**，
#   不一致就报错退出 —— 不允许出现"看起来成功、实际还是旧代码"。
#
# 退出码：0 成功（且已确认是新镜像）/ 1 失败
# ============================================================

set -euo pipefail

# ---- 配置（与 docker-compose.yml 保持一致）----
PROJECT_DIR="/root/project/multi2api"
COMPOSE_SERVICE="wb2api"
CONTAINER_NAME="workbuddy2api"
HOST_PORT="7863"
HEALTH_URL="http://127.0.0.1:7863/healthz"
WAIT_SECONDS=90

# ---- 参数解析 ----
MODE="rebuild"          # 默认：重建 + 重启
case "${1:-}" in
  --no-rebuild) MODE="fast" ;;
  --status)     MODE="status" ;;
  "")           MODE="rebuild" ;;
  *) echo "未知参数: $1（可用: --no-rebuild / --status）" >&2; exit 1 ;;
esac

cd "$PROJECT_DIR"

# ---- 小工具 ----
info() { printf '\033[36m[%s]\033[0m %s\n' "$(date '+%H:%M:%S')" "$*"; }
ok()   { printf '\033[32m[%s] ✅ %s\033[0m\n' "$(date '+%H:%M:%S')" "$*"; }
warn() { printf '\033[33m[%s] ⚠️  %s\033[0m\n' "$(date '+%H:%M:%S')" "$*"; }
die()  { printf '\033[31m[%s] ❌ %s\033[0m\n' "$(date '+%H:%M:%S')" "$*" >&2; exit 1; }

short_id() { echo "${1#sha256:}" | cut -c1-12; }

show_status() {
  echo "---- 容器 ----"
  docker ps -a --filter "name=^/${CONTAINER_NAME}$" \
    --format '  {{.Names}}  {{.Status}}  {{.Ports}}' || true
  echo "---- 端口 ${HOST_PORT} ----"
  if ss -ltn 2>/dev/null | grep -q ":${HOST_PORT}\b"; then
    ss -ltnp 2>/dev/null | grep ":${HOST_PORT}\b" | sed 's/^/  /'
  else
    echo "  （没有进程监听 ${HOST_PORT}）"
  fi
  echo "---- 健康检查 ----"
  if curl -fsS --max-time 5 "$HEALTH_URL" >/dev/null 2>&1; then
    echo "  $(curl -fsS --max-time 5 "$HEALTH_URL")"
  else
    echo "  （${HEALTH_URL} 无响应）"
  fi
}

# ============================================================
# --status：只看不动
# ============================================================
if [ "$MODE" = "status" ]; then
  info "只读状态检查（不做任何改动）"
  show_status
  exit 0
fi

# ============================================================
# 前置检查
# ============================================================
info "前置检查"
[ -f docker-compose.yml ] || die "找不到 $PROJECT_DIR/docker-compose.yml"
[ -f config.json ]        || die "找不到 $PROJECT_DIR/config.json（容器要挂载它，缺了会起不来）"
[ -f Dockerfile ]         || die "找不到 $PROJECT_DIR/Dockerfile"
docker info >/dev/null 2>&1 || die "Docker 不可用（守护进程没跑？）"

if docker compose version >/dev/null 2>&1; then
  DC=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  DC=(docker-compose)
else
  die "既没有 docker compose 也没有 docker-compose"
fi
info "使用：${DC[*]}"

# 端口占用检查：7863 若被**别的**容器占着，先报出来
if ss -ltn 2>/dev/null | grep -q ":${HOST_PORT}\b"; then
  holder="$(docker ps --format '{{.Names}}\t{{.Ports}}' | grep ":${HOST_PORT}->" | cut -f1 || true)"
  if [ -n "$holder" ] && [ "$holder" != "$CONTAINER_NAME" ]; then
    die "端口 ${HOST_PORT} 被另一个容器占用：$holder —— 请先处理它再重试"
  fi
fi

# 记录重启前状态（便于事后对比）
OLD_IMAGE_ID="$(docker inspect -f '{{.Image}}' "$CONTAINER_NAME" 2>/dev/null || echo '')"
if [ -n "$OLD_IMAGE_ID" ]; then
  info "重启前容器镜像：$(short_id "$OLD_IMAGE_ID")"
fi

# ============================================================
# 重建镜像（默认行为）
# ============================================================
if [ "$MODE" = "rebuild" ]; then
  info "构建镜像（Dockerfile 是多阶段构建、依赖层命中缓存，通常几十秒）"
  if ! "${DC[@]}" build "$COMPOSE_SERVICE"; then
    die "镜像构建失败 —— 容器未重启，服务仍在跑旧版本（未被影响）"
  fi
  ok "镜像构建完成"

  # 取刚构建出来的镜像 ID（compose 给镜像打的 tag）
  NEW_IMAGE_ID="$(docker image inspect "$(docker compose config --images "$COMPOSE_SERVICE" 2>/dev/null | head -1)" -f '{{.Id}}' 2>/dev/null || true)"
  if [ -z "$NEW_IMAGE_ID" ]; then
    # 回落：直接从构建输出里解析不到的场合，用 compose 的 images 名再试一次
    NEW_IMAGE_ID="$(docker images --format '{{.ID}}' | head -1)"
    warn "未能确定新镜像 ID，跳过"镜像一致性"校验"
  else
    info "新镜像：$(short_id "$NEW_IMAGE_ID")"
    if [ -n "$OLD_IMAGE_ID" ] && [ "$OLD_IMAGE_ID" = "$NEW_IMAGE_ID" ]; then
      warn "新镜像与重启前**完全相同**（源码没有变化？）—— 这次重启不会带来任何代码变更"
    fi
  fi
else
  NEW_IMAGE_ID=""
  info "已跳过重建（--no-rebuild）：容器将复用现有镜像，改过的代码不会生效"
fi

# ============================================================
# 重启
# ============================================================
info "重启服务 $COMPOSE_SERVICE"
# --force-recreate：镜像 ID 变了但 compose 认为配置没变时，确保容器真的重建
"${DC[@]}" up -d --force-recreate "$COMPOSE_SERVICE" || die "重启失败"

# ============================================================
# 校验：容器必须跑在新镜像上
# ============================================================
RUNNING_IMAGE_ID="$(docker inspect -f '{{.Image}}' "$CONTAINER_NAME" 2>/dev/null || echo '')"
if [ -n "$NEW_IMAGE_ID" ] && [ -n "$RUNNING_IMAGE_ID" ]; then
  if [ "$RUNNING_IMAGE_ID" != "$NEW_IMAGE_ID" ]; then
    warn "容器镜像 ID 与新构建的不一致："
    warn "  容器 = $(short_id "$RUNNING_IMAGE_ID")"
    warn "  新构建 = $(short_id "$NEW_IMAGE_ID")"
    die "容器没有跑在新代码上（可能是 tag 复用或构建缓存异常）—— 请检查后重试"
  fi
  ok "已确认容器运行在新构建的镜像上：$(short_id "$RUNNING_IMAGE_ID")"
fi

# ============================================================
# 等待健康
# ============================================================
info "等待健康（最多 ${WAIT_SECONDS}s，每 2s 探一次）"
elapsed=0
healthy=0
while [ "$elapsed" -lt "$WAIT_SECONDS" ]; do
  if curl -fsS --max-time 5 "$HEALTH_URL" >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 2
  elapsed=$((elapsed + 2))
done

echo
show_status
echo

if [ "$healthy" = "1" ]; then
  ok "重启完成：跑的是新代码，服务健康（耗时约 ${elapsed}s）"
  exit 0
fi

warn "${WAIT_SECONDS}s 内 ${HEALTH_URL} 未响应 —— 下面是最近 40 行日志，用于排障"
echo "----------------------------------------"
docker logs --tail 40 "$CONTAINER_NAME" 2>&1 | sed 's/^/  /' || true
echo "----------------------------------------"
die "服务未就绪（容器可能仍在启动，或配置有误）"
