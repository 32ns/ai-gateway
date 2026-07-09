# AI Gateway

AI Gateway 是一个用 Go 编写的 AI API 网关功能类似sub2api和new-api。它可以把 OpenAI、Claude、ChatGPT/Codex 等账号或 API Key 统一接入到一个兼容协议入口，并提供账号池、模型路由、失败切换、用量计费、用户管理、套餐、支付、审计、备份恢复等运维能力。

![首页](./images/home.png)

![用户控制台](./images/userhome.png)

![生图](./images/image.png)

![安装向导](./images/install.png)



### 程序最大特点就是简单,简单到会用鼠标就可以部署运维

假设你电脑上已经有golang和python环境,直接在工程目录下执行 python install/ag_deploy.py 会出现上图的UI,填入ssh服务地址,也就是你购买的服务器,和密码,还有右侧的网站域名点击下方的 install 稍等半分钟全部就搞定了,如果你在程序运行期间想更新程序或者添加新功能也不用担心,只需要修改完你想要的功能后点击"Upgrade"按钮,程序会自动编译并部署,不会影响用户使用,项目偏大,细节还是挺多的,就不一一介绍了,大家下载到源码后让AI来具体分析一下吧





## 主要功能

- OpenAI 兼容接口：`/v1/chat/completions`、`/v1/responses`、`/v1/embeddings`、`/v1/images/*`、`/v1/audio/*` 等。
- Claude/Anthropic 兼容接口：`/v1/messages`、`/api/v1/messages`、`/anthropic/v1/messages`。
- 多账号池管理：支持 OpenAI、Claude、API Key、OAuth、Codex 导入、自定义上游地址和代理。
- 路由与故障切换：按模型前缀选择供应商，自动处理限流、认证失败、上游异常和临时冷却。
- 运营控制台：用户、客户端 Key、账号组、模型价格、套餐、支付、余额、工单、公告、文档、审计日志。
- 计费与套餐：按 Token、请求、图片等维度计费，支持余额和套餐额度两类资金来源。
- 支付集成：内置微信支付、支付宝和 PersonalPay 相关配置入口。
- 存储后端：默认 SQLite，也支持 PostgreSQL。
- 备份恢复：支持命令行备份/恢复，也支持管理端热恢复。
- Linux 部署：内置 systemd 安装、升级、回滚、重启和卸载命令，可配合 Caddy 反向代理。

## 快速开始

### 1. 准备配置

复制示例配置：

```bash
cp config.example.json config.json
```

默认监听地址是 `127.0.0.1:8088`，默认数据库是 `data/state.db`。

建议生产环境至少配置：

- `api_key`：初始协议客户端 Key；为空时程序会自动生成并在启动日志中输出。
- `master_key`：用于加密持久化凭据、客户端 Key、代理地址和系统密钥。
- `public_base_url`：公网访问地址，用于 OAuth、支付回调、站点地图等场景。

### 2. 启动服务

开发或本地运行：

```bash
go run ./cmd/server -config config.json
```

构建二进制：

```bash
go build -o ag ./cmd/server
./ag -config config.json
```

Windows 下可以使用：

```powershell
go build -o ag.exe .\cmd\server
.\ag.exe -config config.json
```

启动后访问：

```text
http://127.0.0.1:8088
```

首次启动会创建管理员账号：

```text
用户名：root
密码：toor
```

首次登录后请立即修改默认密码。

## 配置说明

`config.example.json` 中包含常用配置：

| 字段 | 说明 |
| --- | --- |
| `host` / `port` | HTTP 服务监听地址 |
| `api_key` | 初始协议客户端 Key |
| `audit_limit` | 审计日志保留数量的初始值 |
| `gateway_audit` | 是否记录网关请求审计 |
| `gateway_audit_errors` | 是否只记录出错的网关请求审计的初始值，后台可动态开关，`gateway_audit=false` 时生效 |
| `gateway_audit_retention_days` | 出错网关请求审计的初始保留天数，后台可动态调整，默认 1 天 |
| `max_in_flight` | 服务最大并发请求数 |
| `protocol_request_body_limit` | 协议请求体大小限制 |
| `database_backend` | `sqlite` 或 `postgres` |
| `state_path` | SQLite 数据库路径 |
| `postgres_dsn` | PostgreSQL DSN，使用 PostgreSQL 时必填 |
| `master_key` | 状态密钥加密用主密钥 |
| `public_base_url` | 公网基础 URL |
| `trusted_proxy_cidrs` | 可信反向代理 CIDR |

PostgreSQL 示例：

```json
{
  "database_backend": "postgres",
  "postgres_dsn": "postgres://user:password@127.0.0.1:5432/ai_gateway?sslmode=disable"
}
```

## API 使用

在管理端创建或查看客户端 Key 后，用标准 Bearer Token 调用网关：

```bash
curl http://127.0.0.1:8088/v1/models \
  -H "Authorization: Bearer YOUR_CLIENT_KEY"
```

OpenAI Chat Completions 示例：

```bash
curl http://127.0.0.1:8088/v1/chat/completions \
  -H "Authorization: Bearer YOUR_CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1",
    "messages": [
      {"role": "user", "content": "Hello"}
    ]
  }'
```

Claude Messages 示例：

```bash
curl http://127.0.0.1:8088/anthropic/v1/messages \
  -H "Authorization: Bearer YOUR_CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-5",
    "max_tokens": 1024,
    "messages": [
      {"role": "user", "content": "Hello"}
    ]
  }'
```

## Linux 服务部署

构建 Linux 二进制后可使用内置安装命令：

```bash
go build -o ag ./cmd/server
sudo ./ag install
```

安装向导默认路径：

| 项目 | 默认值 |
| --- | --- |
| 安装目录 | `/opt/ai-gateway` |
| 配置目录 | `/etc/ai-gateway/config.json` |
| 数据目录 | `/var/lib/ai-gateway` |
| systemd 服务 | `ai-gateway` |
| 内部端口 | `18088` |
| 备用端口 | `18089` |

常用服务命令：

```bash
sudo ./ag restart
sudo ./ag stop
sudo ./ag upgrade
sudo ./ag rollback
sudo ./ag uninstall
```

## 备份与恢复

创建备份：

```bash
./ag backup --config config.json --out ai-gateway.agbak
```

恢复备份：

```bash
./ag restore --config config.json --from ai-gateway.agbak
```

如果备份来源配置了 `master_key`，恢复到不同环境时需要提供来源密钥：

```bash
./ag restore --config config.json --from ai-gateway.agbak --source-master-key OLD_MASTER_KEY
```

## 开发

运行测试：

```bash
go test ./...
```

本项目 `go.mod` 中包含 `personalpay/sdk-go` 的本地 `replace` 路径。如果在新机器上构建失败，请确保该本地 SDK 路径存在，或将其替换为可访问的模块来源。

## 目录结构

```text
cmd/server              程序入口
internal/app            启动、配置、备份恢复、Linux 服务命令
internal/web            Web 控制台、协议入口、静态资源和模板
internal/gateway        网关请求处理、计费、并发控制
internal/controlplane   管理端业务逻辑
internal/providers      OpenAI、Claude 等上游适配器
internal/storage        SQLite/PostgreSQL 存储实现
internal/routing        模型路由策略
internal/failover       账号故障切换
images                  README 截图
```
