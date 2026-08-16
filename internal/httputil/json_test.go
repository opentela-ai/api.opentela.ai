package httputil

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeStrictAcceptsValidObject(t *testing.T) {
	var got map[string]any
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":"b","n":3}`))
	rec := httptest.NewRecorder()
	if err := DecodeStrict(rec, req, 1<<20, &got); err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	if got["a"] != "b" || got["n"] != 3.0 {
		t.Fatalf("decoded=%v, want {a:b n:3}", got)
	}
}

func TestDecodeStrictRejectsUnknownFields(t *testing.T) {
	var got struct{ A string }
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"A":"x","extra":1}`))
	rec := httptest.NewRecorder()
	if err := DecodeStrict(rec, req, 1<<20, &got); err == nil {
		t.Fatal("DecodeStrict accepted unknown field, want error")
	}
}

func TestDecodeStrictRejectsTrailingSecondValue(t *testing.T) {
	// The trailing {} decodes cleanly into the empty sentinel struct, so the
	// rejection must come from DecodeStrict's explicit "multiple values" check
	// rather than from an unknown-field error.
	var got struct{ A string }
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"A":"x"}{}`))
	rec := httptest.NewRecorder()
	err := DecodeStrict(rec, req, 1<<20, &got)
	if err == nil {
		t.Fatal("DecodeStrict accepted two JSON values, want error")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("err=%q, want it to mention multiple values", err.Error())
	}
}

func TestDecodeStrictRejectsTrailingGarbage(t *testing.T) {
	var got struct{ A string }
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"A":"x"}!`))
	rec := httptest.NewRecorder()
	if err := DecodeStrict(rec, req, 1<<20, &got); err == nil {
		t.Fatal("DecodeStrict accepted trailing garbage, want error")
	}
}

func TestDecodeStrictEmptyBodyIsEOF(t *testing.T) {
	var got struct{ A string }
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	rec := httptest.NewRecorder()
	if err := DecodeStrict(rec, req, 1<<20, &got); !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v, want io.EOF", err)
	}
}

func TestDecodeStrictEnforcesMaxBytes(t *testing.T) {
	// A valid but oversized object: the failure must be the size limit, not
	// a JSON syntax error, so the contract (request bodies are bounded) is
	// exercised in isolation.
	body := `{"A":"` + strings.Repeat("x", 200) + `"}`
	var got struct{ A string }
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	err := DecodeStrict(rec, req, 16, &got)
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		t.Fatalf("err=%v, want *http.MaxBytesError", err)
	}
	if got.A != "" {
		t.Fatalf("dst populated despite size limit: A=%q", got.A)
	}
}

func TestWriteJSONSetsContentTypeStatusAndBody(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]string{"ok": "true"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", ct)
	}
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if out["ok"] != "true" {
		t.Fatalf("body=%v, want {ok:true}", out)
	}
}
