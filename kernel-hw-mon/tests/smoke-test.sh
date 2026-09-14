#!/usr/bin/env bash
#
# smoke-test.sh - kernel-hw-mon 功能验证
# 用法: bash smoke-test.sh [脚本路径] [规则路径]
#
set -u

SCRIPT="${1:-$(cd "$(dirname "$0")/.." && pwd)/kernel-hw-mon.sh}"
PATTERNS="${2:-$(cd "$(dirname "$0")/.." && pwd)/patterns.conf}"

TMP=$(mktemp -d)
LOG="$TMP/messages"
STATE="$TMP/state"
mkdir -p "$STATE"

PASS=0
FAIL=0

run() { "$SCRIPT" -l "$LOG" -s "$STATE" -p "$PATTERNS" -f prometheus "$@"; }
metric() { printf '%s\n' "$1" | awk -v m="$2" '$1==m{print $2; exit}'; }
type_metric() {
    printf '%s\n' "$1" | awk -v t="$2" '
        $1 ~ "^kernel_hardware_error_type" && index($1, "type=\"" t "\"") {print $2; exit}'
}
assert_eq() {
    local desc=$1 got=$2 want=$3
    if [ "$got" = "$want" ]; then
        PASS=$((PASS + 1)); printf '  ok   %s (got=%s)\n' "$desc" "$got"
    else
        FAIL=$((FAIL + 1)); printf '  FAIL %s (got=%s want=%s)\n' "$desc" "$got" "$want"
    fi
}

: > "$LOG"

echo "== 1. 首次运行从 EOF 开始，不重放历史 =="
printf 'Sep 13 10:00:00 host kernel: eth0: Link is Down\n' > "$LOG"
out=$(run); assert_eq "首跑 found" "$(metric "$out" kernel_hardware_error)" "0"
assert_eq "首跑 failed" "$(metric "$out" kernel_hw_mon_failed)" "0"
assert_eq "状态文件 mode=file" "$(awk 'NR==1{print $1}' "$STATE/state" 2>/dev/null)" "file"

echo "== 2. 追加网卡错误 -> 1，type=nic 置位 =="
printf 'Sep 13 10:01:00 host kernel: eth0: NIC Link is Down\n' >> "$LOG"
out=$(run); assert_eq "nic found" "$(metric "$out" kernel_hardware_error)" "1"
assert_eq "type nic=1" "$(type_metric "$out" nic)" "1"
assert_eq "type mem=0" "$(type_metric "$out" mem)" "0"

echo "== 3. 立即再跑 -> 0 (不重报) =="
out=$(run); assert_eq "no re-report" "$(metric "$out" kernel_hardware_error)" "0"

echo "== 4. 正常 Link is Up -> 0 =="
printf 'Sep 13 10:02:00 host kernel: eth0: Link is Up 1000 Mbps\n' >> "$LOG"
out=$(run); assert_eq "exclude link up" "$(metric "$out" kernel_hardware_error)" "0"

echo "== 5. 应用日志 I/O error 被来源过滤 -> 0 =="
printf 'Sep 13 10:03:00 host myapp: I/O error on /data/foo\n' >> "$LOG"
out=$(run); assert_eq "source filter" "$(metric "$out" kernel_hardware_error)" "0"

echo "== 6. 内存 EDAC 错误 -> 1，type=mem 置位 =="
printf 'Sep 13 10:04:00 host kernel: EDAC MC0: 1 CE memory read error on CPU_SrcID#0\n' >> "$LOG"
out=$(run); assert_eq "edac found" "$(metric "$out" kernel_hardware_error)" "1"
assert_eq "type mem=1" "$(type_metric "$out" mem)" "1"
assert_eq "type nic=0" "$(type_metric "$out" nic)" "0"

echo "== 6b. 同窗口多类型并集 (nic+raid) =="
printf 'Sep 13 10:04:30 host kernel: eth1: NIC Link is Down\n' >> "$LOG"
printf 'Sep 13 10:04:31 host kernel: megaraid_sas: I/O error, dev sdb\n' >> "$LOG"
out=$(run); assert_eq "multi found" "$(metric "$out" kernel_hardware_error)" "1"
assert_eq "type nic=1" "$(type_metric "$out" nic)" "1"
assert_eq "type raid=1" "$(type_metric "$out" raid)" "1"

echo "== 7. RAID 错误 -> 1 =="
printf 'Sep 13 10:05:00 host kernel: megaraid_sas 0000:03:00.0: I/O error, dev sdb, sector 12345\n' >> "$LOG"
out=$(run); assert_eq "raid found" "$(metric "$out" kernel_hardware_error)" "1"

echo "== 7b. 磁盘/RAID 真实签名覆盖 =="
check_raid() { # desc line
  printf '%s\n' "$2" >> "$LOG"
  local v; v=$(run | awk '/^kernel_hardware_error /{print $2; exit}')
  assert_eq "$1" "$v" "1"
}
check_raid "SCSI FAILED Result"    "Sep 13 10:05:01 host kernel: sd 0:0:0:0: [sda] FAILED Result: hostbyte=DID_OK driverbyte=DRIVER_SENSE"
check_raid "MEDIUM ERROR"          "Sep 13 10:05:02 host kernel: sd 0:0:0:0: [sda] Add. Sense: Unrecovered read error - auto reallocate failed"
check_raid "ATA error UNC"         "Sep 13 10:05:03 host kernel: ata1.00: error: { UNC }"
check_raid "ATA DRDY ERR"          "Sep 13 10:05:04 host kernel: ata1.00: status: { DRDY ERR }"
check_raid "COMRESET failed"       "Sep 13 10:05:05 host kernel: ata1.00: COMRESET failed (errno=-16)"
check_raid "md 磁盘失效"            "Sep 13 10:05:06 host kernel: md/raid1:md0: Disk failure on sdb, disabling device."
check_raid "md recovery FAILED"    "Sep 13 10:05:07 host mdadm: Recovery FAILED on /dev/md0"
check_raid "控制器 Link down"       "Sep 13 10:05:08 host kernel: megaraid_sas 0000:03:00.0: Link down on the controller"
check_raid "控制器 reset failed"    "Sep 13 10:05:09 host kernel: megaraid_sas: ERROR - ctrl reset failed"
check_raid "NVMe 掉盘"              "Sep 13 10:05:10 host kernel: nvme nvme0: I/O 16 QID 1 admin, status: 0x2"
check_raid "NVMe 控制器 down"       "Sep 13 10:05:11 host kernel: nvme nvme0: controller is down; will reset"
check_raid "hpsa 超时"              "Sep 13 10:05:12 host kernel: hpsa 0000:01:00.0: CISS_CMD_STATUS_TIMEOUT"
check_raid "SMART 坏块"             "Sep 13 10:05:13 host smartd[1000]: Device: /dev/sda, 8 Currently unreadable (pending) sectors"

echo "== 7c. RAID 正常事件不应触发 =="
printf 'Sep 13 10:05:14 host mdadm: RebuildStarted /dev/md0\n' >> "$LOG"
out=$(run); assert_eq "md rebuild 正常" "$(metric "$out" kernel_hardware_error)" "0"

echo "== 8. 半行不消费，补齐后可捕获 =="
printf 'Sep 13 10:06:00 host kernel: eth1: Link is Down' >> "$LOG"
out=$(run); assert_eq "partial not consumed" "$(metric "$out" kernel_hardware_error)" "0"
printf '\n' >> "$LOG"
out=$(run); assert_eq "partial completed" "$(metric "$out" kernel_hardware_error)" "1"

echo "== 9. 日志轮转 (inode 变化) 从新文件读取 =="
mv "$LOG" "$LOG.1"; : > "$LOG"
printf 'Sep 13 10:07:00 host kernel: Kernel panic - not syncing\n' >> "$LOG"
out=$(run); assert_eq "rotated found" "$(metric "$out" kernel_hardware_error)" "1"

echo "== 10. 截断 (offset > size) 自动归零 =="
INO=$(stat -Lc '%i' "$LOG")
printf '%s 999999 0\n' "$INO" > "$STATE/state"
printf 'Sep 13 10:08:00 host kernel: EDAC MC1: 1 UE memory read error\n' > "$LOG"
out=$(run); assert_eq "truncate reset found" "$(metric "$out" kernel_hardware_error)" "1"

echo "== 11. 规则文件缺失 -> failed=1 =="
out=$("$SCRIPT" -l "$LOG" -s "$STATE" -p "$TMP/nope.conf" -f prometheus)
assert_eq "missing patterns failed" "$(metric "$out" kernel_hw_mon_failed)" "1"

echo "== 11b. 缺参守卫：-f 作为最后参数不死循环 =="
err=$("$SCRIPT" -f 2>&1 >/dev/null)
assert_eq "缺参报错" "$(printf '%s' "$err" | grep -c '缺少参数')" "1"

echo "== 11c. 空 include 正则不再全命中 =="
P_EMPTY="$TMP/patterns-empty"
printf 'source\t[[:space:]]kernel:\ninclude\tnic\tLink is Down\ninclude\traid\t\n' > "$P_EMPTY"
printf 'Sep 13 10:09:00 host kernel: normal message not an error\n' > "$LOG"
"$SCRIPT" -l "$LOG" -s "$STATE" -p "$P_EMPTY" -f prometheus >/dev/null 2>&1
printf 'Sep 13 10:09:30 host kernel: another normal line\n' >> "$LOG"
out=$("$SCRIPT" -l "$LOG" -s "$STATE" -p "$P_EMPTY" -f prometheus)
assert_eq "空 include 不误报" "$(metric "$out" kernel_hardware_error)" "0"
printf 'Sep 13 10:10:00 host kernel: eth0: Link is Down\n' >> "$LOG"
out=$("$SCRIPT" -l "$LOG" -s "$STATE" -p "$P_EMPTY" -f prometheus)
assert_eq "空 include 仍命中有效规则" "$(metric "$out" kernel_hardware_error)" "1"

echo "== 11d. 零 include 规则 -> failed=1 =="
printf 'source\t[[:space:]]kernel:\n# 无 include\n' > "$TMP/patterns-zero"
out=$("$SCRIPT" -l "$LOG" -s "$STATE" -p "$TMP/patterns-zero" -f prometheus)
assert_eq "零 include failed" "$(metric "$out" kernel_hw_mon_failed)" "1"

echo "== 11e. --scan 输出格式（ts\\ttype\\tmsg）=="
printf 'Sep 13 10:11:00 host kernel: eth0: Link is Down\n' > "$TMP/scan.log"
printf 'Sep 13 10:12:00 host myapp: I/O error\n' >> "$TMP/scan.log"
out=$("$SCRIPT" --scan -l "$TMP/scan.log" -p "$PATTERNS" -s "$TMP/scanstate")
assert_eq "scan 行数" "$(printf '%s\n' "$out" | wc -l | tr -d ' ')" "1"
case "$out" in
  *'	nic	'*"eth0: Link is Down") assert_eq "scan 格式(ts|type|msg)" "ok" "ok";;
  *) assert_eq "scan 格式(ts|type|msg)" "bad" "ok";;
esac

echo "== 11f. --scan --scan-max 限行 =="
for i in $(seq 1 5); do printf "Sep 13 10:13:0%d host kernel: eth0: Link is Down\n" $i >> "$TMP/scan.log"; done
out=$("$SCRIPT" --scan -l "$TMP/scan.log" -p "$PATTERNS" -s "$TMP/scanstate" --scan-max 3)
assert_eq "scan-max 3" "$(printf '%s\n' "$out" | wc -l | tr -d ' ')" "3"

echo "== 11g. --scan 失败约定（不可读日志 -> 非0退出）=="
if "$SCRIPT" --scan -l "$TMP/no-such-file.log" -p "$PATTERNS" -s "$TMP/scanstate" >/dev/null 2>&1; then
    assert_eq "scan 不可读日志 rc!=0" "bad" "ok"
else
    assert_eq "scan 不可读日志 rc!=0" "ok" "ok"
fi

echo "== 12. influx / falcon 输出格式 =="
out=$("$SCRIPT" -l "$LOG" -s "$STATE" -p "$PATTERNS" -f influx)
case "$out" in kernel_hw_mon*kernel_hardware_error=*) assert_eq "influx format" "ok" "ok";; *) assert_eq "influx format" "bad" "ok";; esac
out=$("$SCRIPT" -l "$LOG" -s "$STATE" -p "$PATTERNS" -f falcon)
case "$out" in *'"metric":"kernel_hardware_error"'*) assert_eq "falcon format" "ok" "ok";; *) assert_eq "falcon format" "bad" "ok";; esac

echo "== 13. textfile 输出 =="
TFD="$TMP/textfile"
"$SCRIPT" -l "$LOG" -s "$STATE" -p "$PATTERNS" -f textfile -t "$TFD" >/dev/null
if [ -f "$TFD/kernel_hw_mon.prom" ] && grep -q '^kernel_hardware_error ' "$TFD/kernel_hw_mon.prom"; then
    assert_eq "textfile written" "ok" "ok"
else
    assert_eq "textfile written" "bad" "ok"
fi

echo
echo "PASS=$PASS FAIL=$FAIL"
rm -rf "$TMP"
[ "$FAIL" -eq 0 ]
