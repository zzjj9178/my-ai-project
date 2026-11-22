# ============================================================
#  后端镜像 —— Go 服务，只提供 API 与 WebSocket
#
#  构建上下文就是本目录（backend/）：
#    docker build -t ai-chat-backend ./backend
#
#  单独运行（调试用；正式部署请用 deploy/docker-compose.yml）：
#    docker run -d --name ai-chat-backend -p 8082:8082 \
#      -e DEEPSEEK_API_KEY=sk-xxxxxxxx ai-chat-backend
# ============================================================

# ---------- 第 1 阶段：编译 ----------
FROM golang:1.22-alpine AS builder

WORKDIR /src

# 国内拉 Go 依赖慢可解开注释（本项目零第三方依赖，其实用不上）
# ENV GOPROXY=https://goproxy.cn,direct

# 先只拷 go.mod，利用 Docker 层缓存（改了代码不用重新下载依赖）
COPY go.mod ./
# 再拷所有 go 文件
COPY *.go ./

# 静态编译：CGO_ENABLED=0 才能在 alpine（musl）里跑起来
RUN CGO_ENABLED=0 go build -trimpath -o /out/ai-backend .

# ---------- 第 2 阶段：运行 ----------
FROM alpine:3.19

# ca-certificates：调用 https://api.deepseek.com 必须，否则 x509 报错
# tzdata：让日志时间显示为中国时区
RUN apk add --no-cache ca-certificates tzdata \
 && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
 && echo "Asia/Shanghai" > /etc/timezone

WORKDIR /app

# 只拷后端需要的东西：二进制 + 知识库
# 网页文件不在这里 —— 由前端容器（Nginx）负责
COPY --from=builder /out/ai-backend /app/ai-backend
COPY knowledge.md /app/knowledge.md

ENV LISTEN_ADDR=0.0.0.0:8082 \
    STATIC_DIR=off \
    CLUB_KB_FILE=/app/knowledge.md \
    AI_WELCOME=on \
    AI_WELCOME_STREAM=on \
    RATE_LIMIT_PER_MIN=20 \
    WS_SEND_BUFFER=8192 \
    WS_BACKLOG_HIGH_WATER=400000 \
    GOMEMLIMIT=600MiB \
    TZ=Asia/Shanghai

EXPOSE 8082

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${LISTEN_ADDR##*:}/api/health" >/dev/null || exit 1

CMD ["/app/ai-backend"]
