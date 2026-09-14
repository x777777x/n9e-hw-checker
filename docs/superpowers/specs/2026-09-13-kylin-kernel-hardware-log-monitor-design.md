# Kylin V10 内核/硬件错误日志监控 —— 设计文档

- 日期：2026-09-13
- 目标系统：Kylin V10（RHEL8 系，rsyslog + `/var/log/messages`）
- 监控平台：n9e（Nightingale）
- 采集方式：Categraf `exec` 插件（prometheus / influx / falcon 三种 `data_format`）或 node_exporter textfile collector
- 状态：已实现（2026-09 落地并多发行版容器验证；含类型指标/scan 模式等增量）

## 1. 目标与非目标

### 目标
- 从系统日志中发现 **内核级硬件异常**：网卡、内存、RAID/磁盘、CPU/MCE、PCIe/总线、温度等。
- 每次仅处理“上次运行以来新增的日志”，等价于每分钟检查前一分钟，**不漏报**。
- 同一错误在一分钟内反复刷屏，只输出一个告警信号，**不多报**。
- 对被测服务器性能影响可忽略；不引入安全风险。

### 非目标
- 不解析、不上报具体日志正文（正文通过 Kibana / 日志文件查询）。
- 不做根因分析、不做自动修复。
- 不替代 smartd / mcelog / RAID 厂商工具，仅做“日志层面的统一发现”。

## 2. 核心设计

### 2.1 数据流

```
/var/log/messages ──► kernel-hw-mon.sh ──► 状态文件(inode+offset+last_ts)
                          │
                          ├─ prometheus 文本 ──► Categraf exec(data_format=prometheus)
                          ├─ influx 文本     ──► Categraf exec(data_format=influx)
                          ├─ falcon JSON     ──► Categraf exec(data_format=falcon)
                          └─ .prom 文件       ──► node_exporter textfile collector
```

一个核心脚本 + 一个规则文件，三种输出格式通过 `--format` 切换，保证行为一致。

### 2.2 增量游标（防漏报/防重报的关键）

状态文件 `/var/lib/kernel-hw-mon/state` 记录：`<inode> <offset> <last_epoch>`。

每次运行：

1. `stat -Lc` 取当前 `/var/log/messages` 的 inode 与 size。
2. **首次运行**（无状态文件）：offset 直接置为当前 size，从 EOF 开始，避免把历史日志一次性重放造成告警风暴。
3. inode 与上次不同（logrotate 重命名+新建）→ offset 归零，读取新文件。
4. offset > size（`copytruncate` 截断）→ offset 归零。
5. 否则用 `tail -c +<offset+1>` 只读取增量，交给 gawk 处理。
6. gawk 用 `RT` 判断最后一行是否完整：**只消费以换行结尾的完整行**，最后一段不完整的行不消费、不推进游标，下次补齐后重读。
7. 新 offset = 旧 offset + 已消费字节数，原子写回状态文件。

> 关键点：游标永远停在“行边界”，因此不存在读到半行的问题，也天然避免重复。
> 若 cron/Categraf 调度延迟，只会补读更多增量，不会漏报。

### 2.3 匹配规则（防漏报/防多报）

规则文件 `/etc/kernel-hw-mon/patterns.conf`，Tab 分隔：

```
source <TAB> <regex>            # 来源过滤：只信任 kernel/mcelog/smartd 等
include <TAB> <category> <TAB> <regex>
exclude <TAB> <regex>
```

- **来源过滤**：只有 syslog tag 属于 `kernel`、`mcelog`、`smartd`、`rasdaemon`、`ipmi*`、`mdadm`、`megaraid*` 等的行才进入匹配，避免误匹配应用日志里的 “I/O error”。
- **include**：按类别覆盖网卡、内存/CPU/MCE、RAID/磁盘、PCIe/总线、温度、内核致命异常（oops/panic/hung task/soft lockup/Call Trace）。
- **exclude**：排除正常事件，如 `Link is Up`、`link becomes ready`、`Link up, ready in`。
- 命中任意 include 且未被 exclude、且来源合法 → `found=1`。

### 2.4 指标设计

主指标（0/1 布尔）：

| 指标 | 含义 |
| --- | --- |
| `kernel_hardware_error` | 本窗口内发现硬件/内核异常 = 1，否则 0 |

辅助健康指标（保证“监控自身可用”可观测，避免监控死了没人知道）：

| 指标 | 含义 |
| --- | --- |
| `kernel_hw_mon_failed` | 脚本执行失败（日志不可读、gawk 失败、规则缺失）= 1 |
| `kernel_hw_mon_last_run_timestamp` | 上次成功运行的 Unix 时间戳 |

因为主指标是 0/1，日志风暴只会让它保持 1，不会产生告警风暴。失败场景走独立指标，不会与硬件错误混淆。

### 2.5 性能与安全

- **性能**：只读增量（通常几 KB），`tail -c` 走 seek 不重扫全文件，gawk 流式单遍，耗时毫秒级；状态文件单次原子写。无临时大文件。
- **安全**：脚本只读日志、只写自有状态目录；不执行日志内容（仅正则匹配），无注入面；不以 root 常驻。
- **权限**：`/var/log/messages` 默认 `600 root:root`，普通用户读不到。推荐由 rsyslog 额外输出一份 `640 root:categraf` 的专用日志（无需 sudo），备选 sudoers 最小授权或加入 `adm` 组。详见部署 README。
- **可用性**：状态文件损坏自动重建；gawk 失败不推进游标，下次重试；首跑不重放历史。

### 2.6 已知限制

- `copytruncate` 轮转存在竞态：截断到 0 后、脚本运行前若又写入超过旧 offset 的内容，会漏读头部。RHEL/Kylin 默认 `/var/log/messages` 使用 `create` 轮转，不触发该问题；如使用 `copytruncate`，建议改用 `create`。
- 首次运行从 EOF 开始，部署前 1 分钟内的错误不会补报（避免历史重放风暴）。
- 多行栈回溯依赖每条 printk 独立成行，rsyslog 场景满足。

## 3. 告警规则（n9e）

```
kernel_hardware_error > 0            持续 1m，等级 P1
kernel_hw_mon_failed > 0             持续 3m，等级 P2（监控失联）
time() - kernel_hw_mon_last_run_timestamp > 300   持续 1m，P2（监控停摆）
```

恢复条件分别为对应指标回落并持续若干周期。

## 4. 验证方案

- 用 `logger -t kernel -p kern.err "..."` 注入网卡/内存/RAID 模拟日志，确认 `found=1`。
- 连续运行两次，第二次应为 0（不重报）。
- 无换行结尾的写半行场景：确认不消费、补齐后可捕获。
- `logrotate -f` 触发轮转，确认 inode 变化后从新文件读取。
- 连续刷屏同一条错误，确认主指标恒为 1。
- 规则缺失/日志不可读，确认 `kernel_hw_mon_failed=1`。
