#!/usr/bin/env bash
#
# verify.sh - kernel-hw-mon 全维度验证（可用性/有效性/准确性/安全性）
#
# 设计为在 Linux 容器中以 root 运行（建议 RHEL8 系镜像，如 rockylinux:8，
# 与 Kylin V10 同源）。用法:
#   docker run --rm -v <repo>/kernel-hw-mon:/work:ro rockylinux:8 \
#       bash -c 'dnf -y install gawk util-linux rsyslog procps-ng findutils >/dev/null
#                useradd -r -M categraf; bash /work/tests/verify.sh /work'
#
# 说明: 状态目录须由运行用户独占。脚本会在每个用例前重建状态目录，
#       模拟“单一运行用户”的真实部署（避免 root 与监控用户混用）。
set -u

SRC="${1:-/work}"
SCRIPT=/opt/kernel-hw-mon/kernel-hw-mon.sh
PATTERNS=/opt/kernel-hw-mon/patterns.conf

PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); printf '  ok   %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$1"; }
skip() { SKIP=$((SKIP+1)); printf '  skip %s\n' "$1"; }
assert_cmp() { if [ "$2" = "$3" ]; then ok "$1 (=$2)"; else bad "$1 (got=$2 want=$3)"; fi; }
metric() { printf '%s\n' "$1" | awk -v m="$2" '$1==m{print $2; exit}'; }

# 安装
install -d -m 0755 /opt/kernel-hw-mon
install -m 0755 "$SRC/kernel-hw-mon.sh" /opt/kernel-hw-mon/kernel-hw-mon.sh
install -m 0644 "$SRC/patterns.conf" /opt/kernel-hw-mon/patterns.conf
[ -f "$SCRIPT" ] || { echo "FATAL: script not installed"; exit 1; }

V=$(mktemp -d)
chmod 755 "$V"   # 允许非 root 用户（categraf）遍历
LOG=$V/messages
STATE=$V/state
: > "$LOG"
mkdir -p "$STATE"

run() { "$SCRIPT" -l "$LOG" -s "$STATE" -p "$PATTERNS" -f prometheus "$@"; }

echo
echo "############ 1. 可用性 (Availability) ############"

echo "-- 1.1 root 运行，首次初始化 --"
out=$(run)
assert_cmp "exit0+首跑 found=0" "$(metric "$out" kernel_hardware_error)" "0"
assert_cmp "首跑 failed=0"     "$(metric "$out" kernel_hw_mon_failed)" "0"

echo "-- 1.2 普通用户 categraf 运行 --"
id categraf >/dev/null 2>&1 || useradd -r -M categraf
rm -rf "$STATE"; mkdir -p "$STATE"; chown -R categraf:categraf "$STATE"
chmod 644 "$LOG"
out=$(su -s /bin/bash categraf -c "$SCRIPT -l $LOG -s $STATE -p $PATTERNS -f prometheus")
assert_cmp "categraf 可读日志+可写状态" "$(metric "$out" kernel_hw_mon_failed)" "0"

echo "-- 1.3 状态目录不可写 -> failed=1 --"
rm -rf "$STATE"; mkdir -p "$STATE"; chmod 500 "$STATE"; chown root:root "$STATE"
out=$(su -s /bin/bash categraf -c "$SCRIPT -l $LOG -s $STATE -p $PATTERNS -f prometheus")
assert_cmp "状态目录不可写 failed=1" "$(metric "$out" kernel_hw_mon_failed)" "1"
rm -rf "$STATE"; mkdir -p "$STATE"; chown -R categraf:categraf "$STATE"

echo "-- 1.4 日志不可读 -> 走 journal 回退（成功=0 或 失败=1 均属正常）--"
chmod 600 "$LOG"; chown root:root "$LOG"
out=$(su -s /bin/bash categraf -c "$SCRIPT -l $LOG -s $STATE -p $PATTERNS -f prometheus")
f=$(metric "$out" kernel_hw_mon_failed)
case "$f" in
  0) ok "日志不可读：journal 回退成功 (failed=0)" ;;
  1) ok "日志不可读：journal 回退不可用 (failed=1)" ;;
  *) bad "日志不可读：异常输出 failed=$f" ;;
esac
chmod 644 "$LOG"; chown root:root "$LOG"

echo "-- 1.5 规则文件缺失 -> failed=1 --"
out=$("$SCRIPT" -l "$LOG" -s "$STATE" -p "$V/nope" -f prometheus)
assert_cmp "规则缺失 failed=1" "$(metric "$out" kernel_hw_mon_failed)" "1"

echo "-- 1.6 并发锁：占用锁时第二次调用快速跳过，不损坏状态 --"
printf 'Sep 13 12:00:00 h kernel: eth0: Link is Down\n' >> "$LOG"
"$SCRIPT" -l "$LOG" -s "$STATE" -p "$PATTERNS" >/dev/null
BEFORE=$(cat "$STATE/state")
flock "$STATE/.lock" sleep 5 &
FL=$!
sleep 1
S=$(date +%s%N)
out=$(timeout 3 "$SCRIPT" -l "$LOG" -s "$STATE" -p "$PATTERNS" -f prometheus); RC=$?
E=$(date +%s%N)
wait $FL 2>/dev/null
AFTER=$(cat "$STATE/state")
[ "$RC" -eq 0 ] && ok "并发跳过退出码0" || bad "并发跳过退出码 $RC"
[ $(( (E-S)/1000000 )) -lt 3000 ] && ok "并发跳过耗时 <3s" || bad "并发跳过耗时 $(( (E-S)/1000000 ))ms"
[ "$BEFORE" = "$AFTER" ] && ok "并发期间状态未被破坏" || bad "状态被并发破坏: $BEFORE -> $AFTER"

echo "-- 1.7 Categraf exec 兼容：正常路径 stderr 恒为空、退出码 0 --"
out=$("$SCRIPT" -l "$LOG" -s "$STATE" -p "$PATTERNS" -f prometheus 2>"$V/err"); RC=$?
[ $RC -eq 0 ] && ok "退出码 0" || bad "退出码 $RC"
[ -s "$V/err" ] && bad "stderr 非空($(wc -c <"$V/err")B)" || ok "stderr 为空"

echo
echo "############ 2. 有效性 (Effectiveness) ############"

inj() { # desc line want
    printf '%s\n' "$2" >> "$LOG"
    local v
    v=$(run | awk '/^kernel_hardware_error /{print $2; exit}')
    assert_cmp "$1" "$v" "$3"
}

inj "网卡 Link is Down"       "Sep 13 12:01:00 h kernel: eth0: NIC Link is Down" "1"
inj "Mellanox Port Down"      "Sep 13 12:02:00 h kernel: mlx5_core 0000:03:00.0: Port 1 Link Down" "1"
inj "EDAC CE 内存错误"        "Sep 13 12:03:00 h kernel: EDAC MC0: 1 CE memory read error" "1"
inj "mce 硬件错误"            "Sep 13 12:04:00 h kernel: mce: [Hardware Error]: Machine check events logged" "1"
inj "RAID I/O error"          "Sep 13 12:05:00 h kernel: megaraid_sas 0000:03:00.0: I/O error, dev sdb" "1"
inj "PCIe AER"                "Sep 13 12:06:00 h kernel: pcieport 0000:00:1c.0: AER: Corrected error received" "1"
inj "内核 oops"               "Sep 13 12:07:00 h kernel: BUG: unable to handle kernel paging request at ffff8880" "1"
inj "CPU 温度过高"            "Sep 13 12:08:00 h kernel: CPU0: Package temperature above threshold" "1"
inj "smartd SMART 失败"       "Sep 13 12:09:00 h smartd[1234]: Device: /dev/sda, SMART Failure: ATTRIBUTE 5" "1"
inj "mcelog 上报"             "Sep 13 12:10:00 h mcelog: Hardware Error: CPU 0: Machine Check" "1"

echo
echo "############ 3. 准确性 (Accuracy) ############"

echo "-- 3.1 误报抑制 --"
inj "正常 Link is Up 排除"    "Sep 13 12:20:00 h kernel: eth0: Link is Up 1000Mbps full-duplex" "0"
inj "应用日志 I/O error 来源过滤" "Sep 13 12:21:00 h myapp[100]: I/O error reading /data/x" "0"
inj "普通内核信息"            "Sep 13 12:22:00 h kernel: EXT4-fs (sda1): mounted filesystem" "0"
inj "sshd 登录"               "Sep 13 12:23:00 h sshd[999]: Accepted publickey for root" "0"
inj "audit 审计"              "Sep 13 12:24:00 h audit[123]: SYSCALL arch=c000003e" "0"

echo "-- 3.2 同一错误不重报 --"
out=$(run); assert_cmp "再次运行 found=0" "$(metric "$out" kernel_hardware_error)" "0"

echo "-- 3.3 刷屏风暴：100 条同错误只输出 1 --"
for i in $(seq 1 100); do printf 'Sep 13 12:30:00 h kernel: eth0: Link is Down\n' >> "$LOG"; done
out=$(run); assert_cmp "刷屏 found=1" "$(metric "$out" kernel_hardware_error)" "1"
out=$(run); assert_cmp "刷屏后不再报" "$(metric "$out" kernel_hardware_error)" "0"

echo "-- 3.4 半行：不消费、补齐后捕获 --"
printf 'Sep 13 12:40:00 h kernel: eth1: Link is Down' >> "$LOG"
out=$(run); assert_cmp "半行不消费" "$(metric "$out" kernel_hardware_error)" "0"
printf '\n' >> "$LOG"
out=$(run); assert_cmp "半行补齐后捕获" "$(metric "$out" kernel_hardware_error)" "1"

echo "-- 3.5 日志轮转（inode 变化）--"
mv "$LOG" "$LOG.1"; : > "$LOG"
inj "轮转后新错误" "Sep 13 12:50:00 h kernel: EDAC MC2: 1 UE Uncorrected Error" "1"

echo "-- 3.6 截断（offset>size）自动归零 --"
INO=$(stat -Lc '%i' "$LOG")
printf '%s 999999 0\n' "$INO" > "$STATE/state"
inj "截断后错误" "Sep 13 12:51:00 h kernel: megaraid_sas: I/O error, dev sdc" "1"

echo "-- 3.7 首跑不重放历史 --"
LOG2=$V/messages2; ST2=$V/state2; mkdir -p "$ST2"
printf 'Sep 13 08:00:00 h kernel: eth0: Link is Down\n' > "$LOG2"
out=$("$SCRIPT" -l "$LOG2" -s "$ST2" -p "$PATTERNS" -f prometheus)
assert_cmp "首跑历史不触发" "$(metric "$out" kernel_hardware_error)" "0"

echo
echo "############ 4. 安全性 (Security) ############"

echo "-- 4.1 日志内容命令注入 --"
rm -f /tmp/PWNED /tmp/PWNED2 /tmp/PWNED3
printf 'Sep 13 13:00:00 h kernel: eth0: Link is Down; touch /tmp/PWNED\n' >> "$LOG"
printf 'Sep 13 13:01:00 h kernel: $(touch /tmp/PWNED2) `touch /tmp/PWNED3`\n' >> "$LOG"
out=$(run)
assert_cmp "注入行 found=1" "$(metric "$out" kernel_hardware_error)" "1"
[ ! -e /tmp/PWNED ]  && ok "无分号注入执行"  || bad "分号注入被执行!"
[ ! -e /tmp/PWNED2 ] && ok "无 $() 注入执行" || bad "\$() 注入被执行!"
[ ! -e /tmp/PWNED3 ] && ok "无反引号注入执行" || bad "反引号注入被执行!"

echo "-- 4.2 含正则特殊字符的日志行不崩溃 --"
printf 'Sep 13 13:02:00 h kernel: ( [ ] * . + ? { } | ) Link is Down\n' >> "$LOG"
out=$(run)
assert_cmp "特殊字符不崩溃 found=1" "$(metric "$out" kernel_hardware_error)" "1"

echo "-- 4.3 普通用户只写状态目录，不外写 --"
rm -rf "$STATE"; mkdir -p "$STATE"; chown -R categraf:categraf "$STATE"
chmod 644 "$LOG"
su -s /bin/bash categraf -c "$SCRIPT -l $LOG -s $STATE -p $PATTERNS -f prometheus" >/dev/null 2>&1
if [ -f "$STATE/state" ] && [ -f "$STATE/monitor.log" ] && [ -f "$STATE/.lock" ] \
   && [ "$(find "$STATE" -maxdepth 1 -type f | wc -l)" -eq 3 ]; then
    ok "状态目录仅 3 个预期文件"
else
    bad "状态目录文件异常: $(find "$STATE" -maxdepth 1 -type f | sort | tr '\n' ' ')"
fi
if [ "$(stat -c '%a' "$STATE/state" 2>/dev/null)" = "600" ]; then
    ok "状态文件权限 600"
else
    bad "状态文件权限 $(stat -c '%a' "$STATE/state" 2>/dev/null)"
fi

echo "-- 4.4 脚本与规则权限 --"
[ "$(stat -c '%a' "$SCRIPT")" = "755" ] && ok "脚本 755" || bad "脚本权限 $(stat -c '%a' "$SCRIPT")"
[ "$(stat -c '%a' "$PATTERNS")" = "644" ] && ok "规则 644" || bad "规则权限 $(stat -c '%a' "$PATTERNS")"

echo "-- 4.5 不依赖 SUID/root 能力 --"
if command -v getcap >/dev/null 2>&1; then
    if [ -z "$(getcap "$SCRIPT" 2>/dev/null)" ]; then ok "无特殊 capability"; else bad "脚本带 capability"; fi
else
    skip "无 getcap 工具"
fi

echo
echo "############ 5. rsyslog 推荐部署端到端（Kylin/RHEL 场景） ############"
if command -v rsyslogd >/dev/null 2>&1; then
    RSLOG=$V/kernel-hw-errors.log
    RSSOCK=$V/rsyslog.sock
    RSCONF=$V/rsyslog.conf
    cat > "$RSCONF" <<EOF
global(workDirectory="$V")
module(load="imuxsock" SysSock.Use="on" SysSock.Name="$RSSOCK")
if ((\$syslogfacility-text == 'kern') or (\$programname == 'kernel')) then {
    action(type="omfile" file="$RSLOG" fileOwner="root" fileGroup="categraf" fileCreateMode="0640")
}
EOF
    # 预创建日志文件（owner/mode 与 rsyslog 规则一致），保证游标初始化可读
    install -o root -g categraf -m 0640 /dev/null "$RSLOG" 2>/dev/null || : > "$RSLOG"
    rsyslogd -n -f "$RSCONF" -i "$V/rsyslog.pid" 2>"$V/rsyslog.err" &
    RS_PID=$!
    sleep 2
    if [ -S "$RSSOCK" ]; then
        rm -rf "$STATE"; mkdir -p "$STATE"; chown -R categraf:categraf "$STATE"
        # 先初始化游标（首跑从 EOF 开始，不重放历史），再注入新错误
        su -s /bin/bash categraf -c "$SCRIPT -l $RSLOG -s $STATE -p $PATTERNS -f prometheus" >/dev/null 2>&1
        logger -u "$RSSOCK" -t kernel -p kern.err "eth0: NIC Link is Down (rsyslog e2e)"
        sleep 1
        if [ -f "$RSLOG" ]; then
            out=$(su -s /bin/bash categraf -c "$SCRIPT -l $RSLOG -s $STATE -p $PATTERNS -f prometheus")
            assert_cmp "rsyslog e2e 检测到" "$(metric "$out" kernel_hardware_error)" "1"
            assert_cmp "rsyslog 文件权限 640" "$(stat -c '%a' "$RSLOG")" "640"
        else
            bad "rsyslog 未生成日志文件"; cat "$V/rsyslog.err" 2>/dev/null
        fi
    else
        bad "rsyslog socket 未就绪"; cat "$V/rsyslog.err" 2>/dev/null
    fi
    kill "$RS_PID" 2>/dev/null
else
    skip "容器未安装 rsyslog"
fi

echo
echo "############ 6. logrotate 配置语法 ############"
if command -v logrotate >/dev/null 2>&1; then
    install -m 0644 "$SRC/deploy/logrotate/kernel-hw-mon" /etc/logrotate.d/kernel-hw-mon
    logrotate -d /etc/logrotate.d/kernel-hw-mon >/dev/null 2>"$V/lr.err" \
        && ok "logrotate 配置合法" || { bad "logrotate 配置非法"; cat "$V/lr.err"; }
else
    skip "无 logrotate"
fi

echo
echo "========== 结果: PASS=$PASS FAIL=$FAIL SKIP=$SKIP =========="
rm -rf "$V"
[ "$FAIL" -eq 0 ]
