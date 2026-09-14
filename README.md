# Kylin 硬件日志监控与智能诊断

从系统日志增量发现内核/硬件异常（网卡/内存/磁盘/RAID 卡/CPU/PCIe/温度），接入 n9e 告警，
并在告警触发后自动从 Elasticsearch 检索原始日志、由私有化大模型给出诊断，最后回写企业微信/邮件/工单。

## 组件

```
kernel-hw-mon/    采集端（shell）：增量检测 /var/log/messages → 指标（总 + 按类型 0/1）
                     · --scan 全量扫描模式（供 ansible 报表/on-demand 排查）
linkage/          联动服务（Go）：webhook → 去重/合并 → ES 精确查询 → LLM 分析 → 三路回写
                     · 可选：告警直接触发 agent（skills/hw-alert-analyzer）
skills/           agent skill：hw-alert-analyzer（检索+分析一体化，供 agent 直接调用）
ansible/          部署：采集端 + 联动服务一键部署（roles + playbooks）
tools/            一致性校验等脚本
docs/superpowers/specs/  两份设计文档
```

## 数据流

```
/var/log/messages ──► kernel-hw-mon ──► n9e (kernel_hardware_error_type>0)
                                        │ webhook
                                        ▼
                               linkage (Go) ──► ES syslog-system（按类型精确检索）
                                        │         │
                                        │         ▼
                                        └──► LLM 分析（prompts/analyze.txt 为单一分析规则）
                                        └──► 企业微信 / 邮件 / UniPro 工单
                                        └──► (可选) 直触 agent 加载 hw-alert-analyzer skill
```

## 快速上手

```bash
# 1. 采集端验证
docker run --rm -v "$PWD/kernel-hw-mon":/work:ro rockylinux:8 \
  bash /work/tests/smoke-test.sh /work/kernel-hw-mon.sh /work/patterns.conf

# 2. 联动服务
cd linkage && go test ./... && go build -o kernel-hw-linkage ./cmd/linkage

# 3. 部署（见 ansible/README.md）
# 4. 按类型一致性校验（接入 CI）
bash tools/check-type-alignment.sh kernel-hw-mon/patterns.conf linkage/config.example.json
```

## 关键约定

- **单一事实源**：
  - 硬件类型/告警分类：`kernel-hw-mon/patterns.conf` 的 include 类别
  - ES 检索关键词：`linkage/config.example.json`（采集端与联动端通过 `tools/check-type-alignment.sh` 对齐）
  - LLM 分析规则：`linkage/prompts/analyze.txt`（联动服务与 agent skill 共用）
- 类型划分：nic / mem / raid（磁盘+RAID 卡）/ cpu / bus / kernel / thermal
- 指标均为 0/1 布尔；n9e 规则使用 `max_over_time(...[2m]) > 0` 避免单窗口对齐漏报
