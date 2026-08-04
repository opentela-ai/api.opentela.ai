package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GPUInfo describes the hardware attributed to one serving peer.
type GPUInfo struct {
	Model string // normalized name of the first GPU; "" for CPU-only peers
	Count int    // number of GPUs the peer reports
}

// Resolver maps mesh peer ids to GPU inventory via the public node table
// (/v1/dnt/table), cached and refreshed lazily. A nil *Resolver disables
// attribution (Hook tolerates it).
type Resolver struct {
	upstream *url.URL
	ttl      time.Duration
	client   *http.Client

	mu          sync.Mutex
	gpus        map[string]GPUInfo
	fetched     time.Time
	lastAttempt time.Time // bounds refetch rate while the upstream is down
}

// NewResolver resolves peer→GPU against the upstream node table, considering
// the cache fresh for ttl (hardware inventory changes rarely; minutes are
// plenty).
func NewResolver(upstream *url.URL, ttl time.Duration) *Resolver {
	return &Resolver{
		upstream: upstream,
		ttl:      ttl,
		client:   &http.Client{Timeout: 10 * time.Second},
		gpus:     make(map[string]GPUInfo),
	}
}

// retryFloor limits node-table refetches while refreshes keep failing.
const retryFloor = 15 * time.Second

// Resolve returns the GPU inventory of peerID (empty GPUInfo for unknown or
// GPU-less peers). Fetch failures are soft: they yield empty or stale info
// and are retried no faster than retryFloor, mirroring the catalog's "stale
// data over hard errors".
func (r *Resolver) Resolve(ctx context.Context, peerID string) GPUInfo {
	r.mu.Lock()
	due := time.Since(r.fetched) > r.ttl && time.Since(r.lastAttempt) > retryFloor
	if due {
		r.lastAttempt = time.Now()
	}
	r.mu.Unlock()
	if due {
		if err := r.refresh(ctx); err != nil {
			log.Printf("perf: node table refresh failed (GPU attribution degraded): %v", err)
		}
	}
	r.mu.Lock()
	info := r.gpus[peerID]
	r.mu.Unlock()
	return info
}

func (r *Resolver) refresh(ctx context.Context) error {
	u := *r.upstream
	u.Path = "/v1/dnt/table"
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node table status %d", resp.StatusCode)
	}
	var table map[string]struct {
		Hardware struct {
			GPUs []struct {
				Name string `json:"name"`
			} `json:"gpus"`
		} `json:"hardware"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&table); err != nil {
		return fmt.Errorf("decoding node table: %w", err)
	}
	gpus := make(map[string]GPUInfo, len(table))
	for id, entry := range table {
		list := entry.Hardware.GPUs
		info := GPUInfo{Count: len(list)}
		if len(list) > 0 {
			info.Model = normalizeGPUName(list[0].Name)
		}
		gpus[id] = info
	}
	r.mu.Lock()
	r.gpus = gpus
	r.fetched = time.Now()
	r.mu.Unlock()
	return nil
}

// normalizeGPUName canonicalizes driver-reported names ("NVIDIA GeForce RTX
// 4090", "Tesla T4", …) enough to group identical hardware: trimmed,
// whitespace-collapsed. Vendor strings stay intact — they are the leaderboard
// value.
func normalizeGPUName(name string) string {
	return strings.Join(strings.Fields(name), " ")
}
