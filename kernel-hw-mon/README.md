# kernel-hw-mon — 内核/硬件错误日志监控

从系统日志增量发现内核级硬件异常（网卡、内存/EDAC、RAID/磁盘、CPU/MCE、PCIe/总线、温度、内核致命异常），接入 n9e 告警。

- 只处理“上次运行以来的新增日志”，每分钟一次，等价于检查前一分钟，**不漏报**。
- 主指标为 0/1 布尔，日志刷屏不会造成告警风暴，**不多报**。
- 只读日志、只写自有状态目录，`tail -c` 增量读取 + gawk 单遍流式匹配，**对服务器性能影响可忽略**（实测 10 万行约 0.06s）。

## 目录结构

```
kernel-hw-mon/
├── kernel-hw-mon.sh          # 核心脚本（唯一需要部署到服务器）
├── patterns.conf             # 匹配规则
├── README.md
├── tests/smoke-test.sh       # 功能自测
└── deploy/
    ├── categraf-exec-prometheus.toml   # 方式 A: Categraf exec + prometheus
    ├── categraf-exec-influx.toml       # 方式 B: Categraf exec + influx
    ├── systemd/kernel-hw-mon.{service,timer}  # 方式 C: node_exporter textfile
    ├── rsyslog/49-kernel-hw.conf       # 权限方案: 专用日志文件
    ├── logrotate/kernel-hw-mon
    ├── sudoers/kernel-hw-mon           # 备选权限方案
    └── n9e-alert-rules.md
```

## 指标

| 指标 | 含义 |
| --- | --- |
| `kernel_hardware_error` | 本窗口发现内核/硬件错误 = 1，否则 0（主指标） |
| `kernel_hardware_error_type{type=...}` | 按硬件类型 0/1：`nic/mem/raid/cpu/bus/kernel/thermal`（类别自动取自 `patterns.conf` include 行） |
| `kernel_hw_mon_failed` | 脚本执行失败（日志不可读、规则缺失、gawk 异常）= 1 |
| `kernel_hw_mon_last_run_timestamp` | 上次运行 Unix 时间戳（用于发现监控停摆） |

> `kernel_hardware_error_type` 供联动系统按类型精确查询 Elasticsearch（见 `docs/superpowers/specs/2026-09-13-kylin-hw-alert-es-llm-linkage-design.md`）。

## 部署

### 0. 安装脚本与规则

```bash
install -d -m 0755 /opt/kernel-hw-mon /etc/kernel-hw-mon
install -m 0755 kernel-hw-mon.sh /opt/kernel-hw-mon/kernel-hw-mon.sh
install -m 0644 patterns.conf    /etc/kernel-hw-mon/patterns.conf
```

### 1. 选择权限方案（关键）

`/var/log/messages` 默认 `600 root:root`，普通用户不可读。三选一：

**方案 A（推荐，无需 sudo）：rsyslog 额外输出专用文件**

```bash
install -m 0644 deploy/rsyslog/49-kernel-hw.conf /etc/rsyslog.d/49-kernel-hw.conf
groupadd -f categraf
systemctl restart rsyslog
ls -l /var/log/kernel-hw-errors.log     # 应为 root:categraf 640
install -m 0644 deploy/logrotate/kernel-hw-mon /etc/logrotate.d/kernel-hw-mon
```

之后脚本使用 `--log-file /var/log/kernel-hw-errors.log`。

**方案 B：sudoers 最小授权**（保留读取 `/var/log/messages`）

```bash
# 只放行固定参数 wrapper，避免把任意参数权限交给 categraf（安全）
install -m 0755 deploy/sudoers/kernel-hw-mon-wrapper.sh /etc/kernel-hw-mon/kernel-hw-mon-wrapper.sh
install -m 0440 deploy/sudoers/kernel-hw-mon /etc/sudoers.d/kernel-hw-mon
# exec 命令改为: sudo /etc/kernel-hw-mon/kernel-hw-mon-wrapper.sh
```

**方案 C：加入 adm 组**（取决于发行版是否将 messages 设为 `640 root:adm`）

```bash
usermod -aG adm categraf && systemctl restart categraf
```

### 2. 初始化状态目录

```bash
install -d -o categraf -g categraf -m 0755 /var/lib/kernel-hw-mon
```

### 3. 选择接入方式

**方式 A：Categraf exec 插件（推荐）**

```bash
cp deploy/categraf-exec-prometheus.toml /path/to/categraf/conf/input.exec/kernel-hw-mon.toml
systemctl restart categraf
```

> 说明：当前 Categraf 的插件名为 `exec`；部分旧版本/发行包中称为 `script`，配置项一致（`commands` + `data_format`）。
> 若版本不支持实例级 `interval`，删除该行并将全局 `interval` 设为 `60`。

**方式 B：node_exporter textfile**

```bash
install -m 0644 deploy/systemd/kernel-hw-mon.service /etc/systemd/system/
install -m 0644 deploy/systemd/kernel-hw-mon.timer   /etc/systemd/system/
# node_exporter 需带 --collector.textfile.directory=/var/lib/node_exporter/textfile_collector
install -d -o categraf -g categraf -m 0750 /var/lib/node_exporter/textfile_collector
systemctl daemon-reload && systemctl enable --now kernel-hw-mon.timer
```

## 参数

```
-f, --format <fmt>        prometheus|influx|falcon|textfile (默认 prometheus)
-l, --log-file <path>     日志文件 (默认 /var/log/messages)
-s, --state-dir <dir>     状态目录 (默认 /var/lib/kernel-hw-mon)
-p, --patterns <file>     规则文件 (默认 /etc/kernel-hw-mon/patterns.conf)
-t, --textfile-dir <dir>  textfile 输出目录
-m, --metric <name>       主指标名 (默认 kernel_hardware_error)
-j, --journal             改用 journalctl -k（无 rsyslog 的精简系统）
--scan                    全量扫描日志，逐行输出 "<时间>\t<类型>\t<关键信息>"
                          （供 on-demand 排查/ansible 报表；失败走 stderr 且非 0 退出）
--scan-max <n>            --scan 模式最多输出行数（默认不限）
-d, --debug               调试日志输出到 stderr（仅手工排查时使用）
```

## 规则文件格式（TAB 分隔）

```
source<TAB><regex>                 # 来源过滤，多个 OR；不配置则不限制
include<TAB><category><TAB><regex> # 命中即异常
exclude<TAB><regex>                # 命中则忽略
```

- `source` 用于只信任 `kernel:`、`smartd:`、`mcelog:` 等来源，避免误匹配应用日志里的 “I/O error”。
- 新增/调整规则后无需重启 Categraf，脚本每次运行都会重新读取。
- 正则使用 ERE（gawk）。

## n9e 告警规则

见 `deploy/n9e-alert-rules.md`，核心（0/1 指标需用 `max_over_time` 避免单窗口对齐漏报）：

- `max_over_time(kernel_hardware_error[2m]) > 0` 持续 0m → P1
- `max_over_time(kernel_hardware_error_type[2m]) > 0` 持续 0m → P1（联动触发）
- `kernel_hw_mon_failed > 0` 持续 3m → P2
- `time() - kernel_hw_mon_last_run_timestamp > 300` 持续 1m → P2

## 验证

```bash
# 功能自测（任意 Linux，无需 root）
bash tests/smoke-test.sh        # 16 项：首跑/不重报/来源过滤/半行/轮转/截断/规则缺失/多格式

# 全维度验证（可用性/有效性/准确性/安全性，需 root + RHEL8 系容器）
docker run --rm -v "$PWD":/work:ro docker.m.daocloud.io/library/rockylinux:8 \
  bash -c 'dnf -y install gawk util-linux rsyslog procps-ng findutils >/dev/null 2>&1; \
           useradd -r -M categraf; bash /work/tests/verify.sh /work'
```

已在多种系统镜像上实测通过（verify.sh 全维度断言）：

| 系统 | 与 Kylin V10 关系 | 结果 |
| --- | --- | --- |
| Rocky Linux 8 | RHEL8 系，与 Kylin V10 同源（首选替代） | 47/47 通过 |
| Rocky Linux 9 | RHEL9 系 | 46/46 通过（1 项工具缺失跳过） |
| AlmaLinux 9 | RHEL9 系 | 47/47 通过 |
| CentOS 7 | RHEL7 系，老系统覆盖 | 44/44 通过（2 项工具缺失跳过） |
| Ubuntu 22.04 | Debian 系 | 46/46 通过（1 项工具缺失跳过） |
| Ubuntu 24.04 | Debian 系 | 46/46 通过（1 项工具缺失跳过） |
| Debian 12 | Debian 系 | 46/46 通过（1 项工具缺失跳过） |

覆盖场景：非 root 运行、状态目录/日志/规则权限异常、journal 回退、并发锁、stderr 为空（exec 兼容）、10 类硬件错误命中、5 类正常日志排除、不重报、刷屏风暴、半行、轮转、截断、首跑不重放、命令注入、正则特殊字符、状态目录不外写、rsyslog 端到端、logrotate 配置。

```bash
# 手工注入一条内核错误
logger -t kernel -p kern.err "eth0: NIC Link is Down"
/opt/kernel-hw-mon/kernel-hw-mon.sh -f prometheus --log-file /var/log/kernel-hw-errors.log
# 期望: kernel_hardware_error 1；紧接着再跑一次应为 0
```

## 运维注意与已知限制

1. **首次运行从 EOF 开始**：部署前 1 分钟内的错误不会补报，避免重放历史日志造成告警风暴。
2. **状态目录须由运行用户独占**：不要 root 与监控用户混用（root 先跑生成 600 文件后，普通用户会读不到）。统一由部署用户创建并 `chown`。
2. **日志轮转**：脚本通过 inode 变化识别 `create` 轮转并自动重置游标；`copytruncate` 存在竞态（截断后若立刻写入超过旧 offset 的内容可能漏读），建议 `/var/log/messages` 使用默认 `create` 轮转。
3. **状态文件损坏**：自动重建并从 EOF 开始。
4. **脚本异常**：不会推进游标，下次重试；同时 `kernel_hw_mon_failed=1` 上报。
5. **debug 模式**会向 stderr 写日志，Categraf exec 会因此丢弃结果，仅用于手工排查。
6. 监控自身健康（`kernel_hw_mon_failed`、`last_run_timestamp`）务必配置告警，防止静默失效导致漏报。
