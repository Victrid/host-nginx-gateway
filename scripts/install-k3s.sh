#!/usr/bin/env bash
# install-k3s.sh — 在 k3s 上从零部署 HostNginxGateway（控制器 DaemonSet + Helm chart）。
#
# 用法（root 或 sudo）：
#   sudo bash scripts/install-k3s.sh [选项]
#
# 选项：
#   --controller-image <repo:tag>   控制器镜像（默认 ghcr.io/victrid/host-nginx-gateway:0.1.0，
#                                   也可用本地导入镜像，见 --local-image）
#   --local-image <tar>             使用 docker save 导出的镜像 tar（离线导入 k3s，无需镜像仓库）
#   --k3s-install                   同时安装 k3s server（从清华镜像下载，--disable traefik/servicelb）
#   --host-nginx-prefix <dir>       host nginx 前缀（默认 /etc/nginx）
#   --nginx-binary <path>           host nginx 二进制（默认 /usr/sbin/nginx）
#   --namespace <ns>                Helm release namespace（默认 kube-system）
#   --node-selector <k=v>           DaemonSet 节点选择器，可重复（默认不限制）
#
# 前置条件（本脚本会检查并提示，不自动安装）：
#   - host nginx 已安装（二进制存在即可，无需已启动 80/443；控制器与既有服务共存）
#   - 用户 nginx.conf 已包含：include /etc/nginx/conf.d/k8s-gw/*.conf;（缺失时控制器会在
#     Gateway 状态中报 Accepted=False (Invalid) 并给出提示行，永不自动修改主配置）
#
# 安全提示：每台主机只能运行一个控制器实例（多实例双写 conf.d/k8s-gw，互相触发 reload 抖动）。
set -euo pipefail

REPO_URL="https://github.com/Victrid/HostNginxGateway"
DEFAULT_IMAGE="ghcr.io/victrid/host-nginx-gateway:0.1.0"
K3S_MIRROR="https://rancher-mirror.rancher.cn"   # k3s 官方国内镜像；清华镜像可用
# K3S_MIRROR="https://mirrors.tuna.tsinghua.edu.cn/github-release/k3s-io/k3s/LatestRelease"

CONTROLLER_IMAGE="$DEFAULT_IMAGE"
LOCAL_IMAGE_TAR=""
INSTALL_K3S=false
NGINX_PREFIX="/etc/nginx"
NGINX_BINARY="/usr/sbin/nginx"
NS="kube-system"
NODE_SELECTOR_ARGS=()

log()  { printf '\033[1;34m[install]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[install] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --controller-image) CONTROLLER_IMAGE="$2"; shift 2 ;;
    --local-image)      LOCAL_IMAGE_TAR="$2"; shift 2 ;;
    --k3s-install)      INSTALL_K3S=true; shift ;;
    --host-nginx-prefix) NGINX_PREFIX="$2"; shift 2 ;;
    --nginx-binary)     NGINX_BINARY="$2"; shift 2 ;;
    --namespace)        NS="$2"; shift 2 ;;
    --node-selector)    NODE_SELECTOR_ARGS+=(--set "nodeSelector.$(echo "$2" | tr '.' 'X' | tr '/' 'Y')=true") ; shift 2 ;;
    -h|--help) sed -n '2,26p' "$0"; exit 0 ;;
    *) die "未知参数: $1（--help 查看用法）" ;;
  esac
done

[[ $EUID -eq 0 ]] || die "请以 root 运行（k3s 安装与 host nginx reload 需要）"

# ---------- 1. （可选）安装 k3s ----------
if $INSTALL_K3S && ! command -v k3s >/dev/null 2>&1; then
  log "安装 k3s server（镜像源: $K3S_MIRROR）..."
  # 不装 traefik/servicelb：把 80/443 留给 host nginx，这正是本项目的部署理念
  curl -sfL "$K3S_MIRROR/k3s-install.sh" \
    | INSTALL_K3S_MIRROR="$K3S_MIRROR" sh -s - server \
        --disable traefik --disable servicelb --write-kubeconfig-mode 600
elif $INSTALL_K3S; then
  log "k3s 已安装，跳过"
fi

export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"
[[ -f "$KUBECONFIG" ]] || die "未找到 kubeconfig: $KUBECONFIG（新装 k3s 需等待数十秒，或用 --kubeconfig 指定）"
KUBECTL="kubectl"
command -v kubectl >/dev/null 2>&1 || KUBECTL="k3s kubectl"

# 等待节点就绪（排除 NotReady）
log "等待节点 Ready..."
for i in $(seq 1 60); do
  if $KUBECTL get nodes -o json | grep -q '"type": "Ready".*"status": "True"'; then break; fi
  [[ $i -eq 60 ]] && die "节点 60s 内未 Ready，检查 k3s 日志: journalctl -u k3s"
  sleep 1
done
$KUBECTL get nodes

# ---------- 2. 前置检查 ----------
[[ -x "$NGINX_BINARY" ]] || die "host nginx 未安装: $NGINX_BINARY 不存在（请先安装 nginx）"
if ! grep -qs "conf.d/k8s-gw" "$NGINX_PREFIX/nginx.conf" ; then
  log "提示: $NGINX_PREFIX/nginx.conf 尚未 include 控制器目录。"
  log "  控制器不会修改你的主配置；在 http block 中加入下面一行后，Gateway 才会被接受："
  log "    include $NGINX_PREFIX/conf.d/k8s-gw/*.conf;"
fi

# ---------- 3. helm（若无则安装） ----------
if ! command -v helm >/dev/null 2>&1; then
  log "安装 helm v3.19.0..."
  curl -fsSL https://get.helm.sh/helm-v3.19.0-linux-amd64.tar.gz \
    | tar -xz -C /usr/local/bin --strip-components=1 linux-amd64/helm
fi

CHART_DIR="$(cd "$(dirname "$0")/../charts/host-nginx-gateway" && pwd)"
[[ -d "$CHART_DIR" ]] || die "未找到 chart: $CHART_DIR（请从 $REPO_URL 完整克隆仓库）"

# ---------- 4. 镜像 ----------
if [[ -n "$LOCAL_IMAGE_TAR" ]]; then
  [[ -f "$LOCAL_IMAGE_TAR" ]] || die "镜像 tar 不存在: $LOCAL_IMAGE_TAR"
  log "离线导入镜像 $LOCAL_IMAGE_TAR ..."
  k3s ctr images import "$LOCAL_IMAGE_TAR"
  CONTROLLER_IMAGE="$(docker load -i "$LOCAL_IMAGE_TAR" >/dev/null 2>&1 \
    && docker inspect --format '{{index .RepoTags 0}}' "$(docker images -q | head -1)" \
    || echo "$CONTROLLER_IMAGE")"
fi

# ---------- 5. helm 安装 ----------
log "helm 安装 host-nginx-gateway (ns=$NS, image=$CONTROLLER_IMAGE)..."
helm upgrade --install host-nginx-gateway "$CHART_DIR" -n "$NS" \
  --set image.repository="${CONTROLLER_IMAGE%%:*}" \
  --set image.tag="${CONTROLLER_IMAGE##*:}" \
  --set nginx.confDir="$NGINX_PREFIX" \
  --set nginx.binary="$NGINX_BINARY" \
  "${NODE_SELECTOR_ARGS[@]}"

# ---------- 6. 等待 DaemonSet 就绪 ----------
log "等待控制器 DaemonSet Ready..."
$KUBECTL -n "$NS" rollout status ds/host-nginx-gateway --timeout=180s \
  || die "DaemonSet 未就绪：$KUBECTL -n $NS describe ds host-nginx-gateway"

log "完成。部署摘要："
$KUBECTL -n "$NS" get ds,po -l app.kubernetes.io/instance=host-nginx-gateway 2>/dev/null \
  || $KUBECTL -n "$NS" get ds,po | grep -i gateway
echo
echo "下一步："
echo "  1. 在 $NGINX_PREFIX/nginx.conf 的 http block 确认有:"
echo "       include $NGINX_PREFIX/conf.d/k8s-gw/*.conf;"
echo "  2. 创建 Gateway/HTTPRoute（见 e2e/manifests/ 示例），Host 头路由即刻生效。"
echo "  3. 卸载: helm uninstall host-nginx-gateway -n $NS"
