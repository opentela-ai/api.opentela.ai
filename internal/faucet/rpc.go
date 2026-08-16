package faucet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/opentela-ai/api/internal/solana"
)

// rpcClient is a minimal Solana JSON-RPC client covering exactly what the
// faucet needs: latest blockhash, account existence, and transaction send.
type rpcClient struct {
	endpoint string
	http     *http.Client
}

func newRPCClient(endpoint string) *rpcClient {
	return &rpcClient{
		endpoint: endpoint,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *rpcClient) call(ctx context.Context, method string, params []any, dest any) error {
	payload := rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("faucet: marshal rpc request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("faucet: build rpc request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("faucet: solana rpc request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("faucet: solana rpc status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dest); err != nil {
		return fmt.Errorf("faucet: decode solana rpc response: %w", err)
	}
	return nil
}

// blockhashResponse matches getLatestBlockhash.
type blockhashResponse struct {
	Result struct {
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
		} `json:"value"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

// latestBlockhash returns the current blockhash as raw 32 bytes.
func (c *rpcClient) latestBlockhash(ctx context.Context) ([]byte, error) {
	var out blockhashResponse
	if err := c.call(ctx, "getLatestBlockhash", []any{map[string]string{"commitment": "confirmed"}}, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("faucet: solana rpc error (%d): %s", out.Error.Code, out.Error.Message)
	}
	bh, err := solana.DecodeBase58(out.Result.Value.Blockhash, 32)
	if err != nil || len(bh) != 32 {
		return nil, fmt.Errorf("faucet: invalid blockhash from rpc")
	}
	return bh, nil
}

// accountResponse matches getAccountInfo (base58 encoding, no data needed).
type accountResponse struct {
	Result struct {
		Value *struct {
			Data []any `json:"data"`
		} `json:"value"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

// accountExists reports whether an account exists at pubkey. It uses
// dataSlice with length 0 so the RPC never encodes the account data (base58
// encoding fails for accounts > 128 bytes, which includes every token
// account); only the presence of a non-null value matters.
func (c *rpcClient) accountExists(ctx context.Context, pubkey []byte) (bool, error) {
	var out accountResponse
	if err := c.call(ctx, "getAccountInfo", []any{
		solana.EncodeBase58(pubkey),
		map[string]any{"encoding": "base58", "dataSlice": map[string]int{"offset": 0, "length": 0}, "commitment": "confirmed"},
	}, &out); err != nil {
		return false, err
	}
	if out.Error != nil {
		return false, fmt.Errorf("faucet: solana rpc error (%d): %s", out.Error.Code, out.Error.Message)
	}
	return out.Result.Value != nil, nil
}

// sendTransactionResponse matches sendTransaction.
type sendTransactionResponse struct {
	Result string    `json:"result"`
	Error  *rpcError `json:"error"`
}

// sendTransaction broadcasts a fully signed wire-format transaction and
// returns its signature. Base64 is the modern default (avoids edge cases with
// large base58-encoded transactions on some providers).
func (c *rpcClient) sendTransaction(ctx context.Context, wire []byte) (string, error) {
	var out sendTransactionResponse
	if err := c.call(ctx, "sendTransaction", []any{
		base64.StdEncoding.EncodeToString(wire),
		map[string]any{"encoding": "base64", "preflightCommitment": "confirmed"},
	}, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("faucet: solana rpc error (%d): %s", out.Error.Code, out.Error.Message)
	}
	if out.Result == "" {
		return "", fmt.Errorf("faucet: solana rpc returned an empty signature")
	}
	return out.Result, nil
}
