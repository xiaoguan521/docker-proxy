# Docker Registry Proxy

自建 Docker Hub 镜像代理服务，部署在可访问 Docker Hub 的海外服务器上，解决国内无法直接拉取 Docker 镜像的问题。

## 功能

- 透明代理 Docker Hub（registry-1.docker.io）的所有 Registry V2 API 请求
- 内部自动处理 auth.docker.io 鉴权，Docker 客户端无需配置认证
- Token 缓存，避免重复请求 auth.docker.io
- 官方镜像自动补全 `library/` 前缀
- Blob 下载 CDN 重定向自动跟随（支持多跳）
- 支持多上游仓库路由（quay.io、gcr.io、ghcr.io、registry.k8s.io 等）
- 浏览器访问时展示 Docker Hub 镜像搜索页面
- 爬虫 UA 屏蔽 + nginx 伪装页
- `/health` 健康检查端点，诊断到上游的连通性
- 支持 TLS（HTTPS）
- 支持后台守护进程模式运行
- 单文件编译，无外部依赖，静态链接

## 编译

需要 Go 1.21+。

```bash
# 本机编译
go build -o docker-proxy .

# 交叉编译 Linux x86-64（用于部署到服务器）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o docker-proxy-linux-amd64 .
```

## 部署

将编译好的二进制上传到海外服务器：

```bash
chmod +x docker-proxy-linux-amd64

# 前台运行（调试用）
./docker-proxy-linux-amd64 -addr :5000

# 后台运行
./docker-proxy-linux-amd64 -d -addr :5000

# 停止
kill $(cat docker-proxy.pid)
```

### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `:5000` | 监听地址 |
| `-tls-cert` | 空 | TLS 证书文件路径（留空则使用 HTTP） |
| `-tls-key` | 空 | TLS 私钥文件路径 |
| `-d` | `false` | 后台守护进程模式 |
| `-log` | `docker-proxy.log` | 日志文件路径（守护进程模式下生效） |

### 使用 systemd 管理（推荐）

创建 `/etc/systemd/system/docker-proxy.service`：

```ini
[Unit]
Description=Docker Registry Proxy
After=network.target

[Service]
Type=simple
ExecStart=/opt/docker-proxy/docker-proxy-linux-amd64 -addr :5000
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now docker-proxy
```

## Docker 客户端配置

### 方式一：配置 registry-mirrors（推荐）

编辑 Docker daemon 配置文件 `/etc/docker/daemon.json`：

```json
{
  "registry-mirrors": ["http://你的服务器IP:5000"],
  "insecure-registries": ["你的服务器IP:5000"]
}
```

重启 Docker：

```bash
sudo systemctl daemon-reload
sudo systemctl restart docker
```

验证配置生效：

```bash
docker info | grep -A 5 "Registry Mirrors"
```

之后正常使用 `docker pull` 即可自动走代理：

```bash
docker pull nginx
docker pull ubuntu:22.04
```

> 如果使用 HTTPS（配置了 TLS 证书），则不需要 `insecure-registries`。

### 方式二：直接指定代理地址拉取

无需修改 daemon.json，直接通过代理地址拉取：

```bash
docker pull 你的服务器IP:5000/library/nginx:latest
docker pull 你的服务器IP:5000/bitnami/redis:latest
```

拉取后可用 `docker tag` 重命名：

```bash
docker tag 你的服务器IP:5000/library/nginx:latest nginx:latest
```

## 诊断

### 健康检查

访问 `/health` 端点检测代理到上游的连通性：

```bash
curl http://你的服务器IP:5000/health
```

返回示例：

```json
{
  "proxy": "running",
  "listen": ":5000",
  "checks": [
    {"name": "auth.docker.io", "status": "HTTP 200", "latency": "66ms", "detail": "OK"},
    {"name": "registry-1.docker.io", "status": "HTTP 401", "latency": "13ms", "detail": "OK"},
    {"name": "hub.docker.com", "status": "HTTP 200", "latency": "132ms", "detail": "OK"}
  ]
}
```

- `auth.docker.io` → HTTP 200 表示鉴权服务可达
- `registry-1.docker.io` → HTTP 401 表示 Registry 可达（未认证返回 401 是正常的）
- 任何一个显示 `FAIL` 说明服务器无法访问 Docker Hub

### V2 端点测试

```bash
curl http://你的服务器IP:5000/v2/
# 期望: {} （HTTP 200，带 Docker-Distribution-Api-Version 头）
```

### 手动测试镜像拉取

```bash
curl -s http://你的服务器IP:5000/v2/library/alpine/manifests/latest \
  -H "Accept: application/vnd.docker.distribution.manifest.v2+json" | head -20
```

## 支持的上游仓库

默认代理 Docker Hub。通过 `ns` 查询参数或域名前缀路由支持其他仓库：

| 前缀/参数 | 上游 |
|---|---|
| 默认 | registry-1.docker.io |
| `quay` | quay.io |
| `gcr` | gcr.io |
| `k8s-gcr` | k8s.gcr.io |
| `k8s` | registry.k8s.io |
| `ghcr` | ghcr.io |
| `cloudsmith` | docker.cloudsmith.io |
| `nvcr` | nvcr.io |

## 架构

```
Docker Client (国内)
    │
    │  docker pull nginx
    │
    ▼
Docker Proxy (海外 VPS :5000)
    │
    ├─→ auth.docker.io   (获取 token，带缓存)
    │
    ├─→ registry-1.docker.io  (拉取 manifest)
    │
    └─→ CDN / S3  (跟随重定向，下载 blob)
    │
    ▼
Docker Client 收到镜像数据
```

## License

MIT
