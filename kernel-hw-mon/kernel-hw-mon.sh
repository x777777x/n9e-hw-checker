#!/usr/bin/env bash
#
# kernel-hw-mon.sh - 从系统日志增量发现内核/硬件错误
#
# 设计要点:
#   * 状态文件记录 <inode> <offset> <last_epoch>，只读取上次以来的增量。
#   * gawk 用 RT 判断行是否完整，游标始终停在行边界，避免读半行/重复。
#   * 首次运行从 EOF 开始，避免重放历史日志造成告警风暴。
#   * 始终以退出码 0 结束；失败通过 kernel_hw_mon_failed 指标暴露。
#   * 按硬件类型输出指标 kernel_hardware_error_type{type=...}，类别从 patterns.conf
#     自动解析（不硬编码），供联动系统按类型精确查询 Elasticsearch。
#
# 兼容: RHEL/CentOS/Rocky/Kylin v10 (gawk, flock, GNU coreutils)
#
set -o pipefail
export LC_ALL=C
umask 077

PROG=${0##*/}
VERSION="1.1.0"

LOG_FILE="/var/log/messages"
STATE_DIR="/var/lib/kernel-hw-mon"
STATE_FILE="$STATE_DIR/state"
PATTERN_FILE="/etc/kernel-hw-mon/patterns.conf"
TEXTFILE_DIR="/var/lib/node_exporter/textfile_collector"
FORMAT="prometheus"
METRIC="kernel_hardware_error"
TYPE_METRIC="kernel_hardware_error_type"
FAIL_METRIC="kernel_hw_mon_failed"
TS_METRIC="kernel_hw_mon_last_run_timestamp"
JOURNAL=0
DEBUG=0
SCAN=0
SCAN_MAX=0
TYPES=""

usage() {
    cat <<EOF
$PROG v$VERSION - 增量检测系统日志中的内核/硬件错误

模式:
  （默认）增量检测并输出指标（供 Categraf/alert 采集）
  --scan               全量扫描日志并逐行输出 "<时间>\\t<类型>\\t<关键信息>"
                       （供 on-demand 排查/ansible 报表，不推进游标）

选项:
  -f, --format <fmt>       输出格式: prometheus|influx|falcon|textfile (默认: $FORMAT)
  -l, --log-file <path>    日志文件 (默认: $LOG_FILE)
  -s, --state-dir <dir>    状态目录 (默认: $STATE_DIR)
  -p, --patterns <file>    规则文件 (默认: $PATTERN_FILE)
  -t, --textfile-dir <dir> textfile 输出目录 (默认: $TEXTFILE_DIR)
  -m, --metric <name>      主指标名 (默认: $METRIC)
  -j, --journal            使用 journalctl -k 代替文件游标
  -d, --debug              调试日志输出到 stderr
      --scan-max <n>       --scan 模式下最多输出行数 (默认不限)
  -h, --help               显示本帮助

输出指标:
  $METRIC                      发现内核/硬件错误 = 1，否则 0
  $TYPE_METRIC{type=...}  按类型 0/1（类别取自 patterns.conf include 行）
  $FAIL_METRIC                 监控执行失败 = 1
  $TS_METRIC                   上次运行 Unix 时间戳
EOF
}

# 带值选项守卫：防止 -f 等作为最后一个参数时 shift 2 越界死循环
need_arg() {
    if [ "$#" -lt 2 ]; then
        echo "选项 $1 缺少参数" >&2
        exit 0
    fi
}

while [ $# -gt 0 ]; do
    case "$1" in
        -f|--format)      need_arg "$@"; FORMAT=$2; shift 2;;
        -l|--log-file)    need_arg "$@"; LOG_FILE=$2; shift 2;;
        -s|--state-dir)   need_arg "$@"; STATE_DIR=$2; STATE_FILE="$STATE_DIR/state"; shift 2;;
        -p|--patterns)    need_arg "$@"; PATTERN_FILE=$2; shift 2;;
        -t|--textfile-dir) need_arg "$@"; TEXTFILE_DIR=$2; shift 2;;
        -m|--metric)      need_arg "$@"; METRIC=$2; shift 2;;
        -j|--journal)     JOURNAL=1; shift;;
        -d|--debug)       DEBUG=1; shift;;
        --scan)           SCAN=1; shift;;
        --scan-max)       need_arg "$@"; SCAN_MAX=$2; shift 2;;
        -h|--help)        usage; exit 0;;
        *) echo "未知选项: $1" >&2; usage >&2; exit 0;;
    esac
done

# ------------------------------- 日志与状态 -------------------------------

log_msg() {
    local level=$1; shift
    [ -d "$STATE_DIR" ] || mkdir -p "$STATE_DIR" 2>/dev/null
    { printf '%s %s %s\n' "$(date '+%F %T')" "$level" "$*" >> "$STATE_DIR/monitor.log" 2>/dev/null; } 2>/dev/null
    [ "$DEBUG" -eq 1 ] && printf '%s %s\n' "$level" "$*" >&2
    return 0
}

trim_log() {
    local f="$STATE_DIR/monitor.log" sz
    [ -f "$f" ] || return 0
    sz=$(stat -Lc '%s' "$f" 2>/dev/null) || return 0
    if [ "${sz:-0}" -gt 1048576 ]; then
        tail -c 262144 "$f" > "$f.tmp" 2>/dev/null && mv -f "$f.tmp" "$f" 2>/dev/null
    fi
    return 0
}

HAVE_STATE=0
ST_MODE=""; ST_INODE=""; ST_OFFSET=0; ST_TS=0

# 状态文件: <mode> <inode> <offset> <last_epoch>
# mode = file | journal；兼容旧的 3 字段格式（视为 file 模式）
read_state() {
    local f1 f2 f3 f4
    [ -f "$STATE_FILE" ] || return 0
    read -r f1 f2 f3 f4 < "$STATE_FILE" 2>/dev/null || return 0
    if [ "$f1" = "file" ] || [ "$f1" = "journal" ]; then
        ST_MODE=$f1; ST_INODE=$f2; ST_OFFSET=${f3:-0}; ST_TS=${f4:-0}
    else
        ST_MODE="file"; ST_INODE=$f1; ST_OFFSET=${f2:-0}; ST_TS=${f3:-0}
    fi
    case "$ST_INODE" in ''|*[!0-9]*) ST_INODE="";; esac
    case "$ST_OFFSET" in ''|*[!0-9]*) ST_OFFSET=0;; esac
    case "$ST_TS"     in ''|*[!0-9]*) ST_TS=0;;     esac

    if [ "$ST_MODE" = "file" ]; then
        # 文件模式：inode 有效才视为有状态；inode=0 表示仅 journal 用过，按首跑
        [ -n "$ST_INODE" ] && [ "$ST_INODE" -gt 0 ] || return 0
        HAVE_STATE=1
    else
        # journal 模式：ts 有效才视为有状态
        [ "$ST_TS" -gt 0 ] || return 0
        HAVE_STATE=1
    fi
    return 0
}

write_state() {
    local tmp="$STATE_FILE.$$"
    { printf '%s %s %s %s\n' "$1" "$2" "$3" "$4" > "$tmp" 2>/dev/null \
        && mv -f "$tmp" "$STATE_FILE" 2>/dev/null \
        || rm -f "$tmp" 2>/dev/null; } 2>/dev/null
    return 0
}

# ------------------------------- 类型列表 -------------------------------

# 从 patterns.conf 提取 distinct include 类别（单一事实源，不硬编码）
# 判定与 awk 引擎一致：include 且类别、正则均非空（n>=3 且 $3!=""）
get_types() {
    [ -r "$PATTERN_FILE" ] || return 0
    awk 'BEGIN{FS="\t"} $1=="include" && $2!="" && $3!="" {print $2}' "$PATTERN_FILE" 2>/dev/null \
        | sort -u | tr '\n' ' '
}

# ------------------------------- 输出适配 -------------------------------

# 入参: found failed ts cats   全局: TYPES
emit_prom() {
    local found=$1 failed=$2 ts=$3 cats=$4 t v
    printf '# HELP %s detected kernel/hardware error in last window\n' "$METRIC"
    printf '# TYPE %s gauge\n' "$METRIC"
    printf '%s %s\n' "$METRIC" "$found"
    printf '# HELP %s detected kernel/hardware error by type\n' "$TYPE_METRIC"
    printf '# TYPE %s gauge\n' "$TYPE_METRIC"
    for t in $TYPES; do
        case ",$cats," in *",$t,"*) v=1;; *) v=0;; esac
        printf '%s{type="%s"} %s\n' "$TYPE_METRIC" "$t" "$v"
    done
    printf '# HELP %s monitor execution failure\n' "$FAIL_METRIC"
    printf '# TYPE %s gauge\n' "$FAIL_METRIC"
    printf '%s %s\n' "$FAIL_METRIC" "$failed"
    printf '# HELP %s unix timestamp of last monitor run\n' "$TS_METRIC"
    printf '# TYPE %s gauge\n' "$TS_METRIC"
    printf '%s %s\n' "$TS_METRIC" "$ts"
}

emit_influx() {
    local found=$1 failed=$2 ts=$3 cats=$4 t v
    printf 'kernel_hw_mon %s=%s,%s=%s,%s=%s\n' \
        "$METRIC" "$found" "$FAIL_METRIC" "$failed" "$TS_METRIC" "$ts"
    for t in $TYPES; do
        case ",$cats," in *",$t,"*) v=1;; *) v=0;; esac
        printf 'kernel_hw_mon_type,type=%s %s=%s\n' "$t" "$TYPE_METRIC" "$v"
    done
}

emit_falcon() {
    local found=$1 failed=$2 ts=$3 cats=$4 t v out
    out="[{\"metric\":\"$METRIC\",\"value\":$found,\"tags\":\"source=syslog\"},{\"metric\":\"$FAIL_METRIC\",\"value\":$failed,\"tags\":\"source=syslog\"},{\"metric\":\"$TS_METRIC\",\"value\":$ts,\"tags\":\"source=syslog\"}"
    for t in $TYPES; do
        case ",$cats," in *",$t,"*) v=1;; *) v=0;; esac
        out="$out,{\"metric\":\"$TYPE_METRIC\",\"value\":$v,\"tags\":\"source=syslog,type=$t\"}"
    done
    printf '%s]\n' "$out"
}

emit_textfile() {
    mkdir -p "$TEXTFILE_DIR" 2>/dev/null
    local tmp="$TEXTFILE_DIR/.kernel_hw_mon.prom.$$"
    if { emit_prom "$1" "$2" "$3" "$4"; } > "$tmp" 2>/dev/null; then
        mv -f "$tmp" "$TEXTFILE_DIR/kernel_hw_mon.prom" 2>/dev/null \
            || { rm -f "$tmp" 2>/dev/null; log_msg ERROR "cannot move textfile in $TEXTFILE_DIR"; }
    else
        rm -f "$tmp" 2>/dev/null
        log_msg ERROR "cannot write textfile dir $TEXTFILE_DIR"
    fi
    emit_prom "$1" "$2" "$3" "$4"
}

emit() {
    case "$FORMAT" in
        prometheus) emit_prom "$1" "$2" "$3" "$4";;
        influx)     emit_influx "$1" "$2" "$3" "$4";;
        falcon)     emit_falcon "$1" "$2" "$3" "$4";;
        textfile)   emit_textfile "$1" "$2" "$3" "$4";;
        *)          log_msg ERROR "unknown format: $FORMAT"; emit_prom "$1" "$2" "$3" "$4";;
    esac
}

# ------------------------------- 匹配引擎 -------------------------------

AWK_PROG='
BEGIN {
    ns = 0; ni = 0; ne = 0
    delete seen
    while ((getline line < patfile) > 0) {
        sub(/\r$/, "", line)
        if (line == "" || line ~ /^[ \t]*#/) continue
        n = split(line, a, "\t")
        if (n < 2) continue
        kind = a[1]
        # 跳过空正则，避免空分支匹配所有行（误报）或造成解析异常
        if (kind == "source")       { if (a[2] != "") { ns++; src[ns] = a[2] } }
        else if (kind == "include") { if (n >= 3 && a[2] != "" && a[3] != "") { ni++; inc[ni] = a[3]; cname[ni] = a[2] } }
        else if (kind == "exclude") { if (a[2] != "") { ne++; exc[ne] = a[2] } }
    }
    close(patfile)
    # 预先拼接为单个正则，每行只做 3 次匹配，避免逐条动态正则的开销
    src_re = ""; for (i = 1; i <= ns; i++) src_re = src_re (i > 1 ? "|" : "") src[i]
    inc_re = ""; for (i = 1; i <= ni; i++) inc_re = inc_re (i > 1 ? "|" : "") inc[i]
    exc_re = ""; for (i = 1; i <= ne; i++) exc_re = exc_re (i > 1 ? "|" : "") exc[i]
    if (enforce_source == 0) src_re = ""
}
{
    if (scan) {
        if (src_re != "" && $0 !~ src_re) next
        if (exc_re != "" && $0 ~ exc_re) next
        if (inc_re != "" && $0 ~ inc_re) {
            t = ""
            for (i = 1; i <= ni; i++) if ($0 ~ inc[i]) { t = cname[i]; break }
            if ($0 ~ /^[0-9]{4}-[0-9]{2}-[0-9]{2}T/) {
                ts = $1
                sub(/^[^ \t]+[ \t]+/, "", $0)
            } else {
                ts = $1 " " $2 " " $3
                sub(/^([^ \t]+[ \t]+){3}/, "", $0)
            }
            msg = $0
            sub(/^[^ \t]+[ \t]+/, "", msg)   # 去 host
            sub(/^[^:]*:[ \t]*/, "", msg)    # 去 tag:
            sub(/^[ \t]+/, "", msg)
            print ts "\t" t "\t" msg
            if (scanmax > 0 && ++scancnt >= scanmax) exit
        }
        next
    }
    if (RT == "") next
    consumed += length($0) + length(RT)
    if (src_re != "" && $0 !~ src_re) next
    if (exc_re != "" && $0 ~ exc_re) next
    if (inc_re != "" && $0 ~ inc_re) {
        found = 1
        for (i = 1; i <= ni; i++) if ($0 ~ inc[i]) seen[cname[i]] = 1
        if (debug) {
            for (i = 1; i <= ni; i++) if ($0 ~ inc[i]) { print "MATCH[" cname[i] "]: " $0 > "/dev/stderr"; break }
        }
    }
}
END {
    if (scan) exit
    ncat = 0
    for (c in seen) cats[++ncat] = c
    if (ncat > 1) asort(cats)
    out = ""
    for (i = 1; i <= ncat; i++) out = out (i > 1 ? "," : "") cats[i]
    print consumed+0, found+0, out
}
'

# ------------------------------- 运行模式 -------------------------------

run_file() {
    local now=$1
    local cur_inode cur_size off new_off result rc consumed found cats err_redir

    cur_inode=$(stat -Lc '%i' -- "$LOG_FILE" 2>/dev/null)
    cur_size=$(stat -Lc '%s' -- "$LOG_FILE" 2>/dev/null)
    if [ -z "$cur_inode" ] || [ -z "$cur_size" ]; then
        log_msg ERROR "cannot stat $LOG_FILE"
        emit 0 1 "$now" ""
        return 0
    fi

    if [ "$HAVE_STATE" -ne 1 ] || [ "$ST_MODE" != "file" ]; then
        write_state "file" "$cur_inode" "$cur_size" "$now"
        log_msg INFO "first run, start at EOF offset=$cur_size"
        emit 0 0 "$now" ""
        return 0
    fi

    off=$ST_OFFSET
    if [ "$ST_INODE" != "$cur_inode" ]; then
        log_msg INFO "log rotated (inode $ST_INODE -> $cur_inode), reset offset"
        off=0
    elif [ "$off" -gt "$cur_size" ]; then
        log_msg INFO "log truncated (offset $off > size $cur_size), reset offset"
        off=0
    fi

    if [ "$off" -eq "$cur_size" ]; then
        write_state "file" "$cur_inode" "$cur_size" "$now"
        emit 0 0 "$now" ""
        return 0
    fi

    err_redir="/dev/null"
    [ "$DEBUG" -eq 1 ] && err_redir="/dev/stderr"
    result=$(tail -c +$((off + 1)) -- "$LOG_FILE" 2>/dev/null \
        | gawk -v patfile="$PATTERN_FILE" -v debug="$DEBUG" -v enforce_source=1 "$AWK_PROG" 2>"$err_redir")
    rc=$?
    if [ $rc -ne 0 ] || [ -z "$result" ]; then
        log_msg ERROR "gawk failed (rc=$rc) reading $LOG_FILE, cursor not advanced"
        emit 0 1 "$now" ""
        return 0
    fi

    set -- $result
    consumed=${1:-0}; found=${2:-0}; cats=${3:-}
    case "$consumed" in ''|*[!0-9]*) consumed=0;; esac
    case "$found"    in ''|*[!0-9]*) found=0;;    esac

    new_off=$((off + consumed))
    write_state "file" "$cur_inode" "$new_off" "$now"
    emit "$found" 0 "$now" "$cats"
}

run_journal() {
    local now=$1 since result rc found cats err_redir
    if ! command -v journalctl >/dev/null 2>&1; then
        log_msg ERROR "journalctl not found and log file unreadable: $LOG_FILE"
        emit 0 1 "$now" ""
        return 0
    fi

    since=${ST_TS:-0}
    case "$since" in ''|*[!0-9]*) since=0;; esac
    # 仅当状态是 journal 模式且 ts 有效才续用；否则从 1 分钟前开始
    if [ "$ST_MODE" != "journal" ] || [ "$since" -le 0 ] || [ "$since" -gt "$now" ] || [ $((now - since)) -gt 3600 ]; then
        since=$((now - 60))
    fi

    err_redir="/dev/null"
    [ "$DEBUG" -eq 1 ] && err_redir="/dev/stderr"
    result=$(journalctl -k --no-pager -o cat --since "@$since" 2>/dev/null \
        | gawk -v patfile="$PATTERN_FILE" -v debug="$DEBUG" -v enforce_source=0 "$AWK_PROG" 2>"$err_redir")
    rc=$?
    if [ $rc -ne 0 ] || [ -z "$result" ]; then
        log_msg ERROR "journalctl/gawk failed (rc=$rc)"
        emit 0 1 "$now" ""
        return 0
    fi

    set -- $result
    found=${2:-0}; cats=${3:-}
    case "$found" in ''|*[!0-9]*) found=0;; esac

    write_state "journal" 0 0 "$now"
    emit "$found" 0 "$now" "$cats"
}

# ------------------------------- 扫描模式 -------------------------------

# --scan：全量扫描日志，逐行输出 "<时间>\t<类型>\t<关键信息>"（不推进游标）
# 失败约定：不可读/无规则等错误走 stderr 并以非 0 退出，便于调用方识别失败。
run_scan() {
    local err_redir
    [ -r "$LOG_FILE" ] || { echo "日志不可读: $LOG_FILE" >&2; return 1; }
    err_redir="/dev/null"
    [ "$DEBUG" -eq 1 ] && err_redir="/dev/stderr"
    gawk -v patfile="$PATTERN_FILE" -v debug="$DEBUG" -v enforce_source=1 \
         -v scan=1 -v scanmax="$SCAN_MAX" "$AWK_PROG" "$LOG_FILE" 2>"$err_redir"
}

main() {
    local now
    now=$(date +%s)

    # --scan 模式：不触碰生产 state 目录，失败走 stderr + 非 0 退出
    if [ "$SCAN" -eq 1 ]; then
        if [ ! -r "$PATTERN_FILE" ]; then
            echo "patterns 不可读: $PATTERN_FILE" >&2
            exit 1
        fi
        if ! command -v gawk >/dev/null 2>&1; then
            echo "gawk not found" >&2
            exit 1
        fi
        TYPES=$(get_types)
        if [ -z "$TYPES" ]; then
            echo "patterns 无任何 include 规则: $PATTERN_FILE" >&2
            exit 1
        fi
        run_scan
        return $?
    fi

    mkdir -p "$STATE_DIR" 2>/dev/null
    trim_log

    if [ ! -r "$PATTERN_FILE" ]; then
        log_msg ERROR "pattern file not readable: $PATTERN_FILE"
        emit 0 1 "$now" ""
        return 0
    fi
    if ! command -v gawk >/dev/null 2>&1; then
        log_msg ERROR "gawk not found"
        emit 0 1 "$now" ""
        return 0
    fi

    TYPES=$(get_types)

    # 零 include 规则视为配置错误：静默漏报比告警更危险
    if [ -z "$TYPES" ]; then
        log_msg ERROR "patterns 无任何 include 规则: $PATTERN_FILE"
        emit 0 1 "$now" ""
        return 0
    fi

    if [ ! -d "$STATE_DIR" ] || [ ! -w "$STATE_DIR" ]; then
        log_msg ERROR "state dir not writable: $STATE_DIR"
        emit 0 1 "$now" ""
        return 0
    fi

    if command -v flock >/dev/null 2>&1; then
        if ! { exec 9>"$STATE_DIR/.lock"; } 2>/dev/null; then
            log_msg ERROR "cannot open lock file: $STATE_DIR/.lock"
            emit 0 1 "$now" ""
            return 0
        fi
        if ! flock -n 9; then
            log_msg INFO "another instance is running, skip"
            return 0
        fi
    fi

    read_state

    if [ "$JOURNAL" -eq 1 ] || [ ! -r "$LOG_FILE" ]; then
        run_journal "$now"
    else
        run_file "$now"
    fi
}

main
# metric 模式 main 恒返回 0（exec 插件兼容）；--scan 失败时透传非 0，便于调用方识别
exit $?
