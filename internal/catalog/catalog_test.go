package catalog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Shaped like the live node table: one peer offering a catch-all sandbox, two
// offering an LLM service with models, and a worker offering nothing.
const tableJSON = `{
  "/QmA": {"connected": true,  "owner": "wallet-pubkey-a", "public_address": "10.0.0.1",
            "service": [{"name": "flash-sandbox", "identity_group": ["all"]}]},
  "/QmB": {"connected": true,  "owner": "wallet-pubkey-b", "public_address": "10.0.0.2",
            "service": [{"name": "llm", "identity_group": ["model=Qwen/Qwen3-8B", "region=eu"]}]},
  "/QmC": {"connected": false, "owner": "wallet-pubkey-c",
            "service": [{"name": "llm", "identity_group": ["model=Llama-3", "model=Qwen/Qwen3-8B"]}]},
  "/QmD": {"connected": true,  "owner": "wallet-pubkey-d", "service": null}
}`

func decodeTable(t *testing.T) map[string]Peer {
	t.Helper()
	var table map[string]Peer
	if err := json.Unmarshal([]byte(tableJSON), &table); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return table
}

func TestSummariseGroupsServices(t *testing.T) {
	got := Summarise(decodeTable(t))

	if len(got) != 2 {
		t.Fatalf("got %d services, want 2 (%+v)", len(got), got)
	}
	if got[0].Name != "flash-sandbox" || got[1].Name != "llm" {
		t.Fatalf("services = %q/%q, want flash-sandbox/llm (sorted)", got[0].Name, got[1].Name)
	}

	llm := got[1]
	if llm.Providers != 2 || llm.Online != 1 {
		t.Fatalf("llm providers=%d online=%d, want 2/1", llm.Providers, llm.Online)
	}
	if strings.Join(llm.Models, ",") != "Llama-3,Qwen/Qwen3-8B" {
		t.Fatalf("llm models = %v, want deduped and sorted", llm.Models)
	}
	if strings.Join(llm.IdentityGroups, ",") != "model=Llama-3,model=Qwen/Qwen3-8B,region=eu" {
		t.Fatalf("llm identity groups = %v", llm.IdentityGroups)
	}
}

// A catch-all service advertises no models; it must still be listed.
func TestSummariseKeepsCatchAllService(t *testing.T) {
	sandbox := Summarise(decodeTable(t))[0]
	if len(sandbox.Models) != 0 {
		t.Fatalf("sandbox models = %v, want none", sandbox.Models)
	}
	if strings.Join(sandbox.IdentityGroups, ",") != "all" {
		t.Fatalf("sandbox identity groups = %v, want [all]", sandbox.IdentityGroups)
	}
	if sandbox.Providers != 1 || sandbox.Online != 1 {
		t.Fatalf("sandbox providers=%d online=%d, want 1/1", sandbox.Providers, sandbox.Online)
	}
}

func TestSummariseEmptyMesh(t *testing.T) {
	if got := Summarise(map[string]Peer{}); len(got) != 0 {
		t.Fatalf("got %+v, want no services", got)
	}
}

// The whole point of distilling: nothing identifying a peer or its operator may
// reach the response.
func TestHandlerLeaksNoPeerDetail(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/dnt/table" {
			t.Errorf("upstream path = %q, want /v1/dnt/table", r.URL.Path)
		}
		_, _ = w.Write([]byte(tableJSON))
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	rec := httptest.NewRecorder()
	New(target, time.Minute).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"wallet-pubkey", "10.0.0.1", "public_address", "hardware", "owner"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked %q: %s", secret, body)
		}
	}

	var got Response
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Services) != 2 {
		t.Fatalf("services = %+v, want 2", got.Services)
	}
}

// A public endpoint must not let callers hammer the node.
func TestHandlerCachesUpstream(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(tableJSON))
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	h := New(target, time.Minute)
	for range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (cached)", hits)
	}
}

func TestHandlerReportsUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	rec := httptest.NewRecorder()
	New(target, time.Minute).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
}
