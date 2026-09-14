// Package aggregate 对 ES 命中日志做去重/排序/截断，控制 LLM token 预算。
package aggregate

import (
	"sort"

	"kernel-hw-linkage/internal/es"
	"kernel-hw-linkage/internal/model"
)

// Result 聚合结果。
type Result struct {
	Lines []model.AggregatedLine
}

// Options 聚合参数。
type Options struct {
	MaxLines     int // 交给 LLM 的日志行（去重后）上限
	MaxCharsLine int // 单行截断
}

// Aggregate 将 ES 命中行聚合：
// 相同 message 合并计数，按出现次数降序取 top N，单行截断。
func Aggregate(hits []es.Hit, opt Options) Result {
	type agg struct {
		msg string
		n   int
	}
	counts := map[string]*agg{}
	var order []string
	for _, h := range hits {
		msg := h.Message
		if opt.MaxCharsLine > 0 {
			// 按 rune 截断，避免按字节切碎多字节 UTF-8（中文）产生非法字符
			r := []rune(msg)
			if len(r) > opt.MaxCharsLine {
				msg = string(r[:opt.MaxCharsLine]) + "..."
			}
		}
		if a, ok := counts[msg]; ok {
			a.n++
		} else {
			counts[msg] = &agg{msg: msg, n: 1}
			order = append(order, msg)
		}
	}
	list := make([]*agg, 0, len(counts))
	for _, m := range order {
		list = append(list, counts[m])
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].n > list[j].n })
	if opt.MaxLines > 0 && len(list) > opt.MaxLines {
		list = list[:opt.MaxLines]
	}
	out := make([]model.AggregatedLine, 0, len(list))
	for _, a := range list {
		out = append(out, model.AggregatedLine{Message: a.msg, Count: a.n})
	}
	return Result{Lines: out}
}
