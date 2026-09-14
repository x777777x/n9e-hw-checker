// Package buffer 实现同一 (ident, 时间窗) 内多类型告警的合并聚合。
//
// 同一台服务器同一分钟内 nic+mem 同时触发时，n9e 会发多个回调。
// 这里将同窗口的事件缓冲 MergeSeconds，期间到达的类型合并，
// 空闲超时或最大等待后一次性交给处理器，生成一份综合报告。
package buffer

import (
	"sync"
	"time"
)

// Event 一次待处理告警事件。
type Event struct {
	Ident       string
	Type        string
	Window      int64 // 去重窗口桶（unix 秒 / dedup_minutes*60），用于多类型合并
	TriggerTime int64 // unix 秒
	RuleName    string
}

// Callback 聚合完成后被调用，types 为合并后的类型集合。
type Callback func(ev Event, types []string)

// Aggregator 多类型合并缓冲。
type Aggregator struct {
	mu      sync.Mutex
	entries map[string]*entry
	wait    time.Duration
	cb      Callback
}

type entry struct {
	ev      Event
	types   map[string]bool
	timer   *time.Timer
	maxWait time.Duration
}

// New 创建聚合器，wait 为合并等待时长。
func New(wait time.Duration, cb Callback) *Aggregator {
	return &Aggregator{
		entries: make(map[string]*entry),
		wait:    wait,
		cb:      cb,
	}
}

// Add 加入一个事件；返回 true 表示是首个事件（窗口开始）。
func (a *Aggregator) Add(ev Event) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := ev.Ident + "|" + itoa(ev.Window)
	e, ok := a.entries[k]
	if !ok {
		e = &entry{
			ev:      ev,
			types:   map[string]bool{ev.Type: true},
			maxWait: a.wait,
		}
		a.entries[k] = e
		e.timer = time.AfterFunc(a.wait, func() { a.fire(k) })
		return true
	}
	e.types[ev.Type] = true
	// 已有类型去重由调用方负责；这里仅刷新计时
	return false
}

// Drain 立即触发所有待合并条目（优雅停机时补发缓冲中已去重、尚未分析的事件，
// 避免停机丢失后再也不会复发的告警）。同步执行回调。
func (a *Aggregator) Drain() {
	a.mu.Lock()
	keys := make([]string, 0, len(a.entries))
	for k := range a.entries {
		keys = append(keys, k)
	}
	a.mu.Unlock()
	for _, k := range keys {
		a.fire(k)
	}
}

// fire 从 map 移除并调用回调。
func (a *Aggregator) fire(k string) {
	a.mu.Lock()
	e, ok := a.entries[k]
	if ok {
		delete(a.entries, k)
	}
	a.mu.Unlock()
	if !ok {
		return
	}
	types := make([]string, 0, len(e.types))
	for t := range e.types {
		types = append(types, t)
	}
	a.cb(e.ev, types)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
