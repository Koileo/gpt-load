// Package turnstate 提供进程内的 Codex turn-state 观测总线。执行层在尝试结束
// 时上报观测值，订阅方（控制面的后台 watcher）按需消费；没有任何订阅者时
// Observe 是一次空操作，普通请求路径不承担额外成本。
package turnstate

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

const (
	fernetVersion            = 0x80
	fernetPrefixBytes        = 1 + 8
	fernetIVBytes            = 16
	fernetHMACBytes          = 32
	fernetBlockBytes         = 16
	fernetIssuedFloorSeconds = 1_577_836_800 // 2020-01-01
	fernetIssuedCeilSeconds  = 4_102_444_800 // 2100-01-01
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

// FernetIssuedAt reads the authenticated envelope's clear-text timestamp. It
// validates only the standard Fernet framing; callers must not treat this as
// signature verification.
func FernetIssuedAt(value string) (time.Time, bool) {
	encoded := strings.TrimSpace(value)
	padding := len(encoded) - len(strings.TrimRight(encoded, "="))
	if encoded == "" || padding > 2 {
		return time.Time{}, false
	}
	body := strings.TrimSuffix(strings.TrimSuffix(encoded, "="), "=")
	decoded, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(decoded) < fernetPrefixBytes+fernetIVBytes+fernetBlockBytes+fernetHMACBytes ||
		decoded[0] != fernetVersion {
		return time.Time{}, false
	}
	cipherBytes := len(decoded) - fernetPrefixBytes - fernetIVBytes - fernetHMACBytes
	if cipherBytes < fernetBlockBytes || cipherBytes%fernetBlockBytes != 0 {
		return time.Time{}, false
	}
	seconds := binary.BigEndian.Uint64(decoded[1:fernetPrefixBytes])
	if seconds < fernetIssuedFloorSeconds || seconds >= fernetIssuedCeilSeconds {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0), true
}

type subscriber func(Observation)

var (
	mu          sync.RWMutex
	nextID      uint64
	subscribers = map[uint64]subscriber{}
)

// Subscribe 注册一个订阅者并返回解除函数。允许多个独立消费者同时观察，解除
// 某一个订阅不会覆盖或恢复其他订阅者。
func Subscribe(fn subscriber) (restore func()) {
	if fn == nil {
		return func() {}
	}
	mu.Lock()
	nextID++
	id := nextID
	subscribers[id] = fn
	mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			delete(subscribers, id)
			mu.Unlock()
		})
	}
}

// Observe 上报一次观测；没有订阅者时立即返回。回调在调用方 goroutine 中执行，
// 订阅方必须自行保证不阻塞。
func Observe(o Observation) {
	mu.RLock()
	callbacks := make([]subscriber, 0, len(subscribers))
	for _, fn := range subscribers {
		callbacks = append(callbacks, fn)
	}
	mu.RUnlock()
	for _, fn := range callbacks {
		fn(o)
	}
}
