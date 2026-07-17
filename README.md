# SituationAwareness Master

态势感知主控服务。Master 默认监听 `8001`，从本机节点注册服务获取 Agent 列表，通过各节点的 `8002` 端口下发结构化探测任务，并汇总结果供 Web 展示。

## 调度流程

1. Web 调用 `POST /api/v1/probes` 提交目标地址。
2. Master 请求 `http://127.0.0.1:8888/api/nodes`。
3. 只保留 `status=enabled`、`port=8002` 且 IP 合法的节点；注册中心中的 8001 主控节点会自动忽略。
4. Master 并发调用 `http://节点IP:8002/api/v1/tasks`。
5. Master 汇总每个 Agent 的 DNS、TCP、HTTP 结果并返回 Web。

## 快速启动

```bash
cp .env.example .env
export AGENT_SHARED_TOKEN='与所有 Agent 相同的长随机字符串'
go run ./cmd/master
```

```bash
curl http://127.0.0.1:8001/healthz
curl http://127.0.0.1:8001/api/v1/nodes
curl -X POST http://127.0.0.1:8001/api/v1/probes \
  -H 'Content-Type: application/json' \
  -d '{"target":"https://example.com","options":{"timeoutMs":10000,"ports":[80,443]}}'
```

## 配置参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `MASTER_LISTEN_ADDR` | `:8001` | Master 对 Web 提供 API 的监听地址 |
| `NODE_REGISTRY_URL` | `http://127.0.0.1:8888/api/nodes` | 节点注册中心列表接口 |
| `AGENT_PORT` | `8002` | 允许调度的 Agent 端口，同时用于排除主控节点 |
| `AGENT_TASK_PATH` | `/api/v1/tasks` | Agent 任务接口路径 |
| `AGENT_SHARED_TOKEN` | 空 | 发送给 Agent 的 Bearer 令牌；生产环境必须设置 |
| `MASTER_MAX_PARALLEL` | `20` | 整个 Master 同时向 Agent 发出的最大请求数 |
| `REGISTRY_TIMEOUT` | `3s` | 注册中心请求超时 |
| `TASK_DEFAULT_TIMEOUT` | `10s` | Web 未指定时的探测超时 |
| `TASK_MAX_TIMEOUT` | `30s` | Web 可指定的最大探测超时 |
| `CORS_ALLOWED_ORIGIN` | `*` | Web 跨域来源；生产环境建议设置成实际域名 |

## API

### `GET /api/v1/nodes`

返回当前可调度 Agent。返回内容已经排除了禁用、维护、非 8002 以及无效 IP 的记录。

### `POST /api/v1/probes`

```json
{
  "target": "https://example.com",
  "nodeIds": ["6a4e19c4b4f2bd88d47efb22"],
  "options": {
    "timeoutMs": 10000,
    "ports": [80, 443]
  }
}
```

`nodeIds` 可省略，省略时调度全部合格 Agent。Master 对注册中心故障返回 502，对无可用/无匹配节点返回 422；个别 Agent 失败不会中断其他节点，失败信息会进入汇总结果。

## 验证

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o bin/situation-awareness-master ./cmd/master
```
