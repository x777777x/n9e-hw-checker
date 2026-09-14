# Ansible 部署方案

一键部署「采集端 kernel-hw-mon + 硬件告警联动服务」，对应设计文档 `docs/superpowers/specs/2026-09-13-kylin-hw-alert-es-llm-linkage-design.md` 第 6.2 节。

## 前置

- 控制机已安装 ansible-core（仅用 `ansible.builtin`，无需额外 collection）。
- 目标主机可 SSH（配置 `inventory/hosts.yml`）。
- **联动服务二进制需先本地构建**并放到 `../linkage/kernel-hw-linkage`：

```bash
cd linkage && go build -o kernel-hw-linkage ./cmd/linkage
```

## 目录

```
ansible/
├── ansible.cfg
├── inventory/
│   ├── hosts.yml                 # [linkage] 联动服务; [monitored] 受监控服务器
│   └── group_vars/               # all/linkage/secrets（inventory 同目录，必须放这里才会被加载）
├── playbooks/
│   ├── all.yml                 # 全局参数、仓库根路径
│   ├── linkage.yml             # ES/LLM/窗口/通知/ident→IP 清单
│   └── secrets.yml.example     # 密钥模板（Vault 加密）
├── playbooks/
│   ├── deploy-linkage.yml      # 联动服务
│   ├── deploy-monitor.yml      # 采集端
│   └── site.yml                # 一键全量
└── roles/
    ├── linkage/                # 二进制+配置+systemd+healthz 自检
    └── hw-mon/                 # 脚本/patterns/rsyslog/logrotate/categraf
```

## 使用

```bash
# 1) 配置密钥并加密
cp inventory/group_vars/secrets.yml.example inventory/group_vars/secrets.yml
# 编辑 secrets.yml 填入实际密钥后：
ansible-vault encrypt inventory/group_vars/secrets.yml

# 2) 语法检查
ansible-playbook -i inventory/hosts.yml playbooks/site.yml --syntax-check --ask-vault-pass

# 3) 执行（采集端 + 联动服务）
ansible-playbook -i inventory/hosts.yml playbooks/site.yml --ask-vault-pass

# 只部署联动服务或采集端
ansible-playbook -i inventory/hosts.yml playbooks/deploy-linkage.yml --ask-vault-pass
ansible-playbook -i inventory/hosts.yml playbooks/deploy-monitor.yml
```

## 关键设计

- **密钥**：`secrets.yml` 走 Ansible Vault；渲染出的 `config.json` 权限 0600。
- **二进制版本化**：当前按固定名 `kernel-hw-linkage` 拷贝；需要回滚时用旧版本构建产物重跑 `deploy-linkage.yml` 即可（文件变化触发 handler 重启）。
- **自检**：部署后等待 `/healthz` 通过（`deploy-linkage.yml` 内 uri 探测）。
- **systemd 加固**：`NoNewPrivileges`、`PrivateTmp`、`ProtectSystem=full`、`ProtectHome`。
- **采集端角色**：自动装 rsyslog 专用日志规则（`/var/log/kernel-hw-errors.log`，`640 root:categraf`）+ logrotate + 可选 Categraf exec 配置（设置 `hw_mon_categraf_conf_dir` 生效）。

## 回滚

- 联动服务：重新构建旧版本二进制后重跑 `deploy-linkage.yml`（文件变更触发 handler 重启）。
- 采集端：重跑 `deploy-monitor.yml` 还原脚本/规则；`patterns.conf` 改动即新规则，无需回滚。

## 部署后验证

```bash
# 在受监控服务器注入一条错误，确认检测与联动链路
logger -t kernel -p kern.err "eth0: NIC Link is Down"
# 联动服务日志应出现 analysis done；通知通道收到报告
```

## 按需检索硬件问题日志（报表）

`playbooks/retrieve-hardware-logs.yml` 在清单中每台主机上扫描系统日志，
按 **主机 | 时间 | 异常类型 | 关键信息** 逐行输出，并汇总到 `<ansible>/reports/hardware-issues-report.txt`：

```bash
ansible-playbook -i inventory/hosts.yml playbooks/retrieve-hardware-logs.yml
# 限制每台输出行数
ansible-playbook -i inventory/hosts.yml playbooks/retrieve-hardware-logs.yml -e scan_since_max=200
```

- 日志来源：优先 `/var/log/kernel-hw-errors.log`，回退 `/var/log/messages`（需 root 读，playbook 已 `become: true`）。
- 复用采集端 `--scan` 模式与 `patterns.conf`（单一事实源），异常类型与告警分类一致。
- 无异常的主机不产生报告行；汇总文件仅合并有发现的机器。
