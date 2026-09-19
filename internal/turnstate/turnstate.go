// Package turnstate 提供进程内的 Codex turn-state 观测总线。执行层在尝试结束
// 时上报观测值，订阅方（控制面的后台 watcher）按需消费；没有任何订阅者时
// Observe 是一次空操作，普通请求路径不承担额外成本。
package turnstate

import (
	"sync"
	"time"
)

// Observation 是一次上游尝试观测到的 turn state 快照。只承载元数据，值本身
// 由订阅方自行决定是否落盘或展示。
type Observation struct {
	CredentialID  uint
	ClientModel   string
	UpstreamModel string
	StatusCode    int
	TurnState     string
	ObservedAt    time.Time
}

type subscriber func(Observation)

var (
	mu      sync.RWMutex
	current subscriber
)

// Subscribe 把 fn 设为当前订阅者并返回恢复函数；后订阅者覆盖先订阅者，
// 恢复函数用于测试与停机时还原。
func Subscribe(fn subscriber) (restore func()) {
	mu.Lock()
	previous := current
	current = fn
	mu.Unlock()
	return func() {
		mu.Lock()
		current = previous
		mu.Unlock()
	}
}

// Observe 上报一次观测；没有订阅者时立即返回，订阅方必须自行保证不阻塞。
func Observe(o Observation) {
	mu.RLock()
	fn := current
	mu.RUnlock()
	if fn != nil {
		fn(o)
	}
}
