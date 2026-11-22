#!/usr/bin/env bash
# =============================================================
#  重新拉起容器（不重新构建镜像、不跑测试）
#
#  用法：
#    bash restart.sh              # 重建两个容器
#    MODE=restart bash restart.sh # 只是原地重启（更快，配置完全不动）
#
#  说明：
#    · 密钥优先读 backend/env.sh（已被 .gitignore 忽略），其次读现有容器
#    · ai-chat      → host 网络，监听 80   （对外主入口）
#    · ai-chat-test → 8081 -> 8082        （测试入口）
#    · 会自动检查 80 端口有没有被 nginx 之类的服务占着
#      （2026-10-09 就是 nginx 抢了 80，导致容器一直重启循环）
# =============================================================
set -euo pipefail
cd "$(dirname "$0")"

IMAGE=${IMAGE:-ai-chat}
MAIN=${MAIN:-ai-chat}
TEST=${TEST:-ai-chat-test}
MODE=${MODE:-rebuild}

if [ "$MODE" = "restart" ]; then
  echo ">>> 原地重启（配置不变）"
  docker restart "$MAIN" "$TEST"
  sleep 3
  docker ps --format '  {{.Names}}\t{{.Status}}\t{{.Ports}}'
  exit 0
fi

# ---- 80 端口检查：nginx 抢过端口，导致容器一直重启循环过 ----
if ss -ltn 2>/dev/null | grep -q ':80 ' || netstat -ltn 2>/dev/null | grep -q ':80 '; then
  echo "⚠️  80 端口已被占用，尝试停掉 nginx..."
  if systemctl stop nginx 2>/dev/null; then echo "   nginx 已停止"; else echo "   停止失败，请手动处理"; fi
  sleep 1
fi

# ---- 密钥：env.sh > 现有容器 ----
if [ -f backend/env.sh ]; then
  # shellcheck disable=SC1091
  . backend/env.sh
fi
if [ -z "${DEEPSEEK_API_KEY:-}" ]; then
  DEEPSEEK_API_KEY=$(docker inspect "$MAIN" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
    | sed -n 's/^DEEPSEEK_API_KEY=//p')
fi
if [ -z "${DEEPSEEK_API_KEY:-}" ]; then
  echo "❌ 没拿到 DEEPSEEK_API_KEY"
  echo "   请创建 backend/env.sh，内容：export DEEPSEEK_API_KEY=sk-xxxxxx"
  exit 1
fi

echo ">>> 重建 $MAIN（host 网络，监听 80）"
docker rm -f "$MAIN" >/dev/null 2>&1 || true
docker run -d --name "$MAIN" --restart=always --network host \
  -e LISTEN_ADDR=0.0.0.0:80 \
  -e DEEPSEEK_API_KEY="$DEEPSEEK_API_KEY" \
  -e AI_WELCOME=on \
  -e AI_WELCOME_STREAM=on \
  -e CLUB_KB_FILE=/app/knowledge.md \
  -e RATE_LIMIT_PER_MIN=20 \
  -e TZ=Asia/Shanghai \
  -e WS_SEND_BUFFER=8192 \
  -e WS_BACKLOG_HIGH_WATER=400000 \
  -e GOMEMLIMIT=600MiB \
  "$IMAGE" >/dev/null

echo ">>> 重建 $TEST（8081 -> 8082）"
docker rm -f "$TEST" >/dev/null 2>&1 || true
docker run -d --name "$TEST" --restart=always -p 8081:8082 \
  -e LISTEN_ADDR=0.0.0.0:8082 \
  -e DEEPSEEK_API_KEY="$DEEPSEEK_API_KEY" \
  -e AI_WELCOME=on \
  -e AI_WELCOME_STREAM=on \
  -e CLUB_KB_FILE=/app/knowledge.md \
  -e RATE_LIMIT_PER_MIN=20 \
  -e TZ=Asia/Shanghai \
  -e WS_SEND_BUFFER=8192 \
  -e WS_BACKLOG_HIGH_WATER=400000 \
  -e GOMEMLIMIT=600MiB \
  "$IMAGE" >/dev/null

sleep 5
docker ps --format '  {{.Names}}\t{{.Status}}\t{{.Ports}}'
echo
for p in 80 8081; do
  printf '  :%-5s %s\n' "$p" "$(curl -s -m 8 "http://127.0.0.1:$p/api/health" || echo '无响应')"
done
echo
echo "  主入口   : http://<你的IP>/          （登录页有 ICP 备案号）"
echo "  测试入口 : http://<你的IP>:8081/"
echo "  管理后台 : http://<你的IP>/admin     （admin / qwer1234）"
echo "  实时日志 : docker logs -f $MAIN"
