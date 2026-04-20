package keys

import (
	"context"
	"log/slog"
	"time"
)

// RunExpirySweeper is a blocking loop that auto-revokes keys whose
// expires_at has passed. The proxy hot path already rejects expired
// keys (belt-and-suspenders), so the sweeper is strictly for audit
// trail durability: after a tick, the DB row carries
// revoked_at == expires_at so operators can distinguish automatic
// revocation from a manual POST /admin/v1/keys/{id}/revoke (which
// stamps revoked_at = time.Now()).
//
// The loop returns when ctx is canceled. The first tick fires after
// interval, not immediately, to stay consistent with time.Ticker's
// contract and to avoid a startup thundering-herd when multiple
// replicas come up at once. Failures from ListExpiring or RevokeAt are
// logged with the key ID only, never the token or upstream_key.
func RunExpirySweeper(ctx context.Context, store Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			sweepOnce(ctx, store, now.UTC())
		}
	}
}

// sweepOnce performs a single sweep pass. Extracted so unit tests can
// exercise the body deterministically without racing a ticker.
func sweepOnce(ctx context.Context, store Store, now time.Time) {
	expired, err := store.ListExpiring(ctx, now)
	if err != nil {
		slog.Error("expiry sweep: list failed", "err", err)
		return
	}
	for _, k := range expired {
		if k == nil || k.ExpiresAt == nil {
			continue
		}
		at := k.ExpiresAt.UTC()
		if err := store.RevokeAt(ctx, k.ID, at); err != nil {
			slog.Error("expiry sweep: revoke failed", "err", err, "key_id", k.ID)
			continue
		}
		slog.Info("virtual key auto-revoked on expiry",
			"key_id", k.ID,
			"expires_at", at.Format(time.RFC3339Nano),
		)
	}
}
