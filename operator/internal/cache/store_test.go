package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

type item struct{ A int }

func exercise(t *testing.T, s Store, advance func(time.Duration)) {
	ctx := context.Background()
	if ok, err := s.Get(ctx, "k", &item{}); ok || err != nil {
		t.Fatalf("expected miss, got %v %v", ok, err)
	}
	if err := s.Set(ctx, "k", item{7}, time.Minute); err != nil {
		t.Fatal(err)
	}
	var got item
	if ok, err := s.Get(ctx, "k", &got); !ok || err != nil || got.A != 7 {
		t.Fatalf("got %+v %v %v", got, ok, err)
	}
	advance(2 * time.Minute)
	if ok, _ := s.Get(ctx, "k", &got); ok {
		t.Fatal("expected expiry")
	}
	_ = s.Set(ctx, "d", item{1}, 0)
	_ = s.Delete(ctx, "d")
	if ok, _ := s.Get(ctx, "d", &got); ok {
		t.Fatal("expected delete")
	}
}

func TestRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	r := NewRedis(mr.Addr(), "", 0)
	if err := r.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	exercise(t, r, mr.FastForward)
}

func TestMemory(t *testing.T) {
	m := NewMemory()
	now := time.Now()
	m.Now = func() time.Time { return now }
	exercise(t, m, func(d time.Duration) { now = now.Add(d) })
}

func TestRedisDown(t *testing.T) {
	mr := miniredis.RunT(t)
	r := NewRedis(mr.Addr(), "", 0)
	mr.Close()
	if _, err := r.Get(context.Background(), "k", &item{}); err == nil {
		t.Fatal("expected error when redis is down")
	}
}
