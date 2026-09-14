---
name: hw-alert-analyzer
description: 硬件告警日志检索与诊断。当收到内核/硬件告警（网卡/内存/磁盘/RAID 卡/CPU/PCIe/温度等），需要从 Elasticsearch 检索服务器系统日志并给出诊断意见时使用。可根据服务器 IP/主机名、硬件类型、时间窗自动检索日志并输出结构化分析报告。
---

# 硬件告警日志检索与诊断

把"查日志 + 分析日志"合并为一个 agent 工作流：收到硬件告警后，自动从 Elasticsearch `syslog-system` 索引检索对应服务器、时间窗、硬件类型相关的日志，并按统一分析框架输出诊断报告。联动服务可通过 `agent.executor_url` 直接触发本 skill，运维人员也可直接调用。

## 触发场景

- n9e/监控产生硬件告警（`kernel_hardware_error_type{type=...}`），需要定位具体故障。
- 人工排查服务器硬件问题，需要快速从 ES 取日志并分析。

## 需要的信息

| 信息 | 必填 | 说明 |
| --- | --- | --- |
| 服务器 IP 或主机名 | 是 | 主机名需先解析为 IP（见"定位 IP"） |
| 硬件类型 type | 是 | nic/mem/raid/cpu/bus/kernel/thermal，可多个 |
| 时间窗 | 是 | start/end（RFC3339），或告警触发时间（向前回溯 10 分钟） |

## 工作流

1. **定位 IP**：若只有主机名，先查 `linkage/config.example.json` 的 `resolve.inventory`；未命中则用 ES 反查（按 message 含主机名 + 错误关键词聚合 `source.keyword` 提取 IP）。
2. **取关键词**：配置查找顺序为 `--config` 参数 → 环境变量 `LINKAGE_CONFIG` → 部署配置 `/etc/kernel-hw-linkage/config.json` → 仓库样例 `linkage/config.example.json`。从其中 `es.type_keywords` 读取该类型的 match_phrase 关键词（与采集端 `patterns.conf` 对齐）。
3. **检索日志**：执行 `scripts/es_search.sh --es <url> --ip <IP> --type <type> --start <RFC3339> --end <RFC3339> [--config <config.json>]`，得到命中行与 source 聚合。
4. **解读**：按行归硬件组件、识别错误签名、判断瞬时/持续/级联（规则见 `linkage/prompts/analyze.txt`）。
5. **输出**：严格按下方 Report JSON 输出；其中 `llm` 子对象字段必须严格遵循 `linkage/prompts/analyze.txt` 规定的 JSON schema。

## ES 检索说明（es_search.sh 已封装）

- 索引：`syslog-system`
- 文件名规则：`/syslog/system/<YYYY-MM-DD>/<IP>_<YYYY-MM-DD>.log`，脚本用通配 `source.keyword=/syslog/system/*/<IP>_*.log`（不拼日期，避免跨零点问题）。
- 时间字段：`@timestamp`。
- 关键词：类型对应的 `es.type_keywords`（match_phrase）。

```bash
scripts/es_search.sh --es http://es.example.com:9200 --user readonly --pass "$ES_PASS" \
  --ip 10.0.0.11 --type nic --type mem \
  --start 2026-09-13T09:50:00Z --end 2026-09-13T10:00:00Z \
  --config ../../../linkage/config.example.json
```

## 类型 → 检索关键词

| type | 硬件 | 代表关键词 |
| --- | --- | --- |
| nic | 网卡/链路 | Link is Down、NIC Link is Down、tx timeout、transmit queue、Carrier is off、NETDEV WATCHDOG、Port Link Down |
| mem | 内存/EDAC/MCE | EDAC MC、Memory error、Uncorrected Error、Corrected error、Machine Check、Hardware Error |
| raid | 磁盘/RAID 卡 | megaraid、I/O error、Buffer I/O error、blk_update_request、SCSI error、Medium Error、disk failure on、Recovery FAILED、controller reset、nvme controller is down、CISS_CMD_STATUS、SMART Failure |
| cpu | CPU 温度 | Core/Package temperature above threshold、Thermal event |
| bus | PCIe/IOMMU/ACPI | PCIe Bus Error、AER、DMAR、ACPI Error、PCI error |
| kernel | 内核致命 | Oops、Kernel panic、BUG:、soft/hard lockup、hung task、Call Trace、general protection fault |
| thermal | 温度/电源 | Critical temperature、overheat、Power supply failed |

（完整列表以 `config.json` 的 `es.type_keywords` 为准。）

## 分析规则

严格遵循 `linkage/prompts/analyze.txt`：只依据给定日志、不做臆测；先归类硬件组件再综合判断；结论可执行。该文件同时被联动服务（Go）与联动服务 LLM 加载，两侧一致。

## 输出（必须严格 JSON）

外层为完整 Report（hostname/server_ip/types/…/llm），其中 `llm` 子对象遵循 `prompts/analyze.txt`：

```json
{
  "hostname": "host01",
  "server_ip": "10.0.0.11",
  "types": ["nic"],
  "time_start": "2026-09-13 09:50:00",
  "time_end": "2026-09-13 10:00:00",
  "lines": [ { "message": "eth0: NIC Link is Down", "count": 3 } ],
  "llm": {
    "结论": "网卡链路抖动，链路中断",
    "可能原因": ["网线/光模块松动", "对端设备故障"],
    "影响评估": "业务流量中断",
    "建议动作": ["检查物理链路", "查看 ethtool -S 计数"],
    "紧急度": "高",
    "需进一步检查": ["dmesg 网卡驱动日志"]
  }
}
```

## 相关文件

- 检索脚本：`skills/hw-alert-analyzer/scripts/es_search.sh`
- 分析规则：`linkage/prompts/analyze.txt`
- 类型关键词与 inventory：`linkage/config.example.json`
- 采集端规则（关键词来源）：`kernel-hw-mon/patterns.conf`
- 类别一致性校验：`tools/check-type-alignment.sh`
