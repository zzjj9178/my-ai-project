# ============================================================
#  前端镜像 —— Nginx 提供静态页面，并把 /api、/ws 反代给后端
#
#  构建上下文就是本目录（web/）：
#    docker build -t ai-chat-front ./web
#
#  注意：构建上下文是 web/ 而不是项目根目录，所以下面 COPY 的都是
#        本目录内的文件。新增页面时，记得把文件名加到 COPY 里。
# ============================================================
FROM nginx:1.27-alpine

# 换掉默认站点配置，用我们自己的（同目录下的 nginx.conf）
RUN rm -f /etc/nginx/conf.d/default.conf
COPY nginx.conf /etc/nginx/conf.d/app.conf

# 静态页面：HTML / CSS / JS / 图标
COPY index.html login.html home.html admin.html /usr/share/nginx/html/
COPY common.js fireworks.js /usr/share/nginx/html/
COPY logos/ /usr/share/nginx/html/logos/

# 确保 Nginx 以 nginx 用户能读到所有文件（宿主机文件权限可能比较严）
RUN chmod -R a+rX /usr/share/nginx/html

EXPOSE 80

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1/login.html >/dev/null || exit 1

CMD ["nginx", "-g", "daemon off;"]
