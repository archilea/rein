package keys

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeStore exists only for sweeper tests so we can inject failure
// paths (ListExpiring error, RevokeAt error) without going through
// SQLite. Methods not exercised by the sweeper panic on invocation so
// an accidental new call site is caught in review.
type fakeStore struct {
	mu       sync.Mutex
	expiring []*VirtualKey
	revoked  []struct {
		id string
		at time.Time
	}
	listErr   error
	revokeErr error
	revokeFor map[string]error
}

func (f *fakeStore) Create(context.Context, *VirtualKey) error { panic("not used") }
func (f *fakeStore) GetByToken(context.Context, string) (*VirtualKey, error) {
	panic("not used")
}
func (f *fakeStore) GetByID(context.Context, string) (*VirtualKey, error) { panic("not used") }
func (f *fakeStore) List(context.Context) ([]*VirtualKey, error)          { panic("not used") }
func (f *fakeStore) Revoke(context.Context, string) error                 { panic("not used") }
func (f *fakeStore) Update(context.Context, string, KeyPatch) (*VirtualKey, error) {
	panic("not used")
}

func (f *fakeStore) RevokeAt(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revokeErr != nil {
		return f.revokeErr
	}
	if err, ok := f.revokeFor[id]; ok && err != nil {
		return err
	}
	f.revoked = append(f.revoked, struct {
		id string
		at time.Time
	}{id, at})
	return nil
}

func (f *fakeStore) ListExpiring(context.Context, time.Time) ([]*VirtualKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*VirtualKey, len(f.expiring))
	copy(out, f.expiring)
	return out, nil
}

func TestSweepOnce_RevokesWithExpiresAtStamp(t *testing.T) {
	expiry := time.Date(2026, 4, 17, 9, 0, 0, 0, time.UTC)
	later := expiry.Add(time.Hour)
	fs := &fakeStore{
		expiring: []*VirtualKey{
			{ID: "key_aaaaaaaaaaaaaaaa", ExpiresAt: &expiry},
		},
	}
	sweepOnce(context.Background(), fs, later)

	if len(fs.revoked) != 1 {
		t.Fatalf("revoked count: got %d want 1", len(fs.revoked))
	}
	got := fs.revoked[0]
	if got.id != "key_aaaaaaaaaaaaaaaa" {
		t.Errorf("revoked id: got %q", got.id)
	}
	if !got.at.Equal(expiry) {
		t.Errorf("revoked_at stamp: got %v want %v (must equal expires_at, not sweep time)", got.at, expiry)
	}
}

func TestSweepOnce_SkipsEmpty(t *testing.T) {
	fs := &fakeStore{}
	sweepOnce(context.Background(), fs, time.Now())
	if len(fs.revoked) != 0 {
		t.Errorf("revoked on empty: got %d", len(fs.revoked))
	}
}

func TestSweepOnce_ListError_NoRevoke(t *testing.T) {
	fs := &fakeStore{listErr: errors.New("boom")}
	sweepOnce(context.Background(), fs, time.Now())
	if len(fs.revoked) != 0 {
		t.Errorf("revoked after list error: got %d", len(fs.revoked))
	}
}

func TestSweepOnce_RevokeErrorContinues(t *testing.T) {
	expiry := time.Date(2026, 4, 17, 9, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		expiring: []*VirtualKey{
			{ID: "key_aaaaaaaaaaaaaaaa", ExpiresAt: &expiry},
			{ID: "key_bbbbbbbbbbbbbbbb", ExpiresAt: &expiry},
		},
		revokeFor: map[string]error{
			"key_aaaaaaaaaaaaaaaa": errors.New("locked"),
		},
	}
	sweepOnce(context.Background(), fs, expiry.Add(time.Minute))

	if len(fs.revoked) != 1 {
		t.Fatalf("revoked count: got %d want 1 (first errored, second succeeded)", len(fs.revoked))
	}
	if fs.revoked[0].id != "key_bbbbbbbbbbbbbbbb" {
		t.Errorf("expected second key revoked, got %q", fs.revoked[0].id)
	}
}

func TestSweepOnce_SkipsRowsWithNilExpiresAt(t *testing.T) {
	// Defensive: a hypothetical driver bug that returns a non-expired row
	// must not cause the sweeper to stamp an invalid revoked_at.
	fs := &fakeStore{
		expiring: []*VirtualKey{
			{ID: "key_aaaaaaaaaaaaaaaa", ExpiresAt: nil},
			nil,
		},
	}
	sweepOnce(context.Background(), fs, time.Now())
	if len(fs.revoked) != 0 {
		t.Errorf("revoked with nil expires_at: got %d want 0", len(fs.revoked))
	}
}

func TestRunExpirySweeper_CancelStopsLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunExpirySweeper(ctx, NewMemory(), 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweeper did not exit within 1s of context cancel")
	}
}

// TestRunExpirySweeper_FiresAtLeastOnce asserts that at least one tick
// reaches sweepOnce under a fast cadence. We detect the tick via
// ListExpiring being called on a tracking memory store.
func TestRunExpirySweeper_FiresAtLeastOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	expiry := time.Now().UTC().Add(-time.Hour)
	m := NewMemory()
	id, _ := GenerateID()
	token, _ := GenerateToken()
	vk := &VirtualKey{
		ID: id, Token: token, Name: "t",
		Upstream: UpstreamOpenAI, UpstreamKey: "sk-x",
		CreatedAt: time.Now().UTC(), ExpiresAt: &expiry,
	}
	if err := m.Create(ctx, vk); err != nil {
		t.Fatal(err)
	}

	go RunExpirySweeper(ctx, m, 20*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := m.GetByID(ctx, id)
		if got.IsRevoked() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("sweeper did not revoke expired key within 2s")
}
