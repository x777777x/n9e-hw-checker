#!/usr/bin/env bash
#
# check-type-alignment.sh - 校验 patterns.conf 的 include 类别与联动服务
# config.json 的 es.type_keywords 是否对齐（避免两侧漂移）。
#
# 用法:
#   bash tools/check-type-alignment.sh <patterns.conf> <config.json>
# 退出码 0 = 对齐；非 0 = 差异，并打印 diff。
#
set -u
export LC_ALL=C

PATTERNS="${1:?patterns.conf 路径}"
CONFIG="${2:?linkage config.json 路径}"
RC=0

patterns_types=$(awk 'BEGIN{FS="\t"} $1=="include" && $2!="" {print $2}' "$PATTERNS" 2>/dev/null | sort -u)

# 兼容紧凑(单行)与格式化 JSON：拍平后截取 type_keywords 对象，再提取键
cfg_types=$(tr -d '\n' < "$CONFIG" 2>/dev/null \
    | grep -oE '"type_keywords"[^}]*' \
    | grep -oE '"[a-zA-Z0-9_]+"[[:space:]]*:[[:space:]]*\[' \
    | sed -E 's/^"//; s/".*//' \
    | sort -u)

if [ -z "$patterns_types" ]; then
    echo "错误: 无法从 $PATTERNS 提取 include 类别" >&2
    exit 1
fi
if [ -z "$cfg_types" ]; then
    echo "错误: 无法从 $CONFIG 提取 es.type_keywords 类型" >&2
    exit 1
fi

echo "patterns.conf 类别: $(echo "$patterns_types" | tr '\n' ' ')"
echo "config.json  类型:  $(echo "$cfg_types" | tr '\n' ' ')"

# 差集（comm 需要排序输入，两者已 sort -u）
only_patterns=$(comm -23 <(printf '%s\n' "$patterns_types") <(printf '%s\n' "$cfg_types"))
only_cfg=$(comm -13 <(printf '%s\n' "$patterns_types") <(printf '%s\n' "$cfg_types"))

if [ -n "$only_patterns" ]; then
    echo "差异: 仅在 patterns.conf 出现的类别:" >&2
    echo "$only_patterns" | sed 's/^/  - /' >&2
    RC=1
fi
if [ -n "$only_cfg" ]; then
    echo "差异: 仅在 config.json 出现的类型:" >&2
    echo "$only_cfg" | sed 's/^/  - /' >&2
    RC=1
fi

[ $RC -eq 0 ] && echo "OK: 类别集合一致"
exit $RC
