#!/usr/bin/env bash
#
# es_search.sh - 从 Elasticsearch syslog-system 检索服务器日志（供 agent skill 使用）。
#
# 封装"按服务器 IP + 时间窗 + 硬件类型关键词"的检索，避免 agent 手写查询 DSL。
# 关键词单一事实源：linkage/config.example.json 的 es.type_keywords（与采集端 patterns.conf 对齐）。
#
# 用法:
#   es_search.sh --es <url> [--user U] [--pass P] [--index idx] \
#                [--ip 10.0.0.11] [--type nic] [--type mem] \
#                --start 2026-09-13T09:50:00Z --end 2026-09-13T10:00:00Z \
#                [--max 100] [--config ../linkage/config.example.json] [--keywords "kw1,kw2"]
#
# 说明:
#   - --type 的关键词自动从 config 的 es.type_keywords 读取（可多个 --type）。
#   - --keywords 显式指定时优先，覆盖 --type。
#   - 输出：先打印 source 聚合（确认命中的文件/IP），再打印命中的日志行。
#
set -u

ES=""
USER=""
PASS=""
INDEX="syslog-system"
IP=""
TYPES=()
START=""
END=""
MAX=100
CONFIG=""
KEYWORDS=""
SELF_DIR=$(cd "$(dirname "$0")" && pwd)
DEFAULT_CONFIG="$SELF_DIR/../../../linkage/config.example.json"

# 配置解析优先级：--config > 环境变量 LINKAGE_CONFIG > 部署配置 > 仓库样例
resolve_config() {
    [ -n "$CONFIG" ] && [ -f "$CONFIG" ] && { echo "$CONFIG"; return; }
    local c="${LINKAGE_CONFIG:-}"
    [ -n "$c" ] && [ -f "$c" ] && { echo "$c"; return; }
    [ -f /etc/kernel-hw-linkage/config.json ] && { echo /etc/kernel-hw-linkage/config.json; return; }
    [ -f "$DEFAULT_CONFIG" ] && { echo "$DEFAULT_CONFIG"; return; }
    echo ""
}

usage() {
    sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

while [ $# -gt 0 ]; do
    case "$1" in
        --es) ES=${2:-}; shift 2;;
        --user) USER=${2:-}; shift 2;;
        --pass) PASS=${2:-}; shift 2;;
        --index) INDEX=${2:-}; shift 2;;
        --ip) IP=${2:-}; shift 2;;
        --type) TYPES+=("${2:-}"); shift 2;;
        --start) START=${2:-}; shift 2;;
        --end) END=${2:-}; shift 2;;
        --max) MAX=${2:-}; shift 2;;
        --config) CONFIG=${2:-}; shift 2;;
        --keywords) KEYWORDS=${2:-}; shift 2;;
        -h|--help) usage;;
        *) echo "未知参数: $1" >&2; usage;;
    esac
done

[ -n "$ES" ] || { echo "缺少 --es" >&2; usage; }
[ -n "$START" ] && [ -n "$END" ] || { echo "缺少 --start/--end（RFC3339，如 2026-09-13T09:50:00Z）" >&2; usage; }
[ -n "$IP" ] || { echo "缺少 --ip（或先用 inventory/ES 反查得到 IP）" >&2; usage; }
[ -n "$KEYWORDS" ] || [ ${#TYPES[@]} -gt 0 ] || { echo "缺少 --type 或 --keywords" >&2; usage; }

# 关键词解析：--keywords 优先；否则从 config 读 --type
if [ -n "$KEYWORDS" ]; then
    IFS=',' read -r -a KWS <<< "$KEYWORDS"
else
    CONFIG=$(resolve_config)
    [ -n "$CONFIG" ] || { echo "找不到 config：--config / LINKAGE_CONFIG / /etc/kernel-hw-linkage/config.json / 仓库样例" >&2; usage; }
    KWS=()
    for t in "${TYPES[@]+"${TYPES[@]}"}"; do
        if command -v python3 >/dev/null 2>&1; then
            list=$(python3 - "$CONFIG" "$t" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
for kw in cfg.get("es", {}).get("type_keywords", {}).get(sys.argv[2], []):
    print(kw)
PY
            )
        elif command -v jq >/dev/null 2>&1; then
            list=$(jq -r --arg t "$t" '.es.type_keywords[$t][]' "$CONFIG")
        else
            echo "需要 python3 或 jq 解析 config 关键词" >&2; exit 2
        fi
        while IFS= read -r kw; do
            [ -n "$kw" ] && KWS+=("$kw")
        done <<< "$list"
    done
    [ ${#KWS[@]} -gt 0 ] || { echo "类型 ${TYPES[*]} 无关键词" >&2; exit 2; }
fi

# 构建 DSL
should=""
for kw in "${KWS[@]+"${KWS[@]}"}"; do
    clause="{\"match_phrase\":{\"message\":\"$kw\"}}"
    [ -n "$should" ] && should="$should,"
    should="$should$clause"
done

BODY=$(cat <<EOF
{"size":${MAX},"query":{"bool":{"filter":[
 {"range":{"@timestamp":{"gte":"${START}","lte":"${END}"}}},
 {"wildcard":{"source.keyword":"/syslog/system/*/${IP}_*.log"}},
 {"bool":{"should":[${should}]}}
]}},"aggs":{"by_source":{"terms":{"field":"source.keyword","size":10}}},"sort":[{"@timestamp":"asc"}]}
EOF
)

AUTH=()
[ -n "$USER" ] && AUTH=(-u "${USER}:${PASS}")

echo "== 检索: index=$INDEX ip=$IP types=${TYPES[*]:-自定义} 关键词数=${#KWS[@]} 窗口=${START}~${END} =="
RESP=$(curl -s "${AUTH[@]+"${AUTH[@]}"}" -H "Content-Type: application/json" \
    -X POST "${ES%/}/${INDEX}/_search" -d "$BODY")

if command -v python3 >/dev/null 2>&1; then
    printf '%s' "$RESP" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception as e:
    print("ES 响应解析失败:", e); sys.exit(1)
if "error" in d:
    print("ES 错误:", d["error"]); sys.exit(1)
buckets = d.get("aggregations", {}).get("by_source", {}).get("buckets", [])
print("== source 聚合 ==")
for b in buckets:
    print("  " + str(b["key"]) + ": " + str(b["doc_count"]))
hits = d.get("hits", {}).get("hits", [])
print("== 命中 " + str(len(hits)) + " 行 ==")
for h in hits:
    s = h.get("_source", {})
    print("  " + str(s.get("@timestamp","")) + " " + str(s.get("source","")) + "  " + str(s.get("message","")))
'
else
    printf '%s' "$RESP" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$RESP"
fi
