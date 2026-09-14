package solrpcproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestUpstream(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(respond))
}

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/solana-rpc", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProxyForwardsAllowedMethod(t *testing.T) {
	var gotMethod, gotCT string
	up := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req rpcRequest
		_ = json.Unmarshal(b, &req)
		gotMethod = req.Method
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"context":{"slot":1},"value":true},"id":7}`))
	})
	defer up.Close()
	u, _ := url.Parse(up.URL)
	h := New(u, nil, WithClient(up.Client()))

	rec := post(t, h, `{"jsonrpc":"2.0","id":7,"method":"getLatestBlockhash"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotMethod != "getLatestBlockhash" || !strings.HasPrefix(gotCT, "application/json") {
		t.Fatalf("upstream saw method=%q ct=%q", gotMethod, gotCT)
	}
	var resp struct {
		Result struct {
			Value bool `json:"value"`
		} `json:"result"`
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Result.Value || resp.ID != 7 {
		t.Fatalf("response passthrough broken: %s", rec.Body.String())
	}
}

func TestProxyDeniesDisallowedMethod(t *testing.T) {
	up := newTestUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("upstream must not be reached")
	})
	defer up.Close()
	u, _ := url.Parse(up.URL)
	h := New(u, nil, WithClient(up.Client()))

	for _, method := range []string{"getProgramAccounts", "requestAirdrop", "getLargestAccounts"} {
		rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: code=%d want 403", method, rec.Code)
		}
	}
}

func TestProxyCustomAllowlist(t *testing.T) {
	up := newTestUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":null,"id":1}`))
	})
	defer up.Close()
	u, _ := url.Parse(up.URL)
	// Custom allowlist REPLACES the defaults: getHealth passes, the default
	// methods do not.
	h := New(u, []string{"getHealth"}, WithClient(up.Client()))

	if rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"getHealth"}`); rec.Code != http.StatusOK {
		t.Fatalf("getHealth: code=%d", rec.Code)
	}
	if rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"getBalance"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("getBalance outside custom list: code=%d want 403", rec.Code)
	}
}

func TestProxyBatchAllMethodsMustBeAllowed(t *testing.T) {
	up := newTestUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"jsonrpc":"2.0","result":1,"id":1},{"jsonrpc":"2.0","result":2,"id":2}]`))
	})
	defer up.Close()
	u, _ := url.Parse(up.URL)
	h := New(u, nil, WithClient(up.Client()))

	okBatch := `[
		{"jsonrpc":"2.0","id":1,"method":"getHealth"},
		{"jsonrpc":"2.0","id":2,"method":"getSlot"}
	]`
	if rec := post(t, h, okBatch); rec.Code != http.StatusOK {
		t.Fatalf("allowed batch: code=%d body=%s", rec.Code, rec.Body.String())
	}

	mixed := `[
		{"jsonrpc":"2.0","id":1,"method":"getHealth"},
		{"jsonrpc":"2.0","id":2,"method":"requestAirdrop"}
	]`
	if rec := post(t, h, mixed); rec.Code != http.StatusForbidden {
		t.Fatalf("mixed batch with denied method: code=%d want 403", rec.Code)
	}

	tooBig := "[" + strings.TrimSuffix(strings.Repeat(`{"jsonrpc":"2.0","id":1,"method":"getHealth"},`, maxBatch+1), ",") + "]"
	if rec := post(t, h, tooBig); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized batch: code=%d want 400", rec.Code)
	}
}

func TestProxyRejectsBadRequests(t *testing.T) {
	up := newTestUpstream(t, func(w http.ResponseWriter, _ *http.Request) { t.Fatal("must not reach upstream") })
	defer up.Close()
	u, _ := url.Parse(up.URL)
	h := New(u, nil, WithClient(up.Client()))

	if rec := post(t, h, "not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage body: code=%d", rec.Code)
	}
	// GET is not the RPC verb.
	req := httptest.NewRequest(http.MethodGet, "/solana-rpc", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: code=%d want 405", rec.Code)
	}
}

func TestProxyUpstreamFailureIs502(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	u, _ := url.Parse(up.URL)
	up.Close() // dead upstream
	h := New(u, nil)
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"getHealth"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("dead upstream: code=%d want 502", rec.Code)
	}
}

func TestProxyRateLimitPerIP(t *testing.T) {
	up := newTestUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	defer up.Close()
	u, _ := url.Parse(up.URL)
	h := New(u, nil, WithClient(up.Client()), WithRateLimit(1000, 3))
	base := time.Now()
	h.now = func() time.Time { return base } // frozen clock: no refill
	makeReq := func(ip string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/solana-rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"getHealth"}`))
		req.Header.Set("CF-Connecting-IP", ip)
		return req
	}
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, makeReq("1.2.3.4"))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: code=%d want 200 (burst=3)", i+1, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, makeReq("1.2.3.4"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th call: code=%d want 429", rec.Code)
	}
	// A different IP is unaffected.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, makeReq("5.6.7.8"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("other IP: code=%d want 200", rec2.Code)
	}
}

func TestLimiterRefillsOverTime(t *testing.T) {
	l := newRateLimiter(1, 1)
	now := time.Now()
	if !l.allow("ip", now) {
		t.Fatal("first call must pass")
	}
	if l.allow("ip", now) {
		t.Fatal("empty bucket must deny")
	}
	if !l.allow("ip", now.Add(time.Second)) {
		t.Fatal("1s at 1rps must refill one token")
	}
}
