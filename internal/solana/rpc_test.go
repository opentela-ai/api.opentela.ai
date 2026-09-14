package solana

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTokenDelegationParses covers the jsonParsed token-account shapes the
// allowance poller depends on: delegate set, delegate absent, delegated
// amount zero, and a missing account.
func TestTokenDelegationParses(t *testing.T) {
	const ata = "4Nd1mBQTrMJkzeVSVBarg7VxPoRhVW51qVcuZQau9Jdo"

	respond := func(t *testing.T, body string) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	}

	t.Run("delegate set with amount", func(t *testing.T) {
		srv := respond(t, `{
			"jsonrpc":"2.0","result":{
				"context":{"slot":12345},
				"value":{
					"data":{
						"program":"spl-token","space":165,
						"parsed":{"type":"account","info":{
							"mint":"MintSoMePubkey","owner":"OwnerPubkey",
							"delegate":"Deleg8tePubkey111111111111111111111111",
							"delegatedAmount":{"amount":"4750000","decimals":6,"uiAmount":4.75,"uiAmountString":"4.75"},
							"tokenAmount":{"amount":"10000000","decimals":6,"uiAmount":10.0,"uiAmountString":"10"}
						}}
					},"executable":false,"lamports":2039280,"owner":"TokenProgramPubkey"
				}
			},"id":1}`)
		defer srv.Close()

		c := NewRPCClient(srv.URL)
		td, ok, err := c.TokenDelegation(context.Background(), ata)
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if td.Delegate != "Deleg8tePubkey111111111111111111111111" || td.DelegatedAmountRaw != 4750000 {
			t.Fatalf("delegate = %+v", td)
		}
		if td.Slot != 12345 {
			t.Fatalf("slot = %d", td.Slot)
		}
	})

	t.Run("no delegate", func(t *testing.T) {
		srv := respond(t, `{
			"jsonrpc":"2.0","result":{
				"context":{"slot":9},
				"value":{"data":{"parsed":{"type":"account","info":{
					"mint":"M","owner":"O",
					"tokenAmount":{"amount":"5","decimals":6,"uiAmount":0.000005,"uiAmountString":"0.000005"}
				}}},"executable":false,"lamports":1,"owner":"T"}
			},"id":1}`)
		defer srv.Close()

		c := NewRPCClient(srv.URL)
		td, ok, err := c.TokenDelegation(context.Background(), ata)
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if td.Delegate != "" || td.DelegatedAmountRaw != 0 {
			t.Fatalf("delegate = %+v", td)
		}
	})

	t.Run("account missing", func(t *testing.T) {
		srv := respond(t, `{"jsonrpc":"2.0","result":{"context":{"slot":10},"value":null},"id":1}`)
		defer srv.Close()

		c := NewRPCClient(srv.URL)
		_, ok, err := c.TokenDelegation(context.Background(), ata)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if ok {
			t.Fatal("missing account must report ok=false")
		}
	})

	t.Run("rpc error", func(t *testing.T) {
		srv := respond(t, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"bad"},"id":1}`)
		defer srv.Close()

		c := NewRPCClient(srv.URL)
		if _, _, err := c.TokenDelegation(context.Background(), ata); err == nil {
			t.Fatal("rpc error must surface")
		}
	})
}
