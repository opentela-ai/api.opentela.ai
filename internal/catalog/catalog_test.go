package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/store"
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

type policyStoreStub struct {
	managed []store.InstanceInfo
	err     error
}

func (p policyStoreStub) ListManagedInstancesByPeerIDs(context.Context, []string) ([]store.InstanceInfo, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.managed, nil
}

func TestHandlerFiltersManagedMixedPeerToPermissionlessBindingsOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "peer-managed": {"connected": true, "service": [{"name":"embeddings-public","identity_group":["all"]},{"name":"llm-private","identity_group":["model=Qwen"]}]},
		  "peer-unmanaged": {"connected": true, "service": [{"name":"llm-private","identity_group":["model=Qwen"]}]}
		}`))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	h := NewWithPolicies(target, time.Minute, policyStoreStub{managed: []store.InstanceInfo{{
		PeerID:      "peer-managed",
		PolicyScope: store.PolicyScopeService,
		Services: []store.InstanceService{
			{ServiceName: "embeddings-public", Exposure: store.ExposurePermissionless},
			{ServiceName: "llm-private", Exposure: store.ExposureTrustedRegion},
		},
	}}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var body Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Services) != 2 {
		t.Fatalf("services=%+v, want unmanaged llm-private and managed embeddings-public", body.Services)
	}
	if body.Services[0].Name != "embeddings-public" || body.Services[1].Name != "llm-private" {
		t.Fatalf("services=%+v", body.Services)
	}
	if body.Services[1].Providers != 1 {
		t.Fatalf("llm-private providers=%d, want only unmanaged provider", body.Services[1].Providers)
	}
}

func TestHandlerFailsClosedWhenPolicyLookupFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tableJSON))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	rec := httptest.NewRecorder()
	NewWithPolicies(target, time.Minute, policyStoreStub{err: errors.New("db down")}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503", rec.Code)
	}
}

// GET /v1/service/{service}/v1/models reshapes the catalogue into the
// OpenAI list contract. The table offers two models on the "llm" service;
// both must appear, deduped and sorted, with the OpenAI object/owner shape.
func TestServeModelsOpenAIShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tableJSON))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	h := New(target, time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/v1/service/llm/v1/models", nil)
	req.SetPathValue("service", "llm")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type=%q, want application/json", ct)
	}
	var got ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if got.Object != "list" || len(got.Data) != 2 {
		t.Fatalf("got=%+v, want object=list data=2", got)
	}
	if got.Data[0].ID != "Llama-3" || got.Data[1].ID != "Qwen/Qwen3-8B" {
		t.Fatalf("model ids=%q %q, want Llama-3 Qwen/Qwen3-8B", got.Data[0].ID, got.Data[1].ID)
	}
	for _, m := range got.Data {
		if m.Object != "model" {
			t.Fatalf("model %q object=%q, want model", m.ID, m.Object)
		}
		if m.OwnedBy != "opentela" {
			t.Fatalf("model %q owned_by=%q, want opentela", m.ID, m.OwnedBy)
		}
		if m.Created < 0 {
			t.Fatalf("model %q created=%d, want >=0", m.ID, m.Created)
		}
	}
}

// A service with no models (a catch-all sandbox) yields an empty list, still
// 200 — the OpenAI model-list contract is a list, never a 404.
func TestServeModelsCatchAllIsEmpty(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tableJSON))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	h := New(target, time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/v1/service/flash-sandbox/v1/models", nil)
	req.SetPathValue("service", "flash-sandbox")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	var got ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "list" || len(got.Data) != 0 {
		t.Fatalf("got=%+v, want empty list", got)
	}
}

// An unknown service also yields an empty list (still 200), not a 404.
func TestServeModelsUnknownServiceIsEmpty(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tableJSON))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	h := New(target, time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/v1/service/does-not-exist/v1/models", nil)
	req.SetPathValue("service", "does-not-exist")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	var got ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data) != 0 {
		t.Fatalf("got=%+v, want empty list for unknown service", got)
	}
}

// The catalogue route (no {service} path value) is unaffected: it still
// returns the full distilled Response, not the model list.
func TestServeCatalogueStillServedWithoutServicePathValue(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tableJSON))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	h := New(target, time.Minute)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	var got Response
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Services) != 2 {
		t.Fatalf("services=%d, want full catalogue (2 services)", len(got.Services))
	}
}

// A model-list request hits the same upstream cache as the catalogue, so an
// anonymous flood of GET .../v1/models cannot hammer the node either.
func TestServeModelsSharesUpstreamCache(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(tableJSON))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	h := New(target, time.Minute)

	for range 3 {
		req := httptest.NewRequest(http.MethodGet, "/v1/service/llm/v1/models", nil)
		req.SetPathValue("service", "llm")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d, want 200", rec.Code)
		}
	}
	if hits != 1 {
		t.Fatalf("upstream hits=%d, want 1 (cached across both routes)", hits)
	}
}
