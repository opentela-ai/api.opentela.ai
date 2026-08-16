package solana

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rpcStub dispatches JSON-RPC methods to canned responses.
type rpcStub struct {
	t        *testing.T
	handlers map[string]func(t *testing.T, r rpcRequest) any
}

func newRPCStub(t *testing.T) *rpcStub {
	return &rpcStub{t: t, handlers: map[string]func(*testing.T, rpcRequest) any{}}
}

func (s *rpcStub) on(method string, fn func(*testing.T, rpcRequest) any) *rpcStub {
	s.handlers[method] = fn
	return s
}

func (s *rpcStub) handler(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(body, &req); err != nil {
		s.t.Fatalf("rpc stub: decode request: %v", err)
	}
	fn, ok := s.handlers[req.Method]
	if !ok {
		s.t.Fatalf("rpc stub: unexpected method %q", req.Method)
	}
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      req.ID,
	}
	out := fn(s.t, req)
	raw, err := json.Marshal(out)
	if err != nil {
		s.t.Fatalf("rpc stub: marshal %s: %v", req.Method, err)
	}
	resp.Result = raw
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func TestRPCGetSignaturesForAddress(t *testing.T) {
	stub := newRPCStub(t).on("getSignaturesForAddress", func(t *testing.T, r rpcRequest) any {
		opts := r.Params[1].(map[string]any)
		if opts["commitment"] != "confirmed" {
			t.Errorf("commitment = %v, want confirmed", opts["commitment"])
		}
		if opts["limit"].(float64) != 2 {
			t.Errorf("limit = %v, want 2", opts["limit"])
		}
		if opts["before"] != "sigA" {
			t.Errorf("before = %v, want sigA", opts["before"])
		}
		return []map[string]any{
			{"signature": "sigB", "slot": float64(10), "err": nil, "confirmationStatus": "finalized"},
			{"signature": "sigC", "slot": float64(9), "err": map[string]any{"InstructionError": [2]any{0, "Custom"}}, "confirmationStatus": "confirmed"},
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	c := NewRPCClient(srv.URL)
	got, err := c.GetSignaturesForAddress(context.Background(), "treasuryATA", 2, "sigA", "confirmed")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Signature != "sigB" || got[1].Signature != "sigC" {
		t.Fatalf("got %+v", got)
	}
	if got[1].Err == nil {
		t.Error("failed transaction should carry a non-nil err")
	}
}

func TestRPCGetSignatureStatusesFinalized(t *testing.T) {
	stub := newRPCStub(t).on("getSignatureStatuses", func(t *testing.T, r rpcRequest) any {
		sigs := r.Params[0].([]any)
		out := make([]*map[string]any, len(sigs))
		out[0] = &map[string]any{"slot": float64(5), "confirmations": nil, "status": map[string]any{"Ok": nil}, "confirmationStatus": "finalized"}
		out[1] = &map[string]any{"slot": float64(5), "status": map[string]any{"Err": "BlockhashNotFound"}, "confirmationStatus": "confirmed"}
		return out
	})
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	c := NewRPCClient(srv.URL)
	got, err := c.GetSignatureStatuses(context.Background(), []string{"sigB", "sigBad"})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Finalized() {
		t.Error("got[0] should be finalized")
	}
	if got[1].Finalized() {
		t.Error("got[1] has Err, should not be finalized")
	}
}

// tokenTxJSON is a realistic jsonParsed getTransaction response: a single SPL
// Token transfer from sourceATA (owner walletA) to destATA (the treasury),
// with preTokenBalances attributing walletA as the owner of the source.
const tokenTxJSON = `{
  "slot": 1234,
  "transaction": {
    "accountKeys": ["walletSigner", "sourceATA", "destATA", "tokenProgram"],
    "instructions": [
      {
        "programId": "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
        "parsed": {
          "type": "transfer",
          "info": {
            "source": "sourceATA",
            "destination": "destATA",
            "authority": "walletSigner",
            "amount": "1000000000"
          }
        }
      },
      {
        "programId": "ComputeBudget111111111111111111111111111111",
        "parsed": {"type": "setComputeUnitPrice", "info": {"microLamports": 1000}}
      }
    ]
  },
  "meta": {
    "err": null,
    "preTokenBalances": [
      {"accountIndex": 1, "mint": "EsmcTrdLkFqV3mv4CjLF3AmCx132ixfFSYYRWD78cDzR", "owner": "walletA"},
      {"accountIndex": 2, "mint": "EsmcTrdLkFqV3mv4CjLF3AmCx132ixfFSYYRWD78cDzR", "owner": "treasuryWallet"}
    ],
    "loadedAddresses": {"writable": [], "readonly": []}
  }
}`

func TestGetTransactionAndTokenTransfers(t *testing.T) {
	stub := newRPCStub(t).on("getTransaction", func(t *testing.T, r rpcRequest) any {
		if r.Params[0] != "sigB" {
			t.Errorf("signature = %v, want sigB", r.Params[0])
		}
		opts := r.Params[1].(map[string]any)
		if opts["encoding"] != "jsonParsed" {
			t.Errorf("encoding = %v, want jsonParsed", opts["encoding"])
		}
		if opts["maxSupportedTransactionVersion"].(float64) != 0 {
			t.Errorf("maxSupportedTransactionVersion = %v, want 0", opts["maxSupportedTransactionVersion"])
		}
		var raw map[string]any
		_ = json.Unmarshal([]byte(tokenTxJSON), &raw)
		return raw
	})
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	c := NewRPCClient(srv.URL)
	tx, err := c.GetTransaction(context.Background(), "sigB")
	if err != nil {
		t.Fatal(err)
	}
	transfers := tx.TokenTransfers("", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
	if len(transfers) != 1 {
		t.Fatalf("transfers = %d, want 1", len(transfers))
	}
	tr := transfers[0]
	if tr.Source != "sourceATA" || tr.Destination != "destATA" || tr.Authority != "walletSigner" || tr.AmountRaw != 1000000000 || tr.InstructionIndex != 0 {
		t.Fatalf("transfer = %+v", tr)
	}
	// Sender attribution resolves the source ATA's owner from preTokenBalances.
	if got := tx.SenderWallet(tr.Source, tr.Authority); got != "walletA" {
		t.Errorf("SenderWallet = %q, want walletA", got)
	}
}

func TestTokenTransfersFiltersProgramAndSkipsNonTransfer(t *testing.T) {
	tx := &ConfirmedTransaction{Transaction: ParsedMessage{AccountKeys: []string{"a", "b", "c", "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"}, Instructions: []ParsedIx{
		{ProgramID: "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb", Parsed: &ParsedInfo{Type: "transfer", Info: json.RawMessage(`{"source":"a","destination":"b","authority":"x","amount":"5"}`)}},
		{ProgramID: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", Parsed: &ParsedInfo{Type: "transfer", Info: json.RawMessage(`{"source":"a","destination":"b","authority":"x","amount":"6"}`)}},
		{ProgramID: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", Parsed: &ParsedInfo{Type: "mintTo", Info: json.RawMessage(`{}`)}},
		{ProgramID: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", Parsed: &ParsedInfo{Type: "transfer", Info: json.RawMessage(`{"source":"a","destination":"b","authority":"x","amount":"-1"}`)}},
	}}}
	got := tx.TokenTransfers("", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
	if len(got) != 1 || got[0].AmountRaw != 6 {
		t.Fatalf("got %+v, want one transfer amount 6 (classic only)", got)
	}
}

func TestSenderWalletFallsBackToAuthority(t *testing.T) {
	// No preTokenBalances for the source: fall back to the instruction
	// authority (the common-case sender for a direct ATA transfer).
	tx := &ConfirmedTransaction{
		Transaction: ParsedMessage{AccountKeys: []string{"s", "d"}},
		Meta:        &ParsedMeta{},
	}
	if got := tx.SenderWallet("s", "walletSigner"); got != "walletSigner" {
		t.Errorf("SenderWallet = %q, want walletSigner", got)
	}
}

func TestRPCCallSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"error": map[string]any{"code": -32000, "message": "node down"},
		})
	}))
	defer srv.Close()
	c := NewRPCClient(srv.URL)
	if _, err := c.GetSignaturesForAddress(context.Background(), "x", 1, "", ""); err == nil || !strings.Contains(err.Error(), "node down") {
		t.Fatalf("err = %v, want node down", err)
	}
}

func TestRPCNewClientDefaultTimeout(t *testing.T) {
	c := NewRPCClient("http://x")
	if c.http.Timeout == 0 {
		t.Error("default timeout should be non-zero")
	}
	c2 := NewRPCClient("http://x", WithRPCTimeout(0))
	if c2.http.Timeout != 0 {
		t.Error("WithRPCTimeout(0) should override to 0")
	}
}
