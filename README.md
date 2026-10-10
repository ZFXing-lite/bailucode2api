# bailucode2api

bailucode 平台的 OpenAI 兼容 API 网关与多账号管理面板。

把你的 bailucode 账号变成一个标准的 OpenAI 格式 API 服务：多账号自动轮转、自动签到领额度、实时额度查询，全部通过一个 Web 后台管理。

## 功能特性

- **OpenAI 兼容网关**：`/v1/models`、`/v1/chat/completions`（支持流式），任何 OpenAI 客户端、ChatBox、LobeChat、沉浸式翻译等工具可直接对接
- **多账号池**：多账号 round-robin 轮转，单账号并发上限，失败自动切换下一个账号
- **自动签到**：每日 08:00 / 20:00（北京时间）自动领取两处每日额度，幂等设计，已领取自动跳过
- **Turnstile 三种求解模式**：`browser`（内置浏览器自动完成验证）/ `capsolver`（第三方求解服务）/ `manual`（人工领取）
- **额度查询**：每账号额度快照、汇总统计，保活探测时自动刷新
- **Web 管理后台**：账号导入、状态监控、一键签到、额度刷新，全图形化
- **安全存储**：账号密码与 API Key 使用 AES-GCM 加密落盘

## 快速部署

### 方式一：Docker（推荐）

```bash
git clone https://github.com/ZFXing-lite/bailucode2api.git
cd bailucode2api
cp .env.example .env
# 编辑 .env，至少修改 ADMIN_PASSWORD

docker compose up -d --build
```

默认镜像内不含浏览器（签到为 manual 模式）。如需容器内全自动签到，使用浏览器镜像：

```bash
docker compose -f docker-compose.browser.yml up -d --build
```

### 方式二：裸机部署

依赖：Go 1.22+、Node 18+（仅 browser 签到模式需要）、Chrome/Chromium 与 Xvfb（仅无显示器的服务器需要）。

```bash
git clone https://github.com/ZFXing-lite/bailucode2api.git
cd bailucode2api
cp .env.example .env
# 编辑 .env，至少修改 ADMIN_PASSWORD

# browser 签到模式需要安装依赖
cd scripts && npm install && cd ..

go build -o bailucode2api ./cmd/server
./bailucode2api
```

启动后访问 `http://localhost:18790/`。

## 使用说明

### 1. 登录后台

浏览器打开 `http://localhost:18790/`，使用 `.env` 中设置的 `ADMIN_USERNAME` / `ADMIN_PASSWORD` 登录。

### 2. 导入账号

后台「导入账号」文本框粘贴，每行一个账号：

```
邮箱----密码----APIKey
```

- `----` 为四横线分隔符
- 密码用于登录上游控制台完成签到与额度查询
- API Key 是上游平台分配给你的调用密钥
- 所有凭据 AES-GCM 加密存储，重复邮箱自动去重

### 3. 调用网关

任何 OpenAI 兼容客户端填入：

| 配置项 | 值 |
|---|---|
| Base URL | `http://localhost:18790/v1` |
| API Key | `.env` 中 `API_KEY` 的值（未设置则留空/任意） |

curl 示例：

```bash
curl http://localhost:18790/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <你的API_KEY>" \
  -d '{
    "model": "bailu-2.7-free",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'
```

模型列表可请求 `GET /v1/models` 获取。

### 4. 签到与额度

- **自动签到**：每日 08:00 / 20:00（北京时间）自动领取两处每日额度，无需人工干预
- **手动签到**：后台账号列表点「签到」，或点「立即签到（全账号）」
- **额度查询**：后台每账号显示 已用/剩余/总量、美元额度、定向额度包；顶栏显示全部账号汇总。点「刷额度」实时更新
- 签到与保活过程中会自动刷新额度快照

### 5. 健康检查

```bash
curl http://localhost:18790/health
```

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PORT` | `18790` | 监听端口 |
| `ADMIN_USERNAME` | `admin` | 后台用户名 |
| `ADMIN_PASSWORD` | `admin123` | 后台密码（务必修改） |
| `API_KEY` | 空 | 下游网关鉴权 key；空 = 开放访问，公网部署务必设置 |
| `DATA_DIR` | `/data` | 数据目录 |
| `UPSTREAM_BASE` | `https://bailucode.com/openapi/v1` | 上游 OpenAPI 地址 |
| `UPSTREAM_TIMEOUT` | `120s` | 单次上游请求超时 |
| `QUOTA_SYNC_INTERVAL` | `30m` | 保活探测间隔 |
| `QUOTA_REFRESH_ENABLED` | `true` | 保活时自动刷新额度快照 |
| `CHECKIN_ENABLED` | `true` | 自动签到开关 |
| `PANEL_BASE` | `https://bailucode.com` | 上游控制台地址 |
| `TURNSTILE_SOLVER` | `browser` | 签到验证模式：`browser` / `capsolver` / `manual` |
| `CAPSOLVER_KEY` | 空 | capsolver 模式的服务密钥 |
| `CHROME_PATH` | `/usr/bin/google-chrome` | browser 模式浏览器路径 |
| `CLAIM_HELPER_PATH` | `./scripts/claim_helper.js` | browser 模式脚本路径 |
| `MAX_INFLIGHT` | `4` | 单账号最大并发 |
| `MAX_REQUEST_BYTES` | `8388608` | 单请求体大小上限 |

## 签到模式说明

| 模式 | 适用场景 | 依赖 |
|---|---|---|
| `browser`（默认） | 服务器可安装浏览器，全自动 | Chrome/Chromium、Node 18+、Xvfb（无显示器时） |
| `capsolver` | 无浏览器环境，全自动 | capsolver 账号与密钥 |
| `manual` | 不自动签到，面板提示人工领取 | 无 |

## 常见问题

**签到一直失败？** 检查 browser 模式依赖是否齐全（`google-chrome`、`node`、`xvfb-run`）；服务器环境受限可切换 `capsolver` 模式；验证码策略变化时可临时用 `manual` 人工兜底，签到逻辑幂等，随时可重试。

**忘记后台密码？** 修改 `.env` 中 `ADMIN_PASSWORD` 后重启服务。

**如何安全暴露到公网？** 务必设置 `API_KEY`，用反向代理加 HTTPS，并修改默认后台密码。

## 目录结构

```
cmd/server/main.go          入口：HTTP 服务 + 保活 + 签到调度
internal/config             环境变量配置
internal/store               账号持久化 + AES-GCM 加密
internal/auth                后台会话登录
internal/bailu               上游控制台客户端（登录/状态/领取/额度）
internal/turnstile           签到验证求解器（browser/capsolver/manual）
internal/upstream            上游 OpenAPI 客户端
internal/pool                 账号轮转池（轮转 + 并发上限 + 熔断）
internal/checkin             签到调度器
internal/api                 HTTP 层：管理接口 + /v1 网关
internal/webui               管理后台页面
scripts/claim_helper.js     browser 模式签到脚本
```

## License

MIT
