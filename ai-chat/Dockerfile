# ============================================================
#  AI 聊天室 —— 单进程同时提供「前端网页 + 后端 API + WebSocket」
#
#  构建（注意：上下文是项目根目录，不是 backend/）：
#    docker build -t ai-chat .
#
#  运行（令牌从主机环境变量读取）：
#    export DEEPSEEK_API_KEY=sk-xxxxxxxx
#    docker run -d --name ai-chat -p 8082:8082 -e DEEPSEEK_API_KEY=$DEEPSEEK_API_KEY ai-chat
# ============================================================

# ---------- 第 1 阶段：编译 ----------
FROM swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.22-alpine AS builder

WORKDIR /src

# 国内拉依赖慢可解开注释（本项目零第三方依赖，其实用不上）
# ENV GOPROXY=https://goproxy.cn,direct

# 先只拷 go.mod，利用 Docker 层缓存
COPY backend/go.mod ./
# 把 backend 下所有 go 文件都拷进来（新增文件不用再改这里）
COPY backend/*.go ./

# 静态编译：CGO_ENABLED=0 才能在 alpine（musl）里跑起来
RUN CGO_ENABLED=0 go build -trimpath -o /out/ai-backend .

# ---------- 第 2 阶段：运行 ----------
FROM swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/alpine:3.19

# ca-certificates：调用 https://api.deepseek.com 必须，否则 x509 报错
# tzdata：让日志时间显示为中国时区
RUN apk add --no-cache ca-certificates tzdata \
 && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
 && echo "Asia/Shanghai" > /etc/timezone

WORKDIR /app

# 从构建阶段拿二进制；前端静态文件直接拷进镜像
COPY --from=builder /out/ai-backend /app/ai-backend
COPY web/ /app/web/
# 俱乐部知识库：AI 回答社团相关问题时的事实依据（成员去向 / 竞赛战绩 / 纳新流程…）
COPY backend/knowledge.md /app/knowledge.md

# 运行期固定配置（想改端口就改这两处 + docker run -p）
ENV LISTEN_ADDR=0.0.0.0:8082 \
    STATIC_DIR=/app/web \
    CLUB_KB_FILE=/app/knowledge.md \
    AI_WELCOME=on \
    RATE_LIMIT_PER_MIN=20 \
    WS_SEND_BUFFER=8192 \
    WS_BACKLOG_HIGH_WATER=400000 \
    GOMEMLIMIT=600MiB \
    TZ=Asia/Shanghai

EXPOSE 8082

# 健康检查：端口跟随 LISTEN_ADDR（之前写死 8082，改成 80 端口后容器一直显示 unhealthy）
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${LISTEN_ADDR##*:}/api/health" >/dev/null || exit 1

CMD ["/app/ai-backend"]
