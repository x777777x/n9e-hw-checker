# kernel-hw-linkage — 硬件告警联动诊断服务（Go）

告警触发 → ES 精确检索（按硬件类型）→ 私有化大模型分析 → 多路回写。

设计文档：`docs/superpowers/specs/2026-09-13-kylin-hw-alert-es-llm-linkage-design.md`

## 数据流

```
n9e 回调 (kernel_hardware_error_type>0, 带 type 标签)
   │ POST /webhook (X-Webhook-Token)
   ▼
webhook → 鉴权 → 丢弃恢复事件 → 幂等去重(ident,type,窗口)
   → 多类型合并缓冲(同 ident+窗口, 60s) → 一次综合分析
   → 执行方式二选一：
     · 进程内：ident→IP → ES 精确查询(类型关键词+时间窗+source通配)
               → 日志聚合(去重/topN/截断) → 私有化 LLM → 结构化 JSON
     · 外部 agent：直接把事件交给 agent 执行器
               （加载 hw-alert-analyzer skill 自行检索+分析）
   → 回写: 企业微信 / 邮件 / UniPro 工单（单路失败不影响其它）
```

## Agent skill：检索 + 分析一体化

"查日志 + 分析日志"已封装为 agent skill（`skills/hw-alert-analyzer/`），
告警可通过 `agent.executor_url` **直接触发 agent** 工作，也可由运维直接调用：

- `SKILL.md`：触发条件、工作流、类型→关键词表、输出 schema
- `scripts/es_search.sh`：封装 ES 检索（按 IP + 时间窗 + 类型关键词），关键词单一事实源读 `config.example.json`
- 分析规则：`prompts/analyze.txt`，**服务端与 skill 共用**（服务端通过 `llm.prompt_file` 加载，缺省用内置兜底），两侧永远一致

启用 agent 直触：

```json
"agent": { "enabled": true, "executor_url": "http://agent-host:9000/invoke", "skill": "hw-alert-analyzer", "timeout_s": 120 }
```

`executor_url` 收到 `Incident`（hostname/server_ip/types/time_start/time_end/rule_name/skill），返回 `Report` JSON。

## 构建与运行

```bash
go build -o kernel-hw-linkage ./cmd/linkage
cp config.example.json /etc/kernel-hw-linkage/config.json
LINKAGE_TOKEN=xxx LINKAGE_ES_PASS=xxx LINKAGE_LLM_KEY=xxx \
  ./kernel-hw-linkage --config /etc/kernel-hw-linkage/config.json
```

密钥建议走环境变量（`LINKAGE_TOKEN`、`LINKAGE_ES_PASS`、`LINKAGE_LLM_KEY`、
`LINKAGE_WECOM_URL`、`LINKAGE_SMTP_PASS`、`LINKAGE_UNIPRO_TOKEN`），不落配置文件。

## 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 健康检查 |
| POST | `/webhook` | n9e 告警回调，头 `X-Webhook-Token` 鉴权 |

n9e 回调 payload（容错解析，兼容嵌套/扁平）关键字段：
`event.is_recovered`、`event.trigger_time`(unix)、`event.labels.ident`(hostname)、`event.labels.type`(nic/mem/...)、`event.rule_name`。

## 配置说明

`config.example.json` 为完整样例，要点：

- `es.type_keywords`：`type -> match_phrase 关键词`，需与采集端 `patterns.conf` 的 include 类别对齐。
- `resolve.inventory`：`hostname -> IP` 清单；缺失时自动用 ES 反查兜底。
- `window.dedup_minutes`（去重窗口 10）、`window.merge_seconds`（多类型合并 60）、
  `window.es_lookback_min`（ES 向前回溯 10）。
- `notify`：企微群机器人 / SMTP / UniPro（UniPro 建单 API 需按实际部署补全，见 `internal/notify/notify.go`）。

## 测试

### 单元 + 集成测试

```bash
go test ./...
```

`internal/app/integration_test.go` 用 mock ES / mock LLM / mock 企微做了**全链路端到端**验证：
webhook → 鉴权(401) → 幂等去重 → 多类型合并 → ident→IP → ES 查询 → 聚合 → LLM → 企微回写，
覆盖单类型、同窗口 nic+mem 合并、重复事件去重三个场景。

### 跨系统容器验证

```bash
# 编译 linux 静态测试二进制后，可在任意发行版容器中运行（无需 Go 工具链）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o kernel-hw-linkage-integration.test ./internal/app
docker run --rm -v "$PWD":/linkage:ro rockylinux:9 /linkage/kernel-hw-linkage-integration.test -test.v
```

已在 Rocky8/9、Alma9、Ubuntu 22.04/24.04、Debian 12、CentOS 7 六个发行版容器全部通过。

### 类别一致性校验

`patterns.conf` 的 include 类别必须与 `config.json` 的 `es.type_keywords` 对齐，
用 `tools/check-type-alignment.sh` 校验（建议接入 CI/部署前执行）：

```bash
bash tools/check-type-alignment.sh kernel-hw-mon/patterns.conf linkage/config.example.json
```

已在上述六个发行版容器验证通过。

## 与设计文档的差异（实现说明）

本实现为**纯标准库**（无第三方依赖，可离线编译）：

| 设计文档 | 本实现 | 切换方式 |
| --- | --- | --- |
| Gin | `net/http` | 路由仅 2 个端点，可平替 |
| go-redis | 进程内 `dedup.Deduper` | 实现 `Deduper` 接口换 Redis 版 |
| asynq worker | 进程内聚合缓冲 + 回调 | `buffer.Aggregator` 回调可改为入队 asynq |
| YAML 配置 | JSON 配置 | 换成 yaml/viper，结构不变 |

当前为单进程（webhook + 后台执行一体）。如需多实例/持久化队列，按上表接入 Redis+asynq 后
可拆 `server` / `worker` 两个进程（`cmd/linkage` 已预留 `--mode` 思路）。
