# PicoClaw SSH Tunnel Proxy

通过 SSH 隧道将远程浏览器的 Chrome DevTools Protocol 调试端口转发到本地，实现远程浏览器控制。

## 工作原理

```
本地应用 (PicoClaw MCP 等)
        ↓
本地 127.0.0.1:9222
        ↓
    SSH 隧道
        ↓
远程 127.0.0.1:9222 (浏览器调试端口)
```

## 下载

从 [Releases](../../releases) 页面下载对应平台的压缩包。

### macOS

```bash
unzip picoclaw-proxy-macos-arm64.zip
chmod +x picoclaw-proxy
xattr -cr picoclaw-proxy
./picoclaw-proxy
```

### Linux

```bash
unzip picoclaw-proxy-linux-amd64.zip
chmod +x picoclaw-proxy
./picoclaw-proxy
```

### Windows

解压 `picoclaw-proxy-windows-amd64.zip`，双击或命令行运行 `picoclaw-proxy.exe`。

## 配置

在运行目录下创建 `picoclaw_proxy_config.yaml`：

```yaml
# SSH 连接
host: 10.88.7.15        # 远程服务器地址 (必填)
port: 22                 # SSH 端口 (默认 22)
user: root               # 用户名 (默认 root)
password: ""             # 密码认证 (二选一)
private_key: |           # 私钥认证 (二选一)
  -----BEGIN OPENSSH PRIVATE KEY-----
  ...
  -----END OPENSSH PRIVATE KEY-----

# 端口转发
local_addr: 127.0.0.1    # 本地监听地址 (默认 127.0.0.1)
local_port: 9222          # 本地监听端口 (默认 9222)
remote_addr: 127.0.0.1   # 远程目标地址 (默认 127.0.0.1)
remote_port: 9222         # 远程目标端口 (默认 9222)
```

## 使用

```bash
# 自动查找 picoclaw_proxy_config.yaml / config.yaml / *.yaml
./picoclaw-proxy

# 指定配置文件
./picoclaw-proxy -config /path/to/config.yaml
```

启动后，将 MCP 服务器地址配置为 `http://127.0.0.1:9222` 即可通过 PicoClaw 控制远程浏览器。

## 特性

- SSH 隧道自动重连，连接断开后 5 秒重试
- Keepalive 保活（15 秒间隔）
- 端口占用检测，提示占用进程
- 远程端口就绪检查
- 连接数统计（活跃/总计）
- 跨平台支持：Linux / macOS / Windows，amd64 / arm64

## 构建

```bash
go build -ldflags="-s -w" -o picoclaw-proxy .
```

## License

MIT
