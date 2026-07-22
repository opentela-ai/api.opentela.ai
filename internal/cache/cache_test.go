package cache

import (
	"sync"
	"testing"
	"time"
)

func TestSetGetHit(t *testing.T) {
	c := New(0)
	defer c.Close()
	c.Set("k", true, time.Minute)
	got, ok := c.Get("k")
	if !ok || got != true {
		t.Fatalf("Get = (%v,%v), want (true,true)", got, ok)
	}
}

func TestGetMiss(t *testing.T) {
	c := New(0)
	defer c.Close()
	if _, ok := c.Get("absent"); ok {
		t.Fatal("Get(absent) ok = true, want false")
	}
}

func TestNegativeValueCached(t *testing.T) {
	c := New(0)
	defer c.Close()
	c.Set("bad", false, time.Minute)
	got, ok := c.Get("bad")
	if !ok || got != false {
		t.Fatalf("Get = (%v,%v), want (false,true)", got, ok)
	}
}

func TestExpiryIsMiss(t *testing.T) {
	c := New(0)
	defer c.Close()
	base := time.Unix(1000, 0)
	c.now = func() time.Time { return base }
	c.Set("k", true, time.Minute)
	// Advance the clock past the TTL.
	c.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, ok := c.Get("k"); ok {
		t.Fatal("expired entry returned ok = true, want false")
	}
}

func TestDeleteExpiredEvicts(t *testing.T) {
	c := New(0)
	defer c.Close()
	base := time.Unix(1000, 0)
	c.now = func() time.Time { return base }
	c.Set("k", true, time.Minute)
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	c.now = func() time.Time { return base.Add(2 * time.Minute) }
	c.deleteExpired()
	if c.Len() != 0 {
		t.Fatalf("Len after deleteExpired = %d, want 0", c.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New(0)
	defer c.Close()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := string(rune('a' + n%26))
			c.Set(key, true, time.Minute)
			c.Get(key)
		}(i)
	}
	wg.Wait()
}
