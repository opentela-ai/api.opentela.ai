package billingapi

// Console-managed pricing (per-model buyer caps §5 + seller ask config §6).
// The buyer's per-model cap sheet and the seller's durable ask config are
// both editable with the account JWT — no node-side process required. The
// ask config is republished to the TTL'd market table by the ask refresher
// (internal/askrefresh) while the peer's live mesh observation matches the
// owner; PUT here also attempts an immediate publish so the market moves on
// save.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/pricingapi"
)

// maxModelCaps bounds a per-model cap sheet (the console UI is a table; 128
// distinct models is far past any real deployment).
const maxModelCaps = 128

// maxAskConfig bounds one peer's console-configured asks (mirrors the
// publication contract in pricingapi).
const maxAskConfig = 256

type modelCapsJSON struct {
	Service               string      `json:"service"`
	Model                 string      `json:"model"`
	InputPerMillion       nullableInt `json:"input_per_million"`
	CachedInputPerMillion nullableInt `json:"cached_input_per_million"`
	OutputPerMillion      nullableInt `json:"output_per_million"`
}

type modelCapsSheetRequest struct {
	Models []modelCapsJSON `json:"models"`
}

type askConfigRequest struct {
	Asks []askJSON `json:"asks"`
}

type askJSON struct {
	Service               string `json:"service"`
	Model                 string `json:"model"`
	InputPerMillion       int64  `json:"input_per_million"`
	CachedInputPerMillion int64  `json:"cached_input_per_million"`
	OutputPerMillion      int64  `json:"output_per_million"`
}

type askConfigResponse struct {
	PeerID    string        `json:"peer_id"`
	Asks      []billing.Ask `json:"asks"`
	UpdatedAt *time.Time    `json:"updated_at,omitempty"`
}

type asksConfigListResponse struct {
	Peers []askConfigResponse `json:"peers"`
}

type askConfigPutResponse struct {
	askConfigResponse
	// Published is true when the immediate republish succeeded (peer live,
	// observation matching, advertisement known). False means the config is
	// stored and will be published by the refresher when the peer comes
	// live; PublishError says why this attempt did not land.
	Published    bool    `json:"published"`
	PublishError *string `json:"publish_error,omitempty"`
}

// sellerAskTTLDuration is the market TTL assigned on the console publish
// path — the same server-assigned 5 minutes the nodecred publication uses.
const sellerAskTTLDuration = 5 * time.Minute

// handleModelCapsGet serves GET /manage/billing/preferences/models: the
// account's per-model cap sheet.
func (s *Service) handleModelCapsGet(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ModelCapsForAccount(r.Context(), accountID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, modelCapsSheetRequest{Models: modelCapsToJSON(rows)})
}

// handleModelCapsPut serves PUT /manage/billing/preferences/models: full
// replacement of the per-model cap sheet. Tiers left null inherit the flat
// caps (or unlimited when those are null too).
func (s *Service) handleModelCapsPut(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	if s.mode == config.BillingOff {
		http.Error(w, "billing disabled", http.StatusConflict)
		return
	}
	var req modelCapsSheetRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		return
	}
	if len(req.Models) > maxModelCaps {
		http.Error(w, "too_many_model_caps", http.StatusBadRequest)
		return
	}
	seen := make(map[string]bool, len(req.Models))
	rows := make([]billing.ModelCaps, 0, len(req.Models))
	for _, m := range req.Models {
		m.Service, m.Model = strings.TrimSpace(m.Service), strings.TrimSpace(m.Model)
		if m.Service == "" || m.Model == "" {
			http.Error(w, "empty_service_or_model", http.StatusBadRequest)
			return
		}
		key := m.Service + "\x00" + m.Model
		if seen[key] {
			http.Error(w, "duplicate_model", http.StatusBadRequest)
			return
		}
		seen[key] = true
		rows = append(rows, billing.ModelCaps{
			Service:               m.Service,
			Model:                 m.Model,
			InputPerMillion:       m.InputPerMillion.v,
			CachedInputPerMillion: m.CachedInputPerMillion.v,
			OutputPerMillion:      m.OutputPerMillion.v,
		})
	}
	if err := s.store.ReplaceModelCaps(r.Context(), accountID, rows); err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	saved, err := s.store.ModelCapsForAccount(r.Context(), accountID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, modelCapsSheetRequest{Models: modelCapsToJSON(saved)})
}

// handleAsksConfigList serves GET /manage/billing/asks-config: the durable
// seller ask config for every instance the account owns.
func (s *Service) handleAsksConfigList(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	resp := asksConfigListResponse{Peers: []askConfigResponse{}}
	if s.mode == config.BillingOff {
		httputil.WriteJSON(w, http.StatusOK, resp)
		return
	}
	instances, err := s.store.ListInstancesByUser(r.Context(), accountID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	peerIDs := make([]string, 0, len(instances))
	for _, in := range instances {
		peerIDs = append(peerIDs, in.PeerID)
	}
	cfg, err := s.store.AskConfigForPeers(r.Context(), peerIDs)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, in := range instances {
		asks, ok := cfg[in.PeerID]
		if !ok {
			continue
		}
		updated := latestUpdatedAt(asks)
		resp.Peers = append(resp.Peers, askConfigResponse{PeerID: in.PeerID, Asks: asks, UpdatedAt: updated})
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// handleAsksConfigPut serves PUT /manage/billing/asks-config/{peerID}:
// replaces one owned peer's ask config and attempts an immediate publish.
func (s *Service) handleAsksConfigPut(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	if s.mode == config.BillingOff {
		http.Error(w, "billing disabled", http.StatusConflict)
		return
	}
	peerID := r.PathValue("peerID")
	if peerID == "" {
		http.Error(w, "missing peer", http.StatusBadRequest)
		return
	}
	var req askConfigRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		return
	}
	asks := make([]billing.Ask, 0, len(req.Asks))
	for _, a := range req.Asks {
		a.Service, a.Model = strings.TrimSpace(a.Service), strings.TrimSpace(a.Model)
		asks = append(asks, billing.Ask{
			Service:               a.Service,
			Model:                 a.Model,
			InputPerMillion:       a.InputPerMillion,
			CachedInputPerMillion: a.CachedInputPerMillion,
			OutputPerMillion:      a.OutputPerMillion,
		})
	}
	if err := s.validateAskConfig(asks); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Ownership first: existence is not disclosure — a peer owned by
	// another account reads as not-found.
	if inst, err := s.store.GetInstanceByPeerID(r.Context(), peerID); err != nil || inst.AccountID != accountID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.store.ReplaceAskConfig(r.Context(), peerID, asks); err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	resp := askConfigPutResponse{Published: true}
	if len(asks) > 0 {
		if err := s.publishAskConfig(r.Context(), accountID, peerID, asks); err != nil {
			resp.Published = false
			msg := err.Error()
			resp.PublishError = &msg
		}
	}
	saved, err := s.store.AskConfigForPeers(r.Context(), []string{peerID})
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	row := askConfigResponse{PeerID: peerID}
	if cfg, ok := saved[peerID]; ok {
		row.Asks = cfg
		row.UpdatedAt = latestUpdatedAt(cfg)
	} else {
		row.Asks = []billing.Ask{}
	}
	resp.askConfigResponse = row
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// handleAsksConfigDelete serves DELETE /manage/billing/asks-config/{peerID}:
// clears the config. Live market rows expire naturally (5-minute TTL).
func (s *Service) handleAsksConfigDelete(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	if s.mode == config.BillingOff {
		http.Error(w, "billing disabled", http.StatusConflict)
		return
	}
	peerID := r.PathValue("peerID")
	inst, err := s.store.GetInstanceByPeerID(r.Context(), peerID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if inst.AccountID != accountID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.store.ReplaceAskConfig(r.Context(), peerID, nil); err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, askConfigResponse{PeerID: peerID, Asks: []billing.Ask{}})
}

// validateAskConfig mirrors the publication contract: bounded entry count,
// no empty/duplicate routes, no negative rates. Advertisement matching
// happens at publish time (the peer may be offline while the owner edits).
func (s *Service) validateAskConfig(asks []billing.Ask) error {
	if len(asks) > maxAskConfig {
		return errors.New("too_many_asks")
	}
	seen := make(map[string]bool, len(asks))
	for _, a := range asks {
		if a.Service == "" || a.Model == "" {
			return errors.New("empty_service_or_model")
		}
		key := a.Service + "\x00" + a.Model
		if seen[key] {
			return errors.New("duplicate_ask")
		}
		seen[key] = true
		if a.InputPerMillion < 0 || a.CachedInputPerMillion < 0 || a.OutputPerMillion < 0 {
			return errors.New("negative_rate")
		}
	}
	return nil
}

// publishAskConfig attempts the immediate republish with the same liveness
// and advertisement checks as POST /internal/pricing (the nodecred path).
// Ownership is proven by the JWT account owning the instance rather than by
// the node credential — the same principal, one step up.
func (s *Service) publishAskConfig(ctx context.Context, accountID, peerID string, asks []billing.Ask) error {
	inst, err := s.store.GetInstanceByPeerID(ctx, peerID)
	if err != nil {
		return errors.New("instance not found")
	}
	if inst.AccountID != accountID {
		return errors.New("not your instance")
	}
	if !billing.BillableProvider(inst.AccountID, inst.OwnerWallet) {
		return errors.New("instance not billable")
	}
	obs, err := s.mesh.LookupPeer(ctx, peerID)
	if err != nil {
		return errors.New("peer not observed")
	}
	if obs.Wallet != inst.OwnerWallet {
		return errors.New("observed wallet mismatch")
	}
	if err := pricingapi.ValidateAsksAgainstAdvertisement(asks, obs.Services); err != nil {
		return err
	}
	if _, err := s.store.ReplaceAsks(ctx, peerID, asks, sellerAskTTLDuration); err != nil {
		return err
	}
	return nil
}

func latestUpdatedAt(asks []billing.Ask) *time.Time {
	var latest time.Time
	for _, a := range asks {
		if a.UpdatedAt.After(latest) {
			latest = a.UpdatedAt
		}
	}
	if latest.IsZero() {
		return nil
	}
	t := latest.UTC()
	return &t
}

func modelCapsToJSON(rows []billing.ModelCaps) []modelCapsJSON {
	out := make([]modelCapsJSON, 0, len(rows))
	for _, r := range rows {
		out = append(out, modelCapsJSON{
			Service:               r.Service,
			Model:                 r.Model,
			InputPerMillion:       nullableIntFrom(r.InputPerMillion),
			CachedInputPerMillion: nullableIntFrom(r.CachedInputPerMillion),
			OutputPerMillion:      nullableIntFrom(r.OutputPerMillion),
		})
	}
	return out
}
