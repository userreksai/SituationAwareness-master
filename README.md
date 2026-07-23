# SituationAwareness Master

态势感知主控服务。Master 默认监听 `8001`，从本机节点注册服务获取 Agent 列表，通过各节点的 `8002` 端口下发结构化探测任务，并汇总结果供 Web 展示。

## 调度流程

1. Web 调用 `POST /api/v1/probes` 提交目标地址。
2. Master 请求 `http://127.0.0.1:8888/api/nodes`。
3. 只保留 `status=enabled`、`port=8002` 且 IP 合法的节点，并每分钟请求 Agent 的 `/healthz`。
4. 只有健康状态为 `online` 的 Agent 才会参与任务调度；进程停止或网络中断的节点会标记为 `offline`。
5. Master 并发调用 `http://节点IP:8002/api/v1/tasks`。
6. Master 汇总每个 Agent 的 DNS、TCP、HTTP 结果并返回 Web。

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
| `AGENT_HEALTH_INTERVAL` | `1m` | Agent 健康检查周期；Master 启动时会立即检查一次 |
| `AGENT_HEALTH_TIMEOUT` | `3s` | 单个 Agent `/healthz` 请求超时 |
| `AGENT_SHARED_TOKEN` | 空 | 发送给 Agent 的 Bearer 令牌；生产环境必须设置 |
| `MASTER_MAX_PARALLEL` | `20` | 整个 Master 同时向 Agent 发出的最大请求数 |
| `REGISTRY_TIMEOUT` | `3s` | 注册中心请求超时 |
| `TASK_DEFAULT_TIMEOUT` | `10s` | Web 未指定时的探测超时 |
| `TASK_MAX_TIMEOUT` | `30s` | Web 可指定的最大探测超时 |
| `CORS_ALLOWED_ORIGIN` | `*` | Web 跨域来源；生产环境建议设置成实际域名 |

## 批量检测与数据收敛

批量接口最多接收 200 个目标，并限制单个任务展开后不超过 20,000 次节点检测。Master 最多并行处理 10 个目标，实际 Agent 请求并发仍受 `MASTER_MAX_PARALLEL` 统一限制。

- `POST /api/v1/probes/batch`：提交后台批量任务并返回 `202 Accepted`、任务 ID 和初始状态，不等待所有节点完成。
- `GET /api/v1/probes/batch/{batchTaskId}`：读取任务状态、真实目标进度和目标级汇总；前端轮询到 `status=completed` 后进入结果页。
- `GET /api/v1/probes/batch/{batchTaskId}/targets/{targetIndex}`：按需读取一个目标的完整节点明细。

完整明细保存在 Master 内存中 30 分钟，最多保留最近 10 个批量任务；服务重启后失效。前端因此可以先分页展示轻量汇总，用户展开目标时再加载详细结果。

汇总中的 `failed` 仅表示 Master 未能完成 Agent 调度；`abnormal` 表示 Agent 已返回结果，但 DNS、HTTP 或整体可达性存在异常。批量总览分别通过 `failedNodeChecks` 和 `abnormalNodeChecks` 汇总这两类计数，避免把“调度成功但探测异常”错误显示为 0 次失败。

请求示例：

```json
{
  "targets": ["https://example.com", "1.1.1.1"],
  "nodeIds": ["6a4e19c4b4f2bd88d47efb22"],
  "options": {
    "timeoutMs": 10000,
    "ports": [80, 443]
  }
}
```

## API

### `GET /api/v1/nodes`

返回注册中心中符合基本条件的 Agent，并附带 `healthStatus`、`healthCheckedAt`、`healthLatencyMs` 和可选的 `healthError`。`count` 表示当前在线且可调度的节点数，`totalCount` 表示返回的节点总数。离线节点仍会返回供前端展示，但不会参与任务调度。

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
