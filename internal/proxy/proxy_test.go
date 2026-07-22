package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProxyForwardsRequestAndResponse(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "pong")
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(New(target))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat?q=1", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotPath != "/v1/chat" || gotQuery != "q=1" {
		t.Errorf("upstream got path=%q query=%q, want /v1/chat q=1", gotPath, gotQuery)
	}
	if gotAuth != "Bearer abc" {
		t.Errorf("upstream Authorization = %q, want Bearer abc", gotAuth)
	}
	if gotBody != "hello" {
		t.Errorf("upstream body = %q, want hello", gotBody)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Errorf("missing upstream response header")
	}
	if string(body) != "pong" {
		t.Errorf("body = %q, want pong", body)
	}
}

func TestProxyStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		<-release // block until the test has read the first chunk
		_, _ = io.WriteString(w, "data: second\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(New(target))
	defer front.Close()

	resp, err := http.Get(front.URL + "/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n') // should arrive before "second" is sent
	if err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if !strings.Contains(line, "first") {
		t.Fatalf("first chunk = %q, want it to contain 'first'", line)
	}
	close(release) // now allow the upstream to send the rest
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "second") {
		t.Fatalf("rest = %q, want it to contain 'second'", rest)
	}
}

func TestProxyUpstreamDownReturns502(t *testing.T) {
	// Point at a closed server to force a dial failure.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target, _ := url.Parse(dead.URL)
	dead.Close()

	front := httptest.NewServer(New(target))
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}
