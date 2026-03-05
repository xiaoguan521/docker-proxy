# Docker Registry Proxy

自建 Docker 镜像代理服务，完美解决国内无法顺畅拉取 Docker Hub 及各类海外源容器镜像（ghcr, gcr, quay 等）的问题。

本项目零外部依赖，单文件编译，采用轻量级的 **Auth Rewrite（凭证透传）** 架构。代理服务本身不保存任何 Token 或凭据，而是将鉴权流程完美重定向回 Docker 客户端，**原生支持私有镜像库的 `docker login` 以及多架构镜像（OCI Index）拉取**。

## 🌟 核心功能

- **透明代理 & 鉴权透传**：拦截 HTTP 401 响应，将 `Www-Authenticate` 头改写为代理地址，Docker 客户端使用标准鉴权流程（支持任意仓库的匿名/私有拉取）。
- **多上游仓库智能路由**：支持通过子域名（如 `ghcr.docker...`）或 `ns` 参数无缝分发请求到 quay.io、gcr.io、k8s.gcr.io 等第三方仓库。
- **官方镜像自动补全**：拉取 `nginx` 自动转为 `library/nginx`。
- **多架构清单 (OCI Index) 支持**：合并且透传多重 `Accept` 请求头，完美支持跨平台镜像（ARM64/AMD64）识别。
- **Blob 下载 CDN 重定向跟随**：支持自动跟随上游仓库下发的多跳 30x CDN/S3 下载链接。
- **安全与反爬**：屏蔽常见探测爬虫，附带虚假的 Nginx 默认欢迎页伪装。
- **健康检查与浏览器 UI**：`/health` 端点检测多路连通性；浏览器直接访问时提供极简的镜像搜索页面。
- **自带 CI/CD 工作流**：一键推送 Tag 即自动使用 GitHub Actions 交叉编译四大平台并打包 Release。

## 🔨 安装与运行

需要 Go 1.25+。

你可以直接在 [Releases](https://github.com/xiaoguan521/docker-proxy/releases) 页面下载预编译好的二进制文件，或者手动编译：

```bash
# 本机编译
go build -o docker-proxy .

# 交叉编译 Linux x86-64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o docker-proxy-linux-amd64 .
```

### 部署与启动

将编译好的二进制上传到海外服务器，赋予执行权限后启动：

```bash
chmod +x docker-proxy-linux-amd64

# 前台测试运行
./docker-proxy-linux-amd64 -addr :5000

# 后台守护进程模式运行
./docker-proxy-linux-amd64 -d -addr :5000

# 停止守护进程
kill $(cat docker-proxy.pid)
```

**命令行参数：**
| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `:5000` | 监听地址与端口 |
| `-tls-cert` | 空 | TLS 证书文件路径（空则为 HTTP） |
| `-tls-key` | 空 | TLS 私钥文件路径 |
| `-d` | `false` | 后台守护进程模式 |
| `-log` | `docker-proxy.log` | 守护进程模式下的日志文件路径 |

*(推荐将服务部署在内网端口，前端使用 Nginx Proxy Manager 等反向代理工具配置 HTTPS 与证书)*。

> **⚠️ Nginx 反代注意事项**：如果你使用 Nginx 作为前置反代，请务必开启 Header 透传逻辑：
> ```nginx
> proxy_set_header Host $host;
> proxy_set_header X-Real-IP $remote_addr;
> proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
> proxy_set_header X-Forwarded-Proto $scheme;
> 
> # 必须透传 Authorization
> proxy_set_header Authorization $http_authorization;
> proxy_pass_header Authorization;
> 
> # 取消缓存，应对大文件传输
> proxy_buffering off;
> proxy_request_buffering off;
> client_max_body_size 0;
> ```

## 🛠️ Docker 客户端配置与使用

默认代理 Docker Hub。假设你的代理域名为 `docker.201807.xyz`：

### 方式一：配置 registry-mirrors (仅限 Docker Hub)

编辑 `/etc/docker/daemon.json`：
```json
{
  "registry-mirrors": ["https://docker.201807.xyz"]
}
```
重启 Docker daemon 后，日常的 `docker pull nginx` 将自动通过代理加速。

### 方式二：直接代理拉取 (推荐，支持所有仓库)

通过拼接代理域名，直接拉取目标镜像，这种方式对群晖、K8s 等特定环境非常友好：

```bash
# 拉取 Docker Hub 官方源
docker pull docker.201807.xyz/library/alpine:latest

# 拉取其他用户源
docker pull docker.201807.xyz/bitnami/redis:latest
```

## 🔐 登录私有仓库 & 拉取第三方库

得益于 `Auth Rewrite` 机制，你可以通过指定子域名作为路由（需事先泛解析或在 NPM 中配置好 `ghcr.xxx`、`gcr.xxx` 均指向 `:5000` 端口），完美登录并拉取第三方/私有仓库源：

| 你访问的子域名代理前缀 | 实际转发到的上游仓库 |
|---|---|
| `docker.你的域名` (默认) | `registry-1.docker.io` |
| `ghcr.你的域名` | `ghcr.io` |
| `gcr.你的域名` | `gcr.io` |
| `quay.你的域名` | `quay.io` |
| `k8s.你的域名` | `registry.k8s.io` |

**示例交互：登录 GHCR.io 并拉取私有包**
```bash
# 使用你的 GitHub 用户名及 PAT (Personal Access Token) 登录
$ docker login ghcr.docker.201807.xyz
Username: xiaoguan521
Password: 
Login Succeeded

# 拉取关联的私有镜像
$ docker pull ghcr.docker.201807.xyz/xiaoguan521/music-backend-node:latest
```

## 🩺 诊断与监控

**系统连通性检查**
访问 `/health` 端点，即可获取详尽的上游连通检测结果（代理自身会在内部向各个上游仓库发起通讯测试）：

```bash
curl https://docker.201807.xyz/health
```
```json
{
  "proxy": "running",
  "time": "2026-03-05T06:50:00Z",
  "checks": [
    {"name": "auth.docker.io", "status": "HTTP 200", "latency": "66ms"},
    {"name": "registry-1.docker.io", "status": "HTTP 401", "latency": "13ms"}
  ]
}
```

## 🏗️ 架构对比：相比 Cloudflare Workers 版本的优势

- **真·全库连通**：CF 版大多需要硬编码应对多变的回源和鉴权逻辑；当前 Go 版采用了通用泛代理，一次部署搞定私有库+多三方库。
- **OCI Index 兼容**：CF Worker 的 `fetch` API 在处理重名的 `Accept` Header 时多有丢失，经常引发 `OCI index found, but Accept header does not support` 错误。Go 版精准合并 Header 透传，解决多架构/跨平台拉取卡脖子问题。
- **全带宽利用**：无需忍受 CF Pages / Workers 动辄切断长时链接、限制请求次数的问题。

## License

MIT
