// Package dedup 提供事件幂等去重。
//
// 当前实现为进程内（MemoryDeduper），适用于单实例部署。
// 若要支持多实例/共享状态，实现 Deduper 接口并替换为 Redis 版
// （SETNX key EX ttl），接口不变。
package dedup

import (
	"crypto/sha1"
	"fmt"
	"sync"
	"time"
)

// Deduper 幂等去重接口。
type Deduper interface {
	// Acquire 返回 false 表示该事件在窗口内已处理过，应跳过。
	Acquire(ident, typ string, window int64) bool
}

// MemoryDeduper 进程内实现，带过期清理。
type MemoryDeduper struct {
	mu     sync.Mutex
	seen   map[string]int64 // key -> 过期 unix 秒
	ttlSec int64
}

// NewMemory 创建一个内存去重器，ttl 为窗口时长。
func NewMemory(ttl time.Duration) *MemoryDeduper {
	return &MemoryDeduper{
		seen:   make(map[string]int64),
		ttlSec: int64(ttl.Seconds()),
	}
}

func key(ident, typ string, window int64) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%d", ident, typ, window)))
	return fmt.Sprintf("%x", h[:8])
}

// Acquire 实现 Deduper。
func (d *MemoryDeduper) Acquire(ident, typ string, window int64) bool {
	k := key(ident, typ, window)
	now := time.Now().Unix()
	d.mu.Lock()
	defer d.mu.Unlock()
	if exp, ok := d.seen[k]; ok && exp > now {
		return false
	}
	d.seen[k] = now + d.ttlSec
	// 机会式清理，避免 map 无限增长
	if len(d.seen) > 10000 {
		for kk, exp := range d.seen {
			if exp <= now {
				delete(d.seen, kk)
			}
		}
	}
	return true
}
