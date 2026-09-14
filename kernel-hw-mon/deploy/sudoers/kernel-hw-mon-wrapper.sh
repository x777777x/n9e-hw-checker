#!/usr/bin/env bash
#
# kernel-hw-mon-wrapper.sh - sudo 专用包装脚本。
#
# 为什么需要 wrapper：sudoers 无法约束命令参数个数/内容；
# 若直接放行 /opt/kernel-hw-mon/kernel-hw-mon.sh，categraf 可携带任意参数
# （如 -s/-t/-m）以 root 读写任意路径（提权口子）。
# 因此只把本固定参数 wrapper 加入 sudoers，categraf 只能以 root 执行这一种安全调用。
#
# 注意：真实脚本路径本身不得出现在 sudoers 中。
set -e
exec /opt/kernel-hw-mon/kernel-hw-mon.sh \
    --format prometheus \
    --log-file /var/log/messages \
    --patterns /etc/kernel-hw-mon/patterns.conf \
    --state-dir /var/lib/kernel-hw-mon
