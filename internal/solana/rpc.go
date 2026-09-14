package solana

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// RPCClient is a minimal Solana JSON-RPC client covering the methods needed
// by the billing deposit watcher: getSignaturesForAddress, getTransaction
// (jsonParsed, for sender attribution via preTokenBalances), and
// getSignatureStatuses (to confirm finalization before crediting). The
// faucet retains its own blockhash/send client; transfer construction is
// extracted in a later step.
type RPCClient struct {
	endpoint string
	http     *http.Client
}

// RPCOption configures an RPCClient.
type RPCOption func(*RPCClient)

// WithRPCTimeout sets the per-request HTTP timeout (default 15s).
func WithRPCTimeout(d time.Duration) RPCOption {
	return func(c *RPCClient) { c.http.Timeout = d }
}

// NewRPCClient builds an RPCClient for endpoint (a Solana JSON-RPC URL).
func NewRPCClient(endpoint string, opts ...RPCOption) *RPCClient {
	c := &RPCClient{endpoint: endpoint, http: &http.Client{Timeout: 15 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
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

// call posts a JSON-RPC request and decodes the `result` (or surfaces the
// `error`) into dest. It enforces a 1 MiB response cap so a misbehaving RPC
// cannot exhaust the watcher.
func (c *RPCClient) call(ctx context.Context, method string, params []any, dest any) error {
	payload := rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("solana rpc: marshal %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("solana rpc: build %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("solana rpc: %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("solana rpc: %s status %d", method, resp.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("solana rpc: decode %s: %w", method, err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("solana rpc: %s error (%d): %s", method, envelope.Error.Code, envelope.Error.Message)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, dest); err != nil {
		return fmt.Errorf("solana rpc: unmarshal %s: %w", method, err)
	}
	return nil
}

// SignatureInfo is one entry from getSignaturesForAddress: a confirmed
// transaction signature, its slot, and any error. The watcher skips entries
// with a non-nil Err (failed transactions never move tokens).
type SignatureInfo struct {
	Signature          string           `json:"signature"`
	Slot               int64            `json:"slot"`
	Err                *json.RawMessage `json:"err"`
	ConfirmationStatus string           `json:"confirmationStatus"`
}

// GetSignaturesForAddress lists transaction signatures involving address,
// newest-first. limit caps the page (max 1000); before is the last signature
// of the previous page (empty for the first page). commitment is "confirmed"
// or "finalized" (empty defaults to confirmed). The deposit watcher uses
// "finalized" so every returned signature is safe to credit without a
// separate getSignatureStatuses round-trip.
func (c *RPCClient) GetSignaturesForAddress(ctx context.Context, address string, limit int, before, commitment string) ([]SignatureInfo, error) {
	if commitment == "" {
		commitment = "confirmed"
	}
	opts := map[string]any{"commitment": commitment}
	if limit > 0 {
		opts["limit"] = limit
	}
	if before != "" {
		opts["before"] = before
	}
	var out []SignatureInfo
	if err := c.call(ctx, "getSignaturesForAddress", []any{address, opts}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SignatureStatusValue is the per-signature result from getSignatureStatuses.
// A nil value means the signature is unknown to the RPC (not yet seen or
// expired). ConfirmationStatus is one of "processed", "confirmed",
// "finalized".
type SignatureStatusValue struct {
	Slot               int64  `json:"slot"`
	Confirmations      *int   `json:"confirmations"`
	Status             txErr  `json:"status"`
	ConfirmationStatus string `json:"confirmationStatus"`
}

type txErr struct {
	Ok  json.RawMessage `json:"Ok"`
	Err json.RawMessage `json:"Err"`
}

// Finalized reports whether the transaction reached finalization (the
// strongest guarantee; deposits credit only after this).
func (s *SignatureStatusValue) Finalized() bool {
	return s != nil && s.ConfirmationStatus == "finalized" && len(s.Status.Err) == 0
}

// SignatureStatusFinalized is a constructor for tests and the withdrawal
// worker's fakes: a transaction that the RPC reports as finalized.
func SignatureStatusFinalized() *SignatureStatusValue {
	return &SignatureStatusValue{ConfirmationStatus: "finalized"}
}

// SignatureStatusFailed is a constructor for tests and the withdrawal
// worker's fakes: a transaction the chain rejected with err (a non-null
// JSON object such as `{"Err":"InsufficientFunds"}`).
func SignatureStatusFailed(err json.RawMessage) *SignatureStatusValue {
	return &SignatureStatusValue{ConfirmationStatus: "confirmed", Status: txErr{Err: err}}
}

// GetSignatureStatuses fetches the status of each signature. The result list
// is positional and may contain nil entries; entries[i] is nil when the RPC
// has no information about signatures[i].
func (c *RPCClient) GetSignatureStatuses(ctx context.Context, signatures []string) ([]*SignatureStatusValue, error) {
	if len(signatures) == 0 {
		return nil, nil
	}
	var out []*SignatureStatusValue
	if err := c.call(ctx, "getSignatureStatuses", []any{signatures}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ConfirmedTransaction is the subset of a getTransaction (jsonParsed) response
// the deposit watcher needs: the slot, message account keys, parsed
// instructions, and pre/post token balances (for sender attribution). Only
// SPL Token "transfer" instructions are consumed; everything else is ignored.
//
// The exported sub-types (ParsedMessage, ParsedIx, ParsedInfo, ParsedMeta,
// TokenBalance, LoadedAddresses) mirror the Solana jsonParsed shape so callers
// and tests can construct responses without reaching into unexported fields.
type ConfirmedTransaction struct {
	Slot        int64         `json:"slot"`
	Transaction ParsedMessage `json:"transaction"`
	Meta        *ParsedMeta   `json:"meta"`
}

// ParsedMessage is the transaction.message portion of a jsonParsed
// getTransaction response. AccountKeys are the static signing/non-signing
// keys; versioned transactions append loaded addresses via Meta.LoadedAddresses.
type ParsedMessage struct {
	AccountKeys  []string   `json:"accountKeys"`
	Instructions []ParsedIx `json:"instructions"`
}

// ParsedIx is the union of a token-program instruction (resolved programId +
// structured Parsed) and a generic instruction (base58 data/accounts). Only
// the token Transfer variant is consumed by the watcher.
type ParsedIx struct {
	ProgramID string      `json:"programId"`
	Parsed    *ParsedInfo `json:"parsed,omitempty"`
	Data      string      `json:"data,omitempty"`
	Accounts  []string    `json:"accounts,omitempty"`
}

// ParsedInfo is the structured payload of a known-program instruction:
// for SPL Token, Type is "transfer"/"mintTo"/... and Info is the raw JSON
// ({source, destination, authority, amount, ...}).
type ParsedInfo struct {
	Type string          `json:"type"`
	Info json.RawMessage `json:"info"`
}

// tokenTransferInfo is the SPL Token "transfer" parsed.info. Amount is the
// integer base-unit count as a decimal string (matches the on-chain u64).
type tokenTransferInfo struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Authority   string `json:"authority"`
	Amount      string `json:"amount"`
}

// tokenTransferCheckedInfo is the SPL Token "transferChecked" parsed.info.
// Wallets and most dApps build transferChecked (it embeds the mint and
// decimals, letting receivers validate the transfer), so the deposit watcher
// must treat it as a first-class inbound transfer. Amount lives in
// tokenAmount.amount as an integer base-unit decimal string.
type tokenTransferCheckedInfo struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Authority   string `json:"authority"`
	Mint        string `json:"mint"`
	TokenAmount struct {
		Amount string `json:"amount"`
	} `json:"tokenAmount"`
}

// ParsedMeta is the transaction.metadata portion needed for attribution.
type ParsedMeta struct {
	// Err is non-null when the transaction failed. Failed transactions never
	// move tokens, but the watcher records them as skipped to avoid re-querying.
	Err              *json.RawMessage `json:"err"`
	PreTokenBalances []TokenBalance   `json:"preTokenBalances"`
	LoadedAddresses  LoadedAddresses  `json:"loadedAddresses"`
}

// TokenBalance is one entry from meta.preTokenBalances / postTokenBalances:
// the owner (wallet) of the token account at AccountIndex, for the given Mint.
type TokenBalance struct {
	AccountIndex int    `json:"accountIndex"`
	Mint         string `json:"mint"`
	Owner        string `json:"owner"`
}

// LoadedAddresses is the additional account keys a versioned transaction
// loads via an address lookup table, in order (writable before readonly).
type LoadedAddresses struct {
	Writable []string `json:"writable"`
	Readonly []string `json:"readonly"`
}

// fullAccountKeys returns the complete account key list: static keys followed
// by loaded writable and readonly addresses (versioned transactions). For
// legacy transactions LoadedAddresses is empty and this equals AccountKeys.
func (t *ConfirmedTransaction) fullAccountKeys() []string {
	if t == nil {
		return nil
	}
	keys := make([]string, 0, len(t.Transaction.AccountKeys)+
		len(t.Meta.LoadedAddresses.Writable)+len(t.Meta.LoadedAddresses.Readonly))
	keys = append(keys, t.Transaction.AccountKeys...)
	if t.Meta != nil {
		keys = append(keys, t.Meta.LoadedAddresses.Writable...)
		keys = append(keys, t.Meta.LoadedAddresses.Readonly...)
	}
	return keys
}

// TokenTransfer is one SPL Token "transfer" instruction extracted from a
// confirmed transaction. AmountRaw is the integer amount in the mint's base
// units. InstructionIndex is the position within the message (stable, used as
// the deposit_events primary-key suffix so a single transaction with several
// inbound transfers yields distinct rows).
type TokenTransfer struct {
	InstructionIndex int
	Source           string
	Destination      string
	Authority        string
	AmountRaw        int64
}

// TokenTransfers returns every SPL Token "transfer" or "transferChecked"
// instruction for the given mint and tokenProgram. An empty mint or
// tokenProgram skips that filter. transferChecked carries the mint on the
// instruction itself and is skipped when it does not match the mint filter;
// plain transfers rely on the caller's pre-balance verification (the watcher
// confirms the destination ATA and mint via the configured treasury/mint).
// Non-token and non-transfer instructions are ignored. Transfers with an
// invalid (non-integer) amount are skipped, as are transfers with an empty
// source or destination.
func (t *ConfirmedTransaction) TokenTransfers(mint, tokenProgram string) []TokenTransfer {
	if t == nil {
		return nil
	}
	var out []TokenTransfer
	for i, ix := range t.Transaction.Instructions {
		if ix.Parsed == nil {
			continue
		}
		if tokenProgram != "" && ix.ProgramID != tokenProgram {
			continue
		}
		var info tokenTransferInfo
		switch ix.Parsed.Type {
		case "transfer":
			if err := json.Unmarshal(ix.Parsed.Info, &info); err != nil {
				continue
			}
		case "transferChecked":
			var checked tokenTransferCheckedInfo
			if err := json.Unmarshal(ix.Parsed.Info, &checked); err != nil {
				continue
			}
			// transferChecked names its mint; honor the mint filter directly.
			if mint != "" && checked.Mint != mint {
				continue
			}
			info = tokenTransferInfo{
				Source:      checked.Source,
				Destination: checked.Destination,
				Authority:   checked.Authority,
				Amount:      checked.TokenAmount.Amount,
			}
		default:
			continue
		}
		if info.Source == "" || info.Destination == "" {
			continue
		}
		amount, err := strconv.ParseInt(info.Amount, 10, 64)
		if err != nil || amount <= 0 {
			continue
		}
		_ = mint // mint is verified indirectly via the preTokenBalances owner
		out = append(out, TokenTransfer{
			InstructionIndex: i,
			Source:           info.Source,
			Destination:      info.Destination,
			Authority:        info.Authority,
			AmountRaw:        amount,
		})
	}
	return out
}

// SenderWallet resolves the wallet that owned the source token account at the
// start of the transaction (meta.preTokenBalances). It maps the source ATA's
// pubkey to its account index in the full key list, then reads that index's
// owner from preTokenBalances. When the source had no prior balance record
// (e.g. a wrap-then-transfer), it falls back to the instruction authority —
// the signer that authorized the transfer — which is the common-case sender.
func (t *ConfirmedTransaction) SenderWallet(sourceATA, fallbackAuthority string) string {
	if t == nil {
		return fallbackAuthority
	}
	keys := t.fullAccountKeys()
	idx := -1
	for i, k := range keys {
		if k == sourceATA {
			idx = i
			break
		}
	}
	if idx >= 0 && t.Meta != nil {
		for _, b := range t.Meta.PreTokenBalances {
			if b.AccountIndex == idx && b.Owner != "" {
				return b.Owner
			}
		}
	}
	return fallbackAuthority
}

// GetTransaction fetches the full transaction at signature with jsonParsed
// encoding and maxSupportedTransactionVersion 0 (so both legacy and versioned
// transactions decode uniformly).
func (c *RPCClient) GetTransaction(ctx context.Context, signature string) (*ConfirmedTransaction, error) {
	var out ConfirmedTransaction
	if err := c.call(ctx, "getTransaction", []any{
		signature,
		map[string]any{
			"encoding":                       "jsonParsed",
			"maxSupportedTransactionVersion": 0,
			"commitment":                     "confirmed",
		},
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LatestBlockhash is the reusable form of the faucet's getLatestBlockhash. It
// returns the current blockhash and the last block height at which it remains
// valid. Withdrawals (Step 7) use this to sign and to detect expiry.
func (c *RPCClient) LatestBlockhash(ctx context.Context) ([]byte, uint64, error) {
	var out blockhashResponse
	if err := c.call(ctx, "getLatestBlockhash", []any{map[string]string{"commitment": "confirmed"}}, &out); err != nil {
		return nil, 0, err
	}
	if out.Error != nil {
		return nil, 0, fmt.Errorf("solana rpc: getLatestBlockhash error (%d): %s", out.Error.Code, out.Error.Message)
	}
	bh, err := DecodeBase58(out.Result.Value.Blockhash, PublicKeyBytes)
	if err != nil || len(bh) != PublicKeyBytes {
		return nil, 0, fmt.Errorf("solana rpc: invalid blockhash from rpc")
	}
	return bh, out.Result.Value.LastValidBlockHeight, nil
}

type blockhashResponse struct {
	Result struct {
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
		} `json:"value"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

// GetBlockHeight returns the node's current block height at confirmed
// commitment. The withdrawal worker uses it to prove a transaction's last
// valid block height has passed before restoring reserved credit.
func (c *RPCClient) GetBlockHeight(ctx context.Context) (uint64, error) {
	var out blockHeightResponse
	if err := c.call(ctx, "getBlockHeight", []any{map[string]string{"commitment": "confirmed"}}, &out); err != nil {
		return 0, err
	}
	if out.Error != nil {
		return 0, fmt.Errorf("solana rpc: getBlockHeight error (%d): %s", out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

type blockHeightResponse struct {
	Result uint64    `json:"result"`
	Error  *rpcError `json:"error"`
}

// accountInfoResponse matches getAccountInfo with a zero-length dataSlice: only
// the presence of a non-null value matters, so the RPC never encodes the
// (potentially >128-byte) account data that base58 encoding cannot represent.
type accountInfoResponse struct {
	Result struct {
		Value *json.RawMessage `json:"value"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

// AccountExists reports whether an on-chain account exists at pubkey. The
// withdrawal worker uses it to decide whether to include a Create-ATA
// instruction before the SPL transfer (a Create against an existing account
// fails the whole transaction, so the existence check must come first).
func (c *RPCClient) AccountExists(ctx context.Context, pubkey string) (bool, error) {
	if _, err := DecodeBase58(pubkey, PublicKeyBytes); err != nil {
		return false, fmt.Errorf("solana rpc: account exists: %w", err)
	}
	var out accountInfoResponse
	if err := c.call(ctx, "getAccountInfo", []any{
		pubkey,
		map[string]any{"encoding": "base58", "dataSlice": map[string]int{"offset": 0, "length": 0}, "commitment": "confirmed"},
	}, &out); err != nil {
		return false, err
	}
	if out.Error != nil {
		return false, fmt.Errorf("solana rpc: getAccountInfo error (%d): %s", out.Error.Code, out.Error.Message)
	}
	return out.Result.Value != nil && string(*out.Result.Value) != "null", nil
}

// TokenDelegate is the delegation state of a parsed SPL token account.
type TokenDelegate struct {
	// Delegate is the base58 pubkey authorized to spend, empty when none.
	Delegate string
	// DelegatedAmountRaw is the remaining delegated amount in raw smallest
	// units, 0 when no delegation exists.
	DelegatedAmountRaw uint64
	// Slot is the chain slot the read was served at — the observation ref's
	// uniqueness component for the allowance registry.
	Slot uint64
}

type parsedTokenAccount struct {
	Data struct {
		Parsed struct {
			Info struct {
				Delegate        *string `json:"delegate"`
				DelegatedAmount *struct {
					Amount string `json:"amount"`
				} `json:"delegatedAmount"`
			} `json:"info"`
		} `json:"parsed"`
	} `json:"data"`
}

// TokenDelegation reads the delegation state of the token account at ata
// using getAccountInfo with jsonParsed encoding. ok=false when the account
// does not exist (no ATA yet — no delegation possible).
func (c *RPCClient) TokenDelegation(ctx context.Context, ata string) (TokenDelegate, bool, error) {
	if _, err := DecodeBase58(ata, PublicKeyBytes); err != nil {
		return TokenDelegate{}, false, fmt.Errorf("solana rpc: token delegation: %w", err)
	}
	// call() strips the RPC envelope and decodes the `result` member, so dest
	// is the getAccountInfo result itself ({context, value}). A null result
	// (missing account) leaves Value nil.
	var out struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value *parsedTokenAccount `json:"value"`
	}
	if err := c.call(ctx, "getAccountInfo", []any{
		ata,
		map[string]any{"encoding": "jsonParsed", "commitment": "confirmed"},
	}, &out); err != nil {
		return TokenDelegate{}, false, err
	}
	if out.Value == nil {
		return TokenDelegate{}, false, nil
	}
	var td TokenDelegate
	td.Slot = out.Context.Slot
	info := out.Value.Data.Parsed.Info
	if info.Delegate != nil {
		td.Delegate = *info.Delegate
	}
	if info.DelegatedAmount != nil {
		n, err := strconv.ParseUint(info.DelegatedAmount.Amount, 10, 64)
		if err != nil {
			return TokenDelegate{}, true, fmt.Errorf("solana rpc: token delegation: bad delegatedAmount %q", info.DelegatedAmount.Amount)
		}
		td.DelegatedAmountRaw = n
	}
	return td, true, nil
}

// SendTransaction broadcasts a fully signed wire-format transaction and
// returns its signature. Base64 is the modern default (avoids edge cases with
// large base58-encoded transactions on some providers).
func (c *RPCClient) SendTransaction(ctx context.Context, wire []byte) (string, error) {
	var out sendTransactionResponse
	if err := c.call(ctx, "sendTransaction", []any{
		base64.StdEncoding.EncodeToString(wire),
		map[string]any{"encoding": "base64", "preflightCommitment": "confirmed"},
	}, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("solana rpc: sendTransaction error (%d): %s", out.Error.Code, out.Error.Message)
	}
	if out.Result == "" {
		return "", fmt.Errorf("solana rpc: sendTransaction returned an empty signature")
	}
	return out.Result, nil
}

type sendTransactionResponse struct {
	Result string    `json:"result"`
	Error  *rpcError `json:"error"`
}
