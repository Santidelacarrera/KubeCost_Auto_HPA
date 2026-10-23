// Package cache provides the temporary-state store (metrics cache and scaling
// decision state). Redis is the production backend; Memory is used when Redis
// is not configured and in tests.
package cache

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store is a minimal JSON key/value store with TTLs.
type Store interface {
	// Get decodes the value into v. found is false when the key is absent.
	Get(ctx context.Context, key string, v any) (found bool, err error)
	Set(ctx context.Context, key string, v any, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// Redis implements Store on Redis.
type Redis struct{ C redis.UniversalClient }

// NewRedis connects lazily; errors surface on first use.
func NewRedis(addr, password string, db int) *Redis {
	return NewRedisTLS(addr, password, db, nil)
}

// NewRedisTLS is NewRedis over TLS when tlsCfg is not nil.
func NewRedisTLS(addr, password string, db int, tlsCfg *tls.Config) *Redis {
	return &Redis{C: redis.NewClient(&redis.Options{
		Addr: addr, Password: password, DB: db, TLSConfig: tlsCfg,
		DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
	})}
}

func (r *Redis) Get(ctx context.Context, key string, v any) (bool, error) {
	b, err := r.C.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (r *Redis) Set(ctx context.Context, key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return r.C.Set(ctx, key, b, ttl).Err()
}

func (r *Redis) Delete(ctx context.Context, key string) error { return r.C.Del(ctx, key).Err() }

// Ping verifies connectivity.
func (r *Redis) Ping(ctx context.Context) error { return r.C.Ping(ctx).Err() }

// Memory is an in-process Store.
type Memory struct {
	mu  sync.Mutex
	m   map[string]memEntry
	Now func() time.Time
}

type memEntry struct {
	b   []byte
	exp time.Time
}

func NewMemory() *Memory { return &Memory{m: map[string]memEntry{}, Now: time.Now} }

func (m *Memory) Get(_ context.Context, key string, v any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.m[key]
	if !ok || (!e.exp.IsZero() && !m.Now().Before(e.exp)) {
		delete(m.m, key)
		return false, nil
	}
	return true, json.Unmarshal(e.b, v)
}

func (m *Memory) Set(_ context.Context, key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e := memEntry{b: b}
	if ttl > 0 {
		e.exp = m.Now().Add(ttl)
	}
	m.mu.Lock()
	m.m[key] = e
	m.mu.Unlock()
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.m, key)
	m.mu.Unlock()
	return nil
}
