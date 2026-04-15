package keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RotateResult summarizes a rein-rotate-keys run.
//
// Rotated counts rows whose upstream_key decrypted under the old cipher and
// was re-encrypted under the new cipher inside the rotation transaction.
// Skipped counts rows already encrypted under the new cipher (a second run
// with the same --new-key is a no-op for those rows). Duration is wall time
// for the whole rotation pass.
type RotateResult struct {
	Rotated  int
	Skipped  int
	Duration time.Duration
}

// RotateEncryption re-encrypts every virtual_keys row in the SQLite file at
// path, decrypting upstream_key with oldCipher and re-encrypting with
// newCipher inside a single write transaction.
//
// Idempotent: rows that already decrypt under newCipher (for example, a
// second run with the same --new-key) are left untouched and counted as
// Skipped. If any row decrypts under neither cipher, the entire run aborts
// and the database is left unchanged.
//
// The caller MUST stop the rein server before invoking this function. The
// function makes no attempt to coordinate with a live process; if rein is
// running, SQLite's own write lock will surface a busy error that aborts
// the rotation with no partial writes. See docs/runbooks/key-rotation.md.
//
// Errors never embed plaintext upstream keys or the raw key material, only
// row identifiers and structural failure reasons, so the error surface is
// safe to log.
func RotateEncryption(ctx context.Context, path string, oldCipher, newCipher Cipher) (*RotateResult, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite path is required")
	}
	if oldCipher == nil {
		return nil, errors.New("rotate requires a non-nil old cipher")
	}
	if newCipher == nil {
		return nil, errors.New("rotate requires a non-nil new cipher")
	}

	start := time.Now()

	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)",
		url.PathEscape(path),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	updates, skipped, err := planRotation(ctx, db, oldCipher, newCipher)
	if err != nil {
		return nil, err
	}

	if len(updates) > 0 {
		if err := applyRotation(ctx, db, updates, newCipher); err != nil {
			return nil, err
		}
	}

	return &RotateResult{
		Rotated:  len(updates),
		Skipped:  skipped,
		Duration: time.Since(start),
	}, nil
}

// rotationUpdate captures a single row's pending rewrite: the row id and the
// ciphertext encrypted under the new cipher, ready to be UPDATE'd inside the
// apply transaction.
type rotationUpdate struct {
	id            string
	newCiphertext string
}

// planRotation walks every virtual_keys row, decrypts upstream_key with the
// old cipher, and re-encrypts with the new cipher. Rows that already decrypt
// under the new cipher are counted as skipped. Any row that decrypts under
// neither aborts the whole run BEFORE any DB write, so a partial rotation
// cannot leak out even if we later lose the lock.
func planRotation(ctx context.Context, db *sql.DB, oldCipher, newCipher Cipher) ([]rotationUpdate, int, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, upstream_key FROM virtual_keys`)
	if err != nil {
		return nil, 0, fmt.Errorf("select virtual_keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var updates []rotationUpdate
	skipped := 0
	for rows.Next() {
		var id, enc string
		if err := rows.Scan(&id, &enc); err != nil {
			return nil, 0, fmt.Errorf("scan virtual_keys row: %w", err)
		}
		plain, oldErr := oldCipher.Decrypt(enc)
		if oldErr == nil {
			newCT, encErr := newCipher.Encrypt(plain)
			if encErr != nil {
				return nil, 0, fmt.Errorf("re-encrypt row %s: %w", id, encErr)
			}
			updates = append(updates, rotationUpdate{id: id, newCiphertext: newCT})
			continue
		}
		if _, newErr := newCipher.Decrypt(enc); newErr == nil {
			skipped++
			continue
		}
		// Neither cipher decrypts this row. Without aborting, a partial
		// rotation of the well-formed rows would leave the DB in a
		// half-migrated state that no future --old-key could recover from.
		// Abort cleanly and let the operator diagnose before any write.
		return nil, 0, fmt.Errorf(
			"row %s: upstream_key decrypts under neither --old-key nor --new-key "+
				"(old-key wrong, or the column was written under a third key)",
			id,
		)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate virtual_keys: %w", err)
	}
	return updates, skipped, nil
}

// applyRotation writes the planned updates inside a single transaction and
// then re-reads at least one of the updated rows, decrypting with the new
// cipher, before committing. A failure on any step (UPDATE, re-read,
// verification decrypt) triggers a rollback so callers see either a clean
// success or an unchanged DB.
func applyRotation(ctx context.Context, db *sql.DB, updates []rotationUpdate, newCipher Cipher) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.PrepareContext(ctx, `UPDATE virtual_keys SET upstream_key = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, u := range updates {
		if _, err := stmt.ExecContext(ctx, u.newCiphertext, u.id); err != nil {
			return fmt.Errorf("update row %s: %w", u.id, err)
		}
	}

	// Sanity check: read one of the rows we just wrote and confirm that
	// the new cipher can decrypt it inside the same transaction. This
	// catches a Cipher implementation that successfully produces a
	// ciphertext its own Decrypt rejects (a contract bug), which would
	// otherwise only surface when rein next starts against this DB.
	verifyID := updates[0].id
	var stored string
	if err := tx.QueryRowContext(ctx,
		`SELECT upstream_key FROM virtual_keys WHERE id = ?`, verifyID,
	).Scan(&stored); err != nil {
		return fmt.Errorf("verify row %s: %w", verifyID, err)
	}
	if _, err := newCipher.Decrypt(stored); err != nil {
		return fmt.Errorf("verify decrypt of row %s under new cipher: %w", verifyID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	committed = true
	return nil
}
