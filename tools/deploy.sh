#!/usr/bin/env bash
# Usage: bash tools/deploy.sh [--pull] [--no-cache]
# Run from any directory; Compose uses this checkout's existing configuration.
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
PULL=0
NO_CACHE=0
for arg in "$@"; do
  case "$arg" in
    --pull) PULL=1 ;;
    --no-cache) NO_CACHE=1 ;;
    -h|--help)
      echo '用法：bash tools/deploy.sh [--pull] [--no-cache]'
      echo '默认构建当前代码；--pull 从当前分支跟踪的远端快进更新。'
      echo '仅更新 grok2api 服务，保留现有 Compose 配置、数据挂载及其他服务。'
      exit 0 ;;
    *) echo "未知参数：$arg" >&2; exit 2 ;;
  esac
done
for command in git docker awk mktemp; do
  command -v "$command" >/dev/null || { echo "缺少命令：$command" >&2; exit 1; }
done
docker compose version >/dev/null
docker info >/dev/null
WAIT_SECONDS=${DEPLOY_WAIT_SECONDS:-180}
[[ "$WAIT_SECONDS" =~ ^[1-9][0-9]*$ ]] || { echo 'DEPLOY_WAIT_SECONDS 必须为正整数' >&2; exit 2; }

mkdir -p .tmp/deploy
LOCK=.tmp/deploy/lock
mkdir "$LOCK" 2>/dev/null || { echo "已有部署锁：$ROOT/$LOCK；确认无部署运行后再删除该目录。" >&2; exit 1; }
trap 'result=$?; rmdir "$LOCK" 2>/dev/null || true; exit "$result"' EXIT

if (( PULL )); then
  # Never stash/reset the server's customized Compose files automatically.
  if ! git diff --quiet || ! git diff --cached --quiet; then
    echo '工作区有修改，未执行拉取。请先处理修改，或不带 --pull 构建当前代码。' >&2
    exit 1
  fi
  git rev-parse --abbrev-ref '@{upstream}' >/dev/null
  git pull --ff-only
fi

STAMP=$(TZ=Asia/Shanghai date +%Y%m%d-%H%M%S)
REV=$(git rev-parse --short=8 HEAD)
# Include the build time so rebuilding the same commit never moves a rollback tag.
IMAGE="grok2api:build-${REV}-${STAMP}"
if ! git diff --quiet || ! git diff --cached --quiet; then
  IMAGE="${IMAGE}-dirty"
  echo '提示：本次构建包含本地修改，镜像标签已标记 dirty。'
fi
export GROK2API_IMAGE="$IMAGE"
docker compose config --quiet
CONTAINER=$(docker compose ps -a -q grok2api)
BACKUP=''
ROLLBACK=''
if [[ -n "$CONTAINER" ]]; then
  OLD_IMAGE=$(docker inspect --format '{{.Image}}' "$CONTAINER")
  BACKUP="grok2api:backup-${STAMP}"
  docker tag "$OLD_IMAGE" "$BACKUP"
  echo "旧镜像已备份：${BACKUP}（${OLD_IMAGE}）"
fi

# Save the image choice without discarding other .env settings.
persist_image() {
  local target=$1 tmp
  tmp=$(mktemp "$ROOT/.env.deploy.XXXXXX")
  chmod 600 "$tmp"
  if [[ -f .env ]]; then
    awk '!/^[[:space:]]*(export[[:space:]]+)?GROK2API_IMAGE[[:space:]]*=/' .env > "$tmp"
  fi
  printf '\nGROK2API_IMAGE=%s\n' "$target" >> "$tmp"
  mv -- "$tmp" .env
}

if [[ -n "$BACKUP" ]]; then
  ROLLBACK="$ROOT/.tmp/deploy/rollback-${STAMP}.sh"
  {
    echo '#!/usr/bin/env bash'
    echo 'set -Eeuo pipefail'
    printf 'cd -- %q\n' "$ROOT"
    declare -f persist_image
    printf 'ROOT=%q\n' "$ROOT"
    printf 'export GROK2API_IMAGE=%q\n' "$BACKUP"
    printf 'docker compose up -d --no-deps --no-build --pull never --force-recreate --wait --wait-timeout %q grok2api\n' "$WAIT_SECONDS"
    printf 'persist_image %q\n' "$BACKUP"
  } > "$ROLLBACK"
  chmod 700 "$ROLLBACK"
  echo "回滚命令：bash '$ROLLBACK'"
fi

BUILD_ARGS=(build)
if (( NO_CACHE )); then BUILD_ARGS+=(--no-cache); fi
echo "构建新镜像：$IMAGE"
docker compose "${BUILD_ARGS[@]}" grok2api
BUILT_ID=$(docker image inspect --format '{{.Id}}' "$IMAGE")
if ! docker compose up -d --no-deps --no-build --pull never --force-recreate \
  --wait --wait-timeout "$WAIT_SECONDS" grok2api; then
  echo '部署或健康检查失败，未修改 .env 的镜像设置。' >&2
  docker compose logs --tail=80 grok2api >&2 || true
  [[ -z "$ROLLBACK" ]] || echo "回滚命令：bash '$ROLLBACK'" >&2
  exit 1
fi
CONTAINER=$(docker compose ps -q grok2api)
RUNNING_ID=$(docker inspect --format '{{.Image}}' "$CONTAINER")
if [[ "$RUNNING_ID" != "$BUILT_ID" ]]; then
  echo '运行镜像与构建镜像不一致，请检查 Compose override 的 image 设置。' >&2
  [[ -z "$ROLLBACK" ]] || echo "回滚命令：bash '$ROLLBACK'" >&2
  exit 1
fi
persist_image "$IMAGE"
printf '部署成功：%s\n镜像 ID：%s\n镜像标签已保存到 .env。\n' "$IMAGE" "$RUNNING_ID"
docker compose ps grok2api
