# Asset Proxy

一个带缓存的 HTTP 代理 + 可控源站，以及一次性验收服务。代理只处理
`GET /assets/{id}`。

## 行为约定

- **可缓存性**：仅缓存 `200` 且带 `ETag`、`Cache-Control: max-age=N`
  （`0 <= N <= 60`）的响应；`Vary` 只允许 `Accept-Language`，出现任何其他
  字段（含 `*`）的响应一律不缓存。
- **变体**：缓存键 = 资源 id + 语言变体。仅支持 `zh` / `en` 两种变体
  （取自 `Accept-Language` 中 q 值最高的语言）；其他语言的请求完全绕过
  缓存直接回源。一种语言的缓存绝不会交给另一种语言。
- **过期回源**：过期后用 `If-None-Match` 发起条件请求；源站 `304` 会
  延长原响应的新鲜期（若 304 携带新的 `max-age` 则采用之）。
- **并发合并**：同一资源、同一变体的并发请求只触发一次回源；回源在
  独立上下文中进行，任一等待者取消都不会中断回源或其他等待者。
- **出错回退**：源站 5xx 或不可达时，最多返回过期 30 秒内的旧响应
  （可用 `STALE_IF_ERROR` 调整），并以 `Warning: 110` 与
  `X-Proxy-Stale: true` 明确标记；超过窗口返回 `502`。
- **客户端条件请求**：`If-None-Match` 只与同一变体的缓存项比较，
  绝不会因其他语言的 ETag 而返回 `304`。

### 代理附加的响应头

| 头 | 含义 |
| --- | --- |
| `X-Proxy-Cache` | `HIT` / `MISS` / `REVALIDATED` / `STALE` / `PASS` |
| `X-Proxy-Stale` | 陈旧响应时为 `true` |
| `Warning` | 陈旧响应时为 `110 - "Response is stale"` |
| `Age` | 自缓存时刻起的秒数 |

## 目录结构

```
cmd/proxy       缓存代理（默认 :8080）
cmd/origin      可控源站（默认 :9000），含 /control/* 控制端点
cmd/verify      一次性端到端验收程序
internal/proxy  代理实现（缓存、singleflight、可注入时钟）
internal/origin 源站实现
```

## 配置（环境变量）

代理：`PORT`、`ORIGIN_URL`、`STALE_IF_ERROR`（默认 `30s`）、
`FETCH_TIMEOUT`（默认 `10s`）。
源站：`PORT`。

## 本地运行

```sh
go test ./...                              # 单元测试（假时钟 + 假源站）
go run ./cmd/origin &                      # :9000
STALE_IF_ERROR=3s go run ./cmd/proxy &     # :8080

curl -s -H 'Accept-Language: zh' http://localhost:8080/assets/1
curl -s -H 'Accept-Language: en' http://localhost:8080/assets/1
```

## Docker Compose

```sh
# 启动代理 + 源站
docker compose up --build proxy

# 验收：运行一次性 verify 服务，执行完整自动测试
docker compose up --build --abort-on-container-exit --exit-code-from verify verify
echo $?                  # 0 = 全部通过
docker compose down
```

Compose 中将 `STALE_IF_ERROR` 设为 `3s` 以便快速走完陈旧窗口用例；
精确的 30 秒边界由单元测试用假时钟覆盖。

## 源站控制 API

- `POST /control/config` — 部分 JSON 更新：
  `{"maxAge":2,"mode":"ok|error","delayMs":0,"version":1,"vary":"accept-language|accept-encoding|both|star|none","noETag":false}`
- `GET /control/stats` — 计数器（`hits`、`conditionalHits`、按路径统计）
- `POST /control/reset` — 恢复默认配置并清零计数器
