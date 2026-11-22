# AI 多人聊天室（Go + WebSocket + DeepSeek）

> **最快方式：直接看 [一、Docker 一键部署](#一docker-一键部署推荐)，一条命令前后端全起。**

在原「个人主页 + 单聊 AI」基础上升级：

- ✅ **登录**：只需填 **名称 + 学校 + 专业**，自动填充到个人主页卡片
- ✅ **聊天室**：多人实时聊天，**在线人数**实时显示
- ✅ **默认头像**：根据名称首字 + 自动配色生成，不用准备图片
- ✅ **@AI 提问**：聊天室里 @AI 即可调用 DeepSeek，**回答流式输出且全员可见**

## 一、Docker 一键部署（推荐）

镜像内**只有一个 Go 进程**，同时提供前端网页、后端 API 和 WebSocket，不用再分别启动前后端。

### 1. 构建镜像

```bash
cd ai-chat                       # ⚠️ 上下文必须是项目根目录（含 Dockerfile 和 web/）
docker build -t ai-chat .
```

### 2. 启动容器（端口映射 + 注入令牌）

```bash
export DEEPSEEK_API_KEY=sk-xxxxxxxx      # 主机环境变量

docker run -d \
  --name ai-chat \
  --restart=always \
  -p 8082:8082 \
  -e DEEPSEEK_API_KEY=$DEEPSEEK_API_KEY \
  ai-chat
```

或者直接用脚本（自动构建 + 重建容器，端口写死 8082）：

```bash
bash docker-run.sh
```

浏览器打开 `http://<你的IP>:8082/`。

### 3. 关键说明

| 项 | 值 | 说明 |
|---|---|---|
| 容器内端口 | `8082` | 由 Dockerfile 的 `ENV LISTEN_ADDR=0.0.0.0:8082` 固定 |
| 主机端口 | `8082` | `-p 8082:8082` 左边是主机端口，改 `-p 80:8082` 就能用 80 访问 |
| 前端目录 | `/app/web` | `STATIC_DIR` 已固定，网页由 Go 进程托管 |
| 令牌 | 运行期 `-e` 注入 | **不写进镜像**，避免泄露 |


## 日志与访客 IP

日志分 `[登录]` / `[进入]` / `[聊天]` / `[AI提问]` / `[欢迎]` / `[离开]` 六类，
均带访客 IP（消息内容截断到 120 字）。详见 **`日志与真实IP.md`**。

⚠️ **用 `-p 80:8082` 的 bridge 模式时，日志里 IP 全是 `172.17.0.1`**（docker 网桥地址）。
想看真实访客 IP 请改用 `--network=host`：

```bash
docker run -d --name ai-chat --restart=always \
  --network=host \
  -e LISTEN_ADDR=0.0.0.0:80 \
  -e DEEPSEEK_API_KEY=$DEEPSEEK_API_KEY \
  ai-chat
```

### 4. 常用命令

```bash
docker logs -f ai-chat          # 看日志
docker restart ai-chat          # 改了环境变量后重启
docker rm -f ai-chat            # 停止并删除
docker exec -it ai-chat sh      # 进容器排查
```

### 5. 可选环境变量（都通过 -e 传）

```bash
-e DEEPSEEK_MODEL=deepseek-v4-pro   # 换更强的模型
-e RATE_LIMIT_PER_MIN=5             # 公网分享时把限流调小
-e THINKING=on                      # 开启思维链
-e AI_WELCOME=off                   # 关掉进房欢迎（省 token）
```

### 6. 改端口

把 Dockerfile 里 `LISTEN_ADDR` 和 `EXPOSE` 一起改，再 `-p 主机端口:容器端口`。
例如容器内 8082、主机 80：`docker run -p 80:8082 ...`

## 二、运行架构

```
浏览器 ──→ Go 单进程 :8081
             ├── /            → 静态网页（登录页 + 主页 + 聊天室）
             ├── /api/login   → 登录，发 token
             ├── /api/me      → 恢复登录态（刷新页面不用重填）
             ├── /ws          → WebSocket 长连接（聊天 + 在线状态）
             └── @AI 消息      → 调 api.deepseek.com 并流式广播
```

**推荐单进程跑**（`STATIC_DIR=./web`），前后端同源，不用处理跨域和 cookie 问题。

## 三、页面结构（三个页面）

| 页面 | 作用 |
|---|---|
| `index.html` | **入口**：检测登录态，自动跳 login 或 home |
| `login.html` | **独立登录页**：填名称+学校+专业，成功后跳 home |
| `home.html` | **主页**：个人信息 → Linux/RHCSA → 云原生/K8s/CKA → AI 聊天室 |

```
index.html ──已登录──→ home.html
            └─未登录─→ login.html ──登录成功──→ home.html
```

## 四、启动（三步）

```bash
cd backend
cp env.sh.example env.sh
vim env.sh                    # 填 DEEPSEEK_API_KEY=sk-xxx（不填也能跑，只是 @AI 不可用）
go build -trimpath -o ai-backend .
source env.sh && ./ai-backend
```

浏览器打开 `http://<你的IP>:8081`（单进程模式就是这个端口）。
务必访问 `http://IP:8081/` 而不是具体文件名，让 index.html 做跳转。

后台常驻（SSH 断开也不死）：

```bash
source env.sh
setsid nohup ./ai-backend > backend.log 2>&1 < /dev/null &
```

停止：`pkill -f ai-backend`

## 五、功能说明

### 登录
填三个字段即可，**没有密码**。后端发一个随机 token 存在浏览器 `localStorage`，刷新页面自动恢复，点「退出登录」清除。

> 这是「轻登录」——只为了知道谁是谁（显示昵称和头像），**不做身份验证**，任何人填同名就能进来。课程演示够用，别用它存敏感信息。

### AI 主动欢迎

新成员进入聊天室时，AI 会**主动发一条欢迎消息**，内容会自然地结合他填的学校/专业**以及自我介绍**，并简要介绍自己能做什么（Linux、Red Hat、云原生、Kubernetes 相关问题）。

登录表单有「自我介绍」选填项（限 100 字），填了之后欢迎语会明显更个性化——
例如填"正在备考 CKA，对调度很感兴趣"，AI 就可能顺着这个话题打招呼。

三重保护，避免刷屏和烧钱：

| 保护 | 说明 |
|---|---|
| 开关可关 | `AI_WELCOME=off` 即关闭，默认 on |
| 没 Key 静默跳过 | 未配置 `DEEPSEEK_API_KEY` 时不发也不报错 |
| 冷却 5 分钟 | 同一用户反复进出不会重复欢迎 |

冷却时间改 `main.go` 里的 `welcomeCooldownMin`（分钟），设为 0 表示每次进房都欢迎——**公网环境不建议**，一次刷新就是一次真实 API 调用。

### 历史消息与 AI 回答

新用户进入聊天室时，会回看最近 50 条历史，**包括 AI 的回答和欢迎语**（不只是人类提问）。
因为 `ai_end` 消息里带了全文、提问者和时间戳，回放时能完整还原。

另外，如果你是在某条 AI 回答**生成过程中**进来的，`ai_end` 到达时会用全文兜底渲染，
不会出现"只看到半句"。

### 消息时间戳

每条消息（含 AI 回答）都在昵称旁显示发送时间：

- 当天：`14:30`
- 跨天：`9/21 14:30`

### 头像
不依赖任何图片文件。取名称首字 + 按用户 ID 哈希出的配色，生成圆形字母头像。**把 `web/images/avatar.jpg` 删掉也不影响。**

### 聊天室
- 顶部显示**在线人数**，下方一排头像展示当前在线的人
- 有人进/出会广播系统提示
- 新用户进来能回看最近 **50 条**历史消息
- 断线自动重连（3 秒一次）

### @AI
两种方式：
1. 输入以 `@AI` 开头，如 `@AI 介绍一下云原生`
2. 点输入框旁的 **@AI 按钮**

回答会以流式（打字机）广播给房间所有人，标注「XX 提问」。

**群聊上下文**：@AI 时后端会自动把最近 **30 条**聊天记录（`昵称: 内容`，含此前的 AI 回答）
作为背景注入请求，所以 AI 知道房间里刚讨论了什么，能回答「刚才谁说的那个怎么弄」这类问题。
上下文条数由 `maxContextMsgs` 控制，调小可省 token。

**省钱设计**：只有 @AI 才消耗 DeepSeek 额度，普通聊天走 WebSocket 广播，**一分钱不花**。AI 调用按用户限流（默认 20 次/分钟），系统提示词里也限制了回答长度（200 字内）。

## 六、文件清单

```
ai-chat/
├── web/
│   ├── index.html       # 入口，自动跳转
│   ├── login.html       # 独立登录页
│   ├── home.html        # 主页：个人信息 + 技术介绍 + 聊天室
│   └── common.js        # 公共逻辑（探测/登录/WebSocket/渲染）
└── backend/
    ├── main.go          # 用户体系 + AI 流式调用 + HTTP 接口 + 静态托管
    ├── ws.go            # 手写 WebSocket(RFC6455) + 聊天室 Hub + 上下文
    ├── go.mod           # 零第三方依赖
    └── env.sh.example   # → 复制为 env.sh
```

## 七、移动端适配

已做的处理：

- `<meta viewport>` 加 `viewport-fit=cover`，适配刘海屏
- 输入框 `font-size: 16px` —— **低于 16px 时 iOS 聚焦会自动放大整个页面**
- 布局改为单列流式，卡片内边距收窄
- 聊天室高度用 `68vh`（配合 `min-height`），避免手机上被挤扁
- 极窄屏（<400px）头像居中、标签换行

真机测试建议用手机浏览器直接开 `http://<IP>:8081`。

## 八、公网开放

| 端口 | 用途 |
|---|---|
| **8081** | 网页 + API + WebSocket（单进程模式只需这一个） |

- 云服务器：控制台**安全组放行 TCP 8081**
- 已确认 `LISTEN_ADDR=0.0.0.0:8081`（进程监听没问题）
- 系统内防火墙：`sudo ufw allow 8081/tcp`（你之前 disable 过，演示完建议装回来且只放开需要端口）
- 校园网/家里：无公网 IP，需内网穿透

⚠️ 验证必须**从外网测**（手机 4G 或 canyouseeme.org），在服务器上 curl 自己的公网 IP 必然超时，那是弹性 IP 不支持回环，不是故障。

## 九、环境变量（`backend/env.sh`）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `DEEPSEEK_API_KEY` | 空 | 不填则 @AI 提示未配置，其余功能正常 |
| `LISTEN_ADDR` | `0.0.0.0:8081` | 公网访问必须 `0.0.0.0` |
| `STATIC_DIR` | `./web` | 填了就顺带托管网页（推荐） |
| `DEEPSEEK_MODEL` | `deepseek-flash` | 或 `deepseek-v4-pro` |
| `RATE_LIMIT_PER_MIN` | `20` | 每人每分钟 @AI 次数 |
| `SYSTEM_PROMPT` | 内置（限 200 字） | 自定义 AI 人格 |

## 十、排错

| 现象 | 原因 |
|---|---|
| 登录提示「后端起了吗？」 | 后端没启动，或页面端口填错（单进程是 8081） |
| 网页能开但聊天室连不上 | WebSocket 被拦：检查安全组放行 8081；若套了反代需支持 WS 升级 |
| 外网打不开 | 安全组没放行 8081 |
| @AI 报「未配置 Key」 | 没 `source env.sh` 就启动了，看启动日志 `apikey已配置=` |
| @AI 报 402/401 | Key 无效或余额不足 |
| 提示「AI 调用太频繁」 | 触发限流，调大 `RATE_LIMIT_PER_MIN` |

## 十一、已知限制（课程演示可接受）

| 限制 | 说明 |
|---|---|
| 用户数据存内存 | 服务重启后登录态和历史消息清空 |
| 无密码 | 同名可重复登录，非真实身份认证 |
| 单房间 | 只有一个聊天室，没做多频道 |
| 聊天记录不落盘 | 重启即丢，内存保留最近 50 条 |
