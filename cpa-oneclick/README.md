# CPA 一键启动

面向本机快速启动 / 停止 / 重启 CLIProxyAPI（CPA）的轻量控制脚本。

## 依赖

- macOS / Linux + Bash
- 本仓库根目录已有可执行文件 `cli-proxy-api`，或本机已安装 Go（脚本可自动编译）
- 配置文件：优先使用仓库根目录 `config.yaml`；若不存在则从 `config.example.yaml` 复制

## 快速开始

在仓库根目录执行：

```bash
./cpa-oneclick/start.sh
```

其它命令：

```bash
./cpa-oneclick/status.sh   # 查看运行状态
./cpa-oneclick/stop.sh     # 停止服务
./cpa-oneclick/restart.sh  # 重启服务
./cpa-oneclick/logs.sh     # 跟踪日志
```

Windows 可用：

```powershell
.\cpa-oneclick\start.ps1
.\cpa-oneclick\status.ps1
.\cpa-oneclick\stop.ps1
.\cpa-oneclick\restart.ps1
```

## 默认行为

| 项 | 说明 |
| --- | --- |
| 工作目录 | 仓库根目录（`CLIProxyAPI/`） |
| 配置 | `config.yaml`（缺失时从 `config.example.yaml` 生成） |
| 二进制 | `./cli-proxy-api`；不存在则 `go build -o cli-proxy-api ./cmd/server` |
| 日志 | `cpa-oneclick/logs/cpa.log` |
| PID | `cpa-oneclick/logs/cpa.pid` |
| 默认端口 | `8317`（以 `config.yaml` 为准） |

## 常用地址

服务启动后：

- API：`http://127.0.0.1:8317`
- 管理面板：`http://127.0.0.1:8317/management.html`

## 环境变量（可选）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CPA_ROOT` | 自动检测仓库根目录 | CPA 项目根路径 |
| `CPA_CONFIG` | `$CPA_ROOT/config.yaml` | 配置文件路径 |
| `CPA_BIN` | `$CPA_ROOT/cli-proxy-api` | 可执行文件路径 |
| `CPA_PORT` | 从配置读取，失败则 `8317` | 健康检查端口 |
| `CPA_OPEN_BROWSER` | `0` | 设为 `1` 时启动后尝试打开管理面板 |

示例：

```bash
CPA_OPEN_BROWSER=1 ./cpa-oneclick/start.sh
CPA_CONFIG=/path/to/config.yaml ./cpa-oneclick/start.sh
```

## 说明

- 脚本以前台守护方式用 `nohup` 后台启动，不修改你现有的 `config.yaml` 业务内容（仅在缺失时从示例复制）。
- 若管理面板 `secret-key` 为空，管理 API 可能不可用；请按 `config.example.yaml` 自行配置。
- 这是本地运维辅助项目，不是 Docker 部署方案；Docker 请继续使用仓库根目录 `docker-compose.yml`。
