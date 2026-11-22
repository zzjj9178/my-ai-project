#!/bin/bash
# =========================================================
# 一键部署 AI 智能主页 (v2.0 - 离线版)
# AI&云原生俱乐部实践项目 - 2026
# =========================================================

set -e # 任何命令失败则立即退出

# ---------- 全局变量 ----------
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONFIG_FILE="${SCRIPT_DIR}/config.ini"
TEMPLATE_FILE="${SCRIPT_DIR}/index.html.template"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"
GENERATED_HTML_DIR="${SCRIPT_DIR}/dist" # 存放生成的个性化文件
GENERATED_HTML_FILE="${GENERATED_HTML_DIR}/index.html"

BACKEND_TAR="${SCRIPT_DIR}/final-project_backend.tar"
FRONTEND_TAR="${SCRIPT_DIR}/final-project_frontend.tar"

BAK_IMAGE="final-project_backend:latest"
FNT_IMAGE="final-project_frontend:latest"
PROJECT_NAME="my-ai-app"

# ---------- 函数定义 ----------
function check_root() {
    if [ "$EUID" -ne 0 ]; then
        echo "错误：请使用 sudo 运行此脚本 (sudo ./deploy.sh)。"
        exit 1
    fi
}

function check_config() {
    echo ">>> 1. 检查配置文件..."
    if [ ! -f "$CONFIG_FILE" ]; then
        echo "   [错误] 配置文件 config.ini 未找到！"
        exit 1
    fi

    # 使用 source 命令加载配置
    source "$CONFIG_FILE"

    if [ -z "$STUDENT_NAME" ] || [ -z "$STUDENT_SCHOOL" ] || [ -z "$STUDENT_MAJOR" ] || [ -z "$STUDENT_PHOTO" ]; then
        echo "   [错误] config.ini 中的信息不完整，请填写所有项目。"
        exit 1
    fi

    if [ ! -f "${SCRIPT_DIR}/${STUDENT_PHOTO}" ]; then
        echo "   [错误] 照片文件 '${STUDENT_PHOTO}' 在当前目录下未找到！"
        exit 1
    fi
    echo "   配置检查通过。"
}

function install_docker() {
    echo ">>> 2. 检查并安装 Docker 环境..."
    if ! command -v docker &>/dev/null; then
        echo "   正在安装 docker.io 和 docker-compose..."
        apt-get update >/dev/null
        apt-get install -y docker.io docker-compose >/dev/null
        systemctl enable --now docker
        echo "   Docker 安装完成。"
    else
        echo "   Docker 已安装。"
    fi

    # 配置镜像加速
    mkdir -p /etc/docker
    cat > /etc/docker/daemon.json <<'DAEMON'
    {
      "registry-mirrors": [
        "https://docker.m.daocloud.io",
        "https://docker.1ms.run"
      ]
    }
DAEMON
   if systemctl is-active --quiet docker; then
       systemctl daemon-reload
       systemctl restart docker
   fi
}

function load_images() {
    echo ">>> 3. 导入应用镜像..."
    if [ ! -f "$BACKEND_TAR" ] || [ ! -f "$FRONTEND_TAR" ]; then
        echo "   [错误] 镜像文件 (.tar) 不完整！"
        exit 1
    fi
    echo "   正在导入后端镜像 (这可能需要一点时间)..."
    docker load < "$BACKEND_TAR"
    echo "   正在导入前端镜像..."
    docker load < "$FRONTEND_TAR"
    echo "   镜像导入完成。"
}

function generate_files() {
    echo ">>> 4. 生成个性化文件..."
    
    # 创建存放生成文件的目录
    mkdir -p "${GENERATED_HTML_DIR}/images"

    # 1. 替换模板生成 index.html
    cp "$TEMPLATE_FILE" "$GENERATED_HTML_FILE"
    sed -i "s/__STUDENT_NAME__/${STUDENT_NAME}/g" "$GENERATED_HTML_FILE"
    sed -i "s/__STUDENT_SCHOOL__/${STUDENT_SCHOOL}/g" "$GENERATED_HTML_FILE"
    sed -i "s/__STUDENT_MAJOR__/${STUDENT_MAJOR}/g" "$GENERATED_HTML_FILE"
    sed -i "s/__STUDENT_PHOTO__/${STUDENT_PHOTO}/g" "$GENERATED_HTML_FILE"
    
    # 2. 将照片复制到目标位置，以便挂载
    cp "${SCRIPT_DIR}/${STUDENT_PHOTO}" "${GENERATED_HTML_DIR}/images/"

    # 3. 生成 docker-compose.yml
    cat > "$COMPOSE_FILE" <<EOF
version: "3.5"
services:
  backend:
    image: ${BAK_IMAGE}
    container_name: ${PROJECT_NAME}-backend
    restart: unless-stopped
    extra_hosts:
      - "host.docker.internal:host-gateway"

  frontend:
    image: ${FNT_IMAGE}
    container_name: ${PROJECT_NAME}-frontend
    restart: unless-stopped
    ports:
      - "80:80"
    depends_on:
      - backend
    # 关键：将宿主机上生成的个性化文件挂载到容器内
    volumes:
      - ${GENERATED_HTML_DIR}:/usr/share/nginx/html:ro

networks:
  default:
    name: ${PROJECT_NAME}-net
EOF
    echo "   个性化文件生成完毕。"
}

function start_app() {
    echo ">>> 5. 启动您的AI智能主页..."
    cd "$SCRIPT_DIR"
    docker-compose -p "$PROJECT_NAME" up -d
}

# ---------- 主逻辑 ----------
main() {
    check_root
    check_config
    install_docker
    load_images
    generate_files
    start_app

    echo ""
    echo "🎉🎉 恭喜！部署成功啦！"
    echo "-------------------------------------------------------------"
    echo "请在浏览器中访问您的服务器IP地址:192.168.30.100, 开始体验吧！"
    echo "-------------------------------------------------------------"
    echo ""
    echo "常用管理命令:"
    echo "  - 查看日志: sudo docker-compose -p ${PROJECT_NAME} logs -f"
    echo "  - 停止服务: sudo docker-compose -p ${PROJECT_NAME} down"
    echo "  - 再次启动: sudo docker-compose -p ${PROJECT_NAME} up -d"
    echo ""
}

main "$@"

