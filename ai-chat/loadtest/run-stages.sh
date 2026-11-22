#!/usr/bin/env bash
# =============================================================
#  分级压测：100 → 300 → 500 → 800 → 1000 人，每人 10 条消息
#
#  用法：
#    bash run-stages.sh                      # 用默认阶梯打本机
#    USERS_LIST="1000" bash run-stages.sh     # 只打 1000 人
#    BASE=http://1.2.3.4:8082 bash run-stages.sh
#
#  产物（默认在 results/ 下）：
#    report-<N>.json   结构化压测报告
#    server-<N>.log    该阶段的服务端日志（可查「丢弃消息」）
#    stats-<N>.csv     该阶段容器 CPU/内存采样
# =============================================================
set -uo pipefail
cd "$(dirname "$0")"

BASE=${BASE:-http://127.0.0.1}
USERS_LIST=${USERS_LIST:-"100 300 500 800 1000"}
MSGS=${MSGS:-10}
CONTAINER=${CONTAINER:-ai-chat}
OUTDIR=${OUTDIR:-results}
COOLDOWN=${COOLDOWN:-15}

# 沙箱/受限环境下默认的 Go 构建缓存可能不可写，指到本地目录最稳
export GOCACHE=${GOCACHE:-$PWD/.gocache}

mkdir -p "$OUTDIR" bin

# 等服务端把上一阶段的连接全部回收干净再进下一阶段。
# 不做这一步的话，上一阶段残留的连接会和新阶段叠加，压出来的数字完全不可信。
wait_idle() {
  local n=""
  for _ in $(seq 1 90); do
    n=$(curl -s -m 2 "$BASE/api/health" 2>/dev/null | sed -n 's/.*"online":\([0-9]*\).*/\1/p')
    [ "$n" = "0" ] && return 0
    sleep 1
  done
  echo "  ⚠️ 等待空闲超时（online=${n:-未知}），继续但结果可能受残留连接影响"
}

echo ">>> 编译压测工具"
go build -o bin/loadtest . || { echo "编译失败"; exit 1; }

for U in $USERS_LIST; do
  wait_idle
  echo
  echo "════════════════════════════════════════════════════════"
  echo "   阶段：$U 人 × $MSGS 条   目标：$BASE"
  echo "════════════════════════════════════════════════════════"
  T0=$(date +%Y-%m-%dT%H:%M:%S)

  # 后台每秒采样一次容器资源
  : > "$OUTDIR/stats-$U.csv"
  (
    for _ in $(seq 1 900); do
      docker stats --no-stream --format '{{.CPUPerc}},{{.MemUsage}},{{.MemPerc}}' "$CONTAINER" \
        >> "$OUTDIR/stats-$U.csv" 2>/dev/null || break
      sleep 1
    done
  ) &
  MON=$!

  ./bin/loadtest -base "$BASE" -users "$U" -msgs "$MSGS" \
      -label "stage-$U" -out "$OUTDIR/report-$U.json" \
      -drain-quiet 5s -drain-timeout 300s
  RC=$?

  kill "$MON" 2>/dev/null
  wait "$MON" 2>/dev/null
  T1=$(date +%Y-%m-%dT%H:%M:%S)

  if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    docker logs --since "$T0" --until "$T1" "$CONTAINER" > "$OUTDIR/server-$U.log" 2>&1 || true
    DROPS=$(grep -c '丢弃消息' "$OUTDIR/server-$U.log" 2>/dev/null)
    UP=$(grep -c '\[聊天\]' "$OUTDIR/server-$U.log" 2>/dev/null)
    ENT=$(grep -c '\[进入\]' "$OUTDIR/server-$U.log" 2>/dev/null)
    LEFT=$(grep -c '\[离开\]' "$OUTDIR/server-$U.log" 2>/dev/null)
    echo "  服务端日志 → $OUTDIR/server-$U.log"
    echo "  服务端收到上行聊天 : ${UP:-0} 条（压测端发出 $((U * MSGS)) 条，差值是上行丢失）"
    echo "  服务端进/出房     : ${ENT:-0} / ${LEFT:-0}"
    echo "  服务端「丢弃消息」 : ${DROPS:-0} 次（send 队列满，静默丢）"
    PEAK=$(sort -t, -k3 -rn "$OUTDIR/stats-$U.csv" 2>/dev/null | head -1)
    echo "  容器资源峰值（CPU%,内存,内存%）→ ${PEAK:-无采样}"
  else
    echo "  ❌ 容器 ${CONTAINER} 不在了（可能被 OOM/崩溃干掉了）"
  fi

  [ "$RC" -ne 0 ] && echo "  ⚠️ loadtest 退出码 $RC"

  if [ "$U" != "${USERS_LIST##* }" ]; then
    echo "  冷却 ${COOLDOWN}s ..."
    sleep "$COOLDOWN"
  fi
done

echo
echo ">>> 全部阶段完成，报告在 $OUTDIR/ 下"
ls -1 "$OUTDIR"
