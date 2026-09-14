# Kylin 硬件告警 × ES 检索 × 大模型联动诊断系统 —— 设计文档

- 日期：2026-09-13
- 关联文档：`2026-09-13-kylin-kernel-hardware-log-monitor-design.md`（采集端，本文对其做增量增强）
- 前置：n9e 监控 + kernel-hw-mon 已上线；日志已采集到 Elasticsearch 索引 `syslog-system`
- 状态：设计已评审确认

## 1. 背景与目标

### 目标
- 硬件告警触发后，自动从 ES 检索该服务器对应时间窗的原始日志，交给私有化大模型生成诊断意见。
- 通过**按硬件类型区分**，让 ES 查询更精确、LLM 上下文更聚焦。
- 分析结果多路回写：企业微信、邮件、UniPro 工单。

### 非目标
- 不替代告警本身（告警送达不依赖本系统，fail-open）。
- 不做自动修复。
- 不解析 ES 里除 `syslog-system` 以外的索引。

### 关键事实与假设（已确认）
- ES 文档可用字段仅 `source.keyword`（文件名，规则 `/syslog/system/<YYYY-MM-DD>/<IP>_<YYYY-MM-DD>.log`）与 `message`（行内容），**无 host 字段** → 需要 ident→IP 映射。
- ES 索引内容为各服务器 `/var/log/messages` 的采集结果，与 kernel-hw-mon 检测的内容同源（专用文件是 messages 的子集）。
- 大模型**私有化部署**（openai 兼容端点），日志不出内网。
- 回写目标：企业微信、邮件、UniPro 工单。

## 2. 总体架构

```
kernel-hw-mon（增强：按类型输出指标）
   │
   ▼
n9e 告警（总指标规则 + 类型规则，回调带 type）
   │  webhook
   ▼
联动服务（Go：Gin 入口 + Redis 去重/聚合 + asynq 后台 worker）
   ├─ ① 鉴权 & 容错解析
   ├─ ② 幂等去重 + 同窗口多类型合并
   ├─ ③ ident→IP 映射（清单优先，ES 反查兜底）
   ├─ ④ ES 精确检索（按 type 关键词 + 时间窗 + source）
   ├─ ⑤ 日志聚合（去重/排序/截断，控制 token）
   ├─ ⑥ LLM 分析（私有化端点，输出结构化 JSON）
   └─ ⑦ 回写：企业微信 / 邮件 / UniPro 工单
```

## 3. 组件设计

### 3.1 kernel-hw-mon 增强（采集端）

在现有单指标基础上**增量**增加按类型指标，保持向后兼容：

```
# prometheus 格式（canonical）
kernel_hardware_error                          # 总指标，0/1，原有告警不破坏
kernel_hardware_error_type{type="nic"} 1       # 7 类各恒出 0/1
kernel_hardware_error_type{type="mem"} 0
kernel_hardware_error_type{type="raid"} 0
kernel_hardware_error_type{type="cpu"} 0
kernel_hardware_error_type{type="bus"} 0
kernel_hardware_error_type{type="kernel"} 0
kernel_hardware_error_type{type="thermal"} 0
kernel_hw_mon_failed 0
kernel_hw_mon_last_run_timestamp 1789312163
```

- awk 在命中 include 时记录类别（`cname[i]` 已存在），END 输出命中类别集合。
- **类别集合不硬编码**：每次运行从 `patterns.conf` 解析 distinct include 类别，逐类输出 0/1 → 新增类别自动出现。
- influx / falcon / textfile 格式同步支持（type 作为 tag/独立 metric）。

**7 类划分与语义**（来自 patterns.conf）：

| type | 覆盖 | 代表关键词 |
| --- | --- | --- |
| nic | 网卡/链路/驱动 | Link is Down、link is not ready、NIC Link is Down、Port Link Down、transmit queue timed out、tx timeout、transmit timeout、Carrier is off、NETDEV WATCHDOG |
| mem | 内存/EDAC/MCE | EDAC MC、Memory error、CE/UE、Uncorrected/Corrected Error、mce:、Machine Check、MCG status、Hardware Error |
| cpu | CPU 温度/热事件 | CPUx: Core/Package temperature above threshold、Thermal event |
| raid | 磁盘/RAID/存储 | megaraid、I/O error、Buffer I/O error、blk_update_request、SCSI error、Medium Error、Sense Key、timing out command、md/raid、SMART 系列、软/硬 reset failed |
| bus | PCIe/IOMMU/ACPI | PCIe Bus Error、AER:、DMAR:、ACPI Error、PCI error、Uncorrectable/Correctable error |
| kernel | 内核致命异常 | Oops、Kernel panic、BUG:、general protection fault、soft/hard lockup、hung task、Call Trace、watchdog BUG、RCU Stall |
| thermal | 温度/电源 | Critical temperature、thermal.*critical、overheat、Power supply failed |

> 分类原则：按关键词归属组件；存在天然重叠（MCE 内存/CPU、温度跨 cpu/thermal）→ **一行命中多类取并集**，同窗口内多类可同时为 1。

### 3.2 n9e 告警与回调

- 保留规则：`kernel_hardware_error > 0`（P1，总告警，面向值班可见性）。
- 联动规则：`kernel_hardware_error_type > 0`（P1，逐 type 触发，**回调天然携带 type 标签**）。
- 告警回调（webhook）配置共享密钥；payload 字段按实际 n9e 版本校准，服务端**容错解析**。
- 关键字段：`ident/labels`、`trigger_time`、`rule_name`、`is_recovered`、`type`。

### 3.3 联动服务

**技术栈**：Go 1.2x + Gin（HTTP 入口）+ asynq（Redis 后台任务）+ go-redis + `elastic/go-elasticsearch` + `net/http`（LLM 私有端点/企微/UniPro）+ `net/smtp`（邮件）。单一静态二进制，与 n9e/categraf 的 Go 生态一致。

**① 入口与鉴权**
- `POST /webhook`，校验 `X-Webhook-Token` 与配置的共享密钥一致；来源 IP 白名单可选。
- `is_recovered=true` 的事件直接丢弃（只分析触发，不分析恢复）。

**② 去重与合并（关键，防 LLM 风暴）**
- 去重键：`sha1(ident + type + 10min 时间窗)`，Redis `SETNX` + TTL 30min。
- 合并：同一 `(ident, 时间窗)` 内多个 type 回调，在 Redis 缓冲等待（如 60s 聚合窗口）→ 汇总所有 type 一次分析。任一 type 单独超时也触发。
- 效果：同分钟 nic+mem 同时触发 = 一份综合报告，各 type 分别精确查 ES，最后合并给 LLM。

**③ ident→IP 映射**
- ES 无 host 字段，文件按 IP 命名。
- 首选：服务内静态清单 `{hostname: ip}`（配置文件/环境变量，小规模够用）。
- 兜底：ES 反查 —— 时间窗内搜 `message` 含 hostname + 错误关键词，取命中 `source.keyword` 反推 IP。
- 映射失败 → 标记"定位失败"，告警照常送达，不阻塞。

**④ ES 精确检索（按 type）**

查询策略：**不拼文件名日期**（避免跨零点/IP 对齐问题），用通配 source + 时间窗 + 类别关键词。
时间字段假设为 filebeat 默认的 `@timestamp`；若实际索引时间字段不同，仅需调整 range 字段名。

```
POST syslog-system/_search
{
  "size": 100,
  "query": { "bool": { "filter": [
      { "range": { "@timestamp": { "gte": "<T-10m>", "lte": "<T>" } } },
      { "wildcard": { "source.keyword": "/syslog/system/*/<IP>_*.log" } },
      { "bool": { "should": [
          { "match_phrase": { "message": "Link is Down" } },
          { "match_phrase": { "message": "tx timeout" } },
          ...  # 仅该 type 的 ES 关键词
      ] } }
  ] } },
  "aggs": { "by_source": { "terms": { "field": "source.keyword", "size": 5 } } },
  "sort": [{ "@timestamp": "asc" }]
}
```

- **type→ES 关键词表**：`es_keywords.yaml`，每 type 一组 `match_phrase` 子串，初版手工对齐 patterns.conf；用 smoke 断言校验两边 type 集合一致（patterns.conf include 类别 ⊆ 表内 type），避免漂移。
- 先 `by_source` 聚合确认命中文件/IP，再取回 message 行。

**⑤ 日志聚合（token 预算）**
- 相同 message 合并计数（`Link is Down ×120`）。
- 按频率排序取 **top 30 种**，单行截断 200 字符；上下文总预算 ~4k token。
- 附带每行命中的 type 标注。

**⑥ LLM 分析**
- 私有化 openai 兼容端点（`base_url` + `model` 配置化），**日志不出内网**。
- System prompt：SRE 硬件排障专家，只依据提供日志、不臆测。
- 输入：服务器 IP、时间窗、告警类型、聚合后的日志行。
- 输出（结构化 JSON）：
```json
{
  "结论": "...",
  "可能原因": ["..."],
  "影响评估": "...",
  "建议动作": ["..."],
  "紧急度": "高/中/低",
  "需进一步检查": ["..."]
}
```
- 失败处理：重试 1 次，仍失败回写"分析失败"，不影响告警。

**⑦ 结果回写（三路适配器，可插拔）**
- `Report` 统一结构体；各通道独立，单路失败不影响其它。
- 企业微信：群机器人 markdown 消息。
- 邮件：SMTP，正文为分析报告。
- UniPro 工单：REST OpenAPI 建单，适配器接口化（具体 API 参数按实际部署补全）。

## 4. 数据流（一次完整触发）

```
T  事件写入 /var/log/messages ──► filebeat ──► ES syslog-system
T+1m  kernel-hw-mon 检测 → kernel_hardware_error_type{type="nic"}=1
T+2m  n9e 评估命中 → webhook(ident,type=nic,trigger_time)
T+2m  联动服务鉴权→去重→(60s 聚合)→ident→IP→ES 查询→日志聚合
T+3m  LLM 分析 → JSON 报告
T+4m  回写企微/邮件/UniPro
```

## 5. 可靠性 / 安全 / 可观测

- **fail-open**：ES/LLM/回写任意环节失败，告警送达不受影响；联动侧记录并重试（指数退避 ≤3 次）。
- **超时**：ES 查询 5s、LLM 30s、worker 任务 60s，避免占用 worker。
- **凭据**：ES 只读角色、LLM key、企微 webhook、SMTP、UniPro token 全部走环境变量/密钥管理，不入代码。
- **数据安全**：LLM 私有化，日志不出内网；ES 用只读账号；回调鉴权。
- **可观测**：每阶段耗时与结果计数指标（analysis_success/fail、dedup_skipped、es_hits），日志结构化。
- **去重防风暴**：Redis 去重 + 聚合窗口双保险，同一事件 30min 内最多分析一次。

## 6. 部署形态

### 6.1 运行形态

- 单一 Go 静态二进制，两种 mode：`server`（Gin 入口）与 `worker`（asynq 消费），可同机或拆分部署。
- Redis 独立实例（复用现有集群即可）。
- systemd 管理；配置用 YAML + 环境变量（密钥只走环境/文件权限 600）。

### 6.2 Ansible 部署方案（推荐）

#### 目录结构（仓库 `ansible/` 下）

```
ansible/
├── ansible.cfg
├── requirements.yml            # collections: community.general 等
├── inventory/
│   └── hosts.yml               # [linkage] 联动服务主机; [monitored] 受监控服务器
├── group_vars/
│   ├── all.yml                 # ES/Redis/LLM/通知通道等全局参数（非密钥）
│   ├── linkage.yml             # 联动服务参数、ident→IP 清单
│   └── secrets.yml             # 密钥，Ansible Vault 加密
├── playbooks/
│   ├── deploy-linkage.yml      # 部署联动服务（server + worker 两个 unit）
│   ├── deploy-monitor.yml      # 部署采集端 kernel-hw-mon（可选角色）
│   └── site.yml                # 一键全量
└── roles/
    ├── linkage/                # tasks/main.yml + templates + handlers
    └── hw-mon/                 # 采集端：脚本/patterns/rsyslog/logrotate/categraf
```

#### inventory 示例

```yaml
all:
  children:
    linkage:
      hosts:
        linkage01: { ansible_host: 10.0.0.10 }
    monitored:
      hosts:
        server01: { ansible_host: 10.0.0.11, hw_mon_ident: host01 }
```

#### linkage 角色核心内容

- **任务**：创建运行用户 → 拷贝版本化二进制 → 从模板渲染 `config.yaml` → 渲染两个 systemd unit（`kernel-hw-linkage-server.service`、`kernel-hw-linkage-worker.service`）→ 写 `EnvironmentFile`（0600，含密钥）→ daemon-reload + enable + restart。
- **配置模板**（`config.yaml.j2`）：ES 地址/索引/只读凭据、Redis 地址、LLM `base_url`/`model`、去重与聚合窗口、ident→IP 清单（来自 group_vars）、通知通道参数（企微 webhook / SMTP / UniPro）——**密钥字段引用 Vault 变量，不落明文**。
- **systemd unit 模板**：`User={{ linkage_user }}`、`Restart=on-failure`、`NoNewPrivileges=true`、`PrivateTmp=true`、`ProtectSystem=full`、`ProtectHome=true`、`ReadWritePaths=/var/lib/kernel-hw-linkage`。
- **回滚**：二进制按版本命名（`kernel-hw-linkage-{{ version }}`），回滚即 `ansible-playbook deploy-linkage.yml -e linkage_version=<上一版本>` 重指 unit。
- **部署后自检**：等待服务 active → `curl /healthz` → 用干跑 payload 触发一次 webhook 冒烟验证。
- **密钥管理**：`ansible-vault encrypt group_vars/secrets.yml`，执行时 `--ask-vault-pass` 或挂载 vault 凭证。

#### hw-mon 角色（可选，同一套自动化）

拷贝 `kernel-hw-mon.sh`/`patterns.conf` → 装 rsyslog `49-kernel-hw.conf` + logrotate → 配置 Categraf exec → handler 重启 rsyslog/categraf。与联动服务一起由 `site.yml` 编排，实现“采集端 + 联动服务”一键部署。

#### 使用示例

```bash
ansible-galaxy install -r ansible/requirements.yml
ansible-playbook -i ansible/inventory/hosts.yml ansible/playbooks/site.yml --ask-vault-pass
```

#### 与现有环境的关系

- Redis、ES、私有 LLM 均假设已存在，playbook 只接入不部署（如缺 Redis，可加一个可选 role 部署容器版）。

## 7. 边界与已知限制

- 只针对 `syslog-system` 索引；文件名规则变更需同步调整 wildcard。
- type 由检测端（patterns.conf）定义，ES 关键词表需人工对齐（有 smoke 校验兜底）。
- UniPro 建单 API、n9e 回调 payload 字段名待实际环境校准。
- 大模型结论为辅助意见，需人工复核。

## 8. 验证方案

- **采集端**：容器注入各类错误 → 断言 `kernel_hardware_error_type{type=...}` 正确置 1；多类型同窗口并集正确。
- **联动服务**：
  - 单测：模拟 n9e payload → 去重/合并 → 映射 → ES（mock）→ LLM（stub）→ 各通道输出。
  - 集成：真实 n9e 回调 + 真实 ES 命中 + 私有 LLM。
  - 故障注入：ES 超时/LLM 挂 → 确认 fail-open 且重试。
- **回归**：采集端原有 47 项 verify 全部保持通过（指标增量为向后兼容）。
