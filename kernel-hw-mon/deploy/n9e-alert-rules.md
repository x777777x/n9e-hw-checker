# n9e 告警规则

指标由 Categraf exec（或 node_exporter textfile）上报，0/1 布尔量。

## 1. 内核/硬件错误（主告警，P1）

- 表达式：`max_over_time(kernel_hardware_error[2m]) > 0`
- 持续时长：`0m`
- 恢复：`max_over_time(kernel_hardware_error[5m]) == 0`
- 说明：`服务器 {{$labels.ident}} 检测到内核/硬件错误，请到 Kibana/日志中查询关键字（网卡/内存/RAID/PCIe/温度等）`

> **为什么用 `max_over_time` + 0m**：`kernel_hardware_error` 是 0/1 且单次错误只保持 1 个采集窗口（60s）。
> 直接 `> 0` + `for 1m` 在评估周期对齐不良时可能静默不触发（漏报）。
> `max_over_time(…[2m]) > 0` 保证窗口内任意一个采样为 1 即告警；2m 覆盖相邻两次采集。
> 因指标是 0/1，刷屏只会保持为 1，不会产生告警风暴。

## 1b. 按类型联动告警（P1，供联动系统触发，回调携带 type 标签）

- 表达式：`max_over_time(kernel_hardware_error_type[2m]) > 0`
- 持续时长：`0m`
- 恢复：`max_over_time(kernel_hardware_error_type[5m]) == 0`
- 说明：每条告警事件对应一个 `type`（nic/mem/raid/...），联动服务据此精确查询 ES 并分析。
  可配置为「通知 → 联动服务 webhook」，事件本身无需对值班可见（值班看规则 1）。

## 2. 监控执行失败（P2）

- 表达式：`kernel_hw_mon_failed > 0`
- 持续时长：`3m`
- 说明：`{{$labels.ident}} 内核日志监控执行失败（日志不可读/规则缺失/gawk 异常），请检查 /var/lib/kernel-hw-mon/monitor.log`

## 3. 监控停摆（P2）

- 表达式：`time() - kernel_hw_mon_last_run_timestamp > 300`
- 持续时长：`1m`
- 说明：`{{$labels.ident}} 内核日志监控超过 5 分钟未成功运行`

> **nodata 提醒**：若 Categraf 整体宕机，`kernel_hw_mon_last_run_timestamp` 序列会消失，
> 上式变成无数据而不触发。建议为 rule 3 开启"无数据即告警"（n9e 告警规则的 nodata 选项），
> 或另配一条 `up == 0`/采集端进程存活类规则兜底，避免"监控也挂了却没人知道"。

> 规则 2、3 保证“监控自身挂了”也能被发现，避免静默失效导致漏报。

## 建议的通知/抑制

- 同一台机器上规则 1 触发时，规则 2、3 可配置静默，减少重复通知。
- 规则 1 建议直连值班（P1），规则 2、3 走监控运维群（P2）。
