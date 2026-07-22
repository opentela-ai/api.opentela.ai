package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/store"
)

// fakeStore records how many times Validate is called and returns a scripted result.
type fakeStore struct {
	valid bool
	err   error
	calls int
}

func (f *fakeStore) Validate(_ context.Context, _ string) (bool, error) {
	f.calls++
	return f.valid, f.err
}

func TestValidatorValidCaches(t *testing.T) {
	fs := &fakeStore{valid: true}
	c := cache.New(0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Second)

	for i := 0; i < 3; i++ {
		ok, err := v.Valid(context.Background(), "tok")
		if err != nil || !ok {
			t.Fatalf("Valid = (%v,%v), want (true,nil)", ok, err)
		}
	}
	if fs.calls != 1 {
		t.Fatalf("store called %d times, want 1 (cache should serve the rest)", fs.calls)
	}
}

func TestValidatorInvalidCachedNegatively(t *testing.T) {
	fs := &fakeStore{valid: false}
	c := cache.New(0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Minute)

	ok, err := v.Valid(context.Background(), "bad")
	if err != nil || ok {
		t.Fatalf("Valid = (%v,%v), want (false,nil)", ok, err)
	}
	// Cached negatively → no second store call.
	if _, _ = v.Valid(context.Background(), "bad"); fs.calls != 1 {
		t.Fatalf("store called %d times, want 1", fs.calls)
	}
	// The cached key is the hash, and the value is false.
	if val, hit := c.Get(store.HashKey("bad")); !hit || val {
		t.Fatalf("cache Get = (%v,%v), want (false,true)", val, hit)
	}
}

func TestValidatorStoreErrorNotCached(t *testing.T) {
	fs := &fakeStore{err: errors.New("db down")}
	c := cache.New(0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Minute)

	if _, err := v.Valid(context.Background(), "tok"); err == nil {
		t.Fatal("Valid expected error, got nil")
	}
	if _, hit := c.Get(store.HashKey("tok")); hit {
		t.Fatal("store error must not be cached")
	}
}
