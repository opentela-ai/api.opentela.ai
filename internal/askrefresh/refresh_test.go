package askrefresh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/store"
)

type fakeStore struct {
	cfgs    map[string][]billing.Ask
	insts   map[string]store.InstanceInfo
	pub     map[string][]billing.Ask
	pubTTL  time.Duration
	cfgErr  error
	instErr error
	pubErr  error
}

func (f *fakeStore) AllAskConfigs(context.Context) (map[string][]billing.Ask, error) {
	if f.cfgErr != nil {
		return nil, f.cfgErr
	}
	return f.cfgs, nil
}
func (f *fakeStore) GetInstanceByPeerID(_ context.Context, peerID string) (store.InstanceInfo, error) {
	if f.instErr != nil {
		return store.InstanceInfo{}, f.instErr
	}
	if inst, ok := f.insts[peerID]; ok {
		return inst, nil
	}
	return store.InstanceInfo{}, store.ErrNotFound
}
func (f *fakeStore) ReplaceAsks(_ context.Context, peerID string, asks []billing.Ask, ttl time.Duration) (int64, error) {
	if f.pubErr != nil {
		return 0, f.pubErr
	}
	if f.pub == nil {
		f.pub = map[string][]billing.Ask{}
	}
	f.pub[peerID] = asks
	f.pubTTL = ttl
	return int64(len(asks)), nil
}

type fakeMesh struct {
	peers map[string]mesh.PeerObservation
	err   error
}

func (f fakeMesh) LookupPeer(_ context.Context, peerID string) (mesh.PeerObservation, error) {
	if f.err != nil {
		return mesh.PeerObservation{}, f.err
	}
	if obs, ok := f.peers[peerID]; ok {
		return obs, nil
	}
	return mesh.PeerObservation{}, errors.New("not found")
}

func asksFor(model string, in int64) []billing.Ask {
	return []billing.Ask{{Service: "llm", Model: model, InputPerMillion: in, CachedInputPerMillion: 0, OutputPerMillion: in * 3}}
}

func TestRefreshPublishesLiveMatchingPeer(t *testing.T) {
	st := &fakeStore{
		cfgs:  map[string][]billing.Ask{"peer-live": asksFor("m1", 100)},
		insts: map[string]store.InstanceInfo{"peer-live": {AccountID: "acct-1", OwnerWallet: "WalletA"}},
	}
	m := fakeMesh{peers: map[string]mesh.PeerObservation{
		"peer-live": {Wallet: "WalletA", Online: true, ObservedAt: time.Now().UTC()},
	}}
	s := New(st, m, nil)
	s.refreshOnce(context.Background())

	pub := st.pub["peer-live"]
	if len(pub) != 1 || pub[0].Model != "m1" || pub[0].InputPerMillion != 100 {
		t.Fatalf("published = %+v", pub)
	}
	if st.pubTTL != DefaultTTL {
		t.Fatalf("ttl = %v, want %v", st.pubTTL, DefaultTTL)
	}
}

func TestRefreshSkipsWhenNotLiveOrOwnerMismatch(t *testing.T) {
	st := &fakeStore{
		cfgs: map[string][]billing.Ask{
			"peer-dark":    asksFor("m1", 100), // mesh lookup fails
			"peer-hijack":  asksFor("m2", 100), // observation wallet differs from owner
			"peer-nonbill": asksFor("m3", 100), // no owner wallet: not billable
		},
		insts: map[string]store.InstanceInfo{
			"peer-dark":    {AccountID: "acct-1", OwnerWallet: "WalletA"},
			"peer-hijack":  {AccountID: "acct-1", OwnerWallet: "WalletA"},
			"peer-nonbill": {AccountID: "acct-1"},
		},
	}
	m := fakeMesh{peers: map[string]mesh.PeerObservation{
		"peer-hijack": {Wallet: "SomebodyElse", Online: true, ObservedAt: time.Now().UTC()},
	}}
	s := New(st, m, nil)
	s.refreshOnce(context.Background())

	if len(st.pub) != 0 {
		t.Fatalf("nothing must publish for dark/hijacked/nonbillable peers, got %+v", st.pub)
	}
}

func TestRefreshStaleObservation(t *testing.T) {
	st := &fakeStore{
		cfgs:  map[string][]billing.Ask{"peer-stale": asksFor("m1", 100)},
		insts: map[string]store.InstanceInfo{"peer-stale": {AccountID: "acct-1", OwnerWallet: "WalletA"}},
	}
	m := fakeMesh{peers: map[string]mesh.PeerObservation{
		"peer-stale": {Wallet: "WalletA", Online: true, ObservedAt: time.Now().UTC().Add(-30 * time.Minute)},
	}}
	s := New(st, m, nil)
	s.SetOwnershipMaxAge(10 * time.Minute)
	s.refreshOnce(context.Background())
	if len(st.pub) != 0 {
		t.Fatalf("stale observation must not republish: %+v", st.pub)
	}
}

func TestRefreshStoreErrorsAreSkipped(t *testing.T) {
	// Load failure and publish failure are both non-fatal: the next cycle
	// retries and market rows simply expire in between.
	st := &fakeStore{cfgErr: errors.New("db down")}
	s := New(st, fakeMesh{}, nil)
	s.refreshOnce(context.Background()) // must not panic
	if len(st.pub) != 0 {
		t.Fatalf("no publish expected: %+v", st.pub)
	}

	st2 := &fakeStore{
		cfgs:   map[string][]billing.Ask{"peer-live": asksFor("m1", 100)},
		insts:  map[string]store.InstanceInfo{"peer-live": {AccountID: "acct-1", OwnerWallet: "WalletA"}},
		pubErr: errors.New("db write failed"),
	}
	m2 := fakeMesh{peers: map[string]mesh.PeerObservation{
		"peer-live": {Wallet: "WalletA", Online: true, ObservedAt: time.Now().UTC()},
	}}
	s2 := New(st2, m2, nil)
	s2.refreshOnce(context.Background()) // must not panic
}
