#!/usr/bin/env bash
# =============================================================
#  一键构建并启动容器
#  用法：
#    export DEEPSEEK_API_KEY=sk-xxxxxxxx
#    bash docker-run.sh
#
#  脚本会：停止并删除同名旧容器 → 重新构建镜像 → 后台启动
#  端口固定为 8082（改 PORT 变量可换）
# =============================================================
set -e

PORT=8082
IMAGE=ai-chat
CONTAINER=ai-chat

# 1. 检查令牌
if [ -z "$DEEPSEEK_API_KEY" ]; then
  echo "❌ 未设置 DEEPSEEK_API_KEY"
  echo "   请先执行： export DEEPSEEK_API_KEY=sk-xxxxxxxx"
  echo "   或写到 ~/.bashrc 里永久生效"
  exit 1
fi

# 2. 构建镜像（上下文必须是项目根目录）
echo ">>> 构建镜像 $IMAGE ..."
docker build -t "$IMAGE" .

# 3. 清理旧容器
if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
  echo ">>> 删除旧容器 $CONTAINER"
  docker rm -f "$CONTAINER" >/dev/null
fi

# 4. 启动：端口映射 + 注入令牌（令牌只在运行期注入，不进镜像层）
echo ">>> 启动容器，端口 ${PORT} -> ${PORT}"
docker run -d \
  --name "$CONTAINER" \
  --restart=always \
  -p "${PORT}:${PORT}" \
  -e "DEEPSEEK_API_KEY=${DEEPSEEK_API_KEY}" \
  -e "DEEPSEEK_MODEL=${DEEPSEEK_MODEL:-deepseek-flash}" \
  -e "RATE_LIMIT_PER_MIN=${RATE_LIMIT_PER_MIN:-20}" \
  "$IMAGE"

sleep 2
echo ""
echo ">>> 容器状态："
docker ps --filter "name=${CONTAINER}" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"

echo ""
echo ">>> 健康检查："
curl -s "http://127.0.0.1:${PORT}/api/health" && echo "" || echo "  （curl 未安装或尚未就绪）"

echo ""
echo "🎉 完成！浏览器打开： http://<你的IP>:${PORT}/"
echo "   查看日志： docker logs -f ${CONTAINER}"
