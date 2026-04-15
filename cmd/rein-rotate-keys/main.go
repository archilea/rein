// Package main is the entry point for rein-rotate-keys, the offline tool
// for rotating the AES-256-GCM key that encrypts upstream_key values in the
// Rein SQLite keystore.
//
// The tool is a separate binary (not a rein subcommand) by design: the
// running rein process holds exactly one REIN_ENCRYPTION_KEY at a time, so
// rotation is inherently offline. A separate binary makes that UX contract
// obvious to operators and keeps the production rein binary free of admin
// surface area.
//
// Usage:
//
//	rein-rotate-keys \
//	  --db sqlite:./rein.db \
//	  --old-key $OLD_HEX \
//	  --new-key $NEW_HEX
//
// See docs/runbooks/key-rotation.md for the end-to-end runbook.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/archilea/rein/internal/keys"
)

// version is set at build time via -ldflags, mirroring cmd/rein.
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// Errors go to stderr so a caller can parse "rotated=N skipped=M"
		// on stdout without mixing the two streams. We never print key
		// material or plaintext, only the structural failure reason.
		fmt.Fprintln(os.Stderr, "rein-rotate-keys: "+err.Error())
		os.Exit(1)
	}
}

// run is the testable entry point: it parses flags from argv, builds the two
// ciphers, and calls keys.RotateEncryption. Split from main so the binary
// can be exercised in-process by tests without os.Exit interference.
func run(argv []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("rein-rotate-keys", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		dbFlag      string
		oldKeyFlag  string
		newKeyFlag  string
		versionFlag bool
	)
	fs.StringVar(&dbFlag, "db", "", "database URL (sqlite:<path>)")
	fs.StringVar(&oldKeyFlag, "old-key", "", "current encryption key, 64 hex chars")
	fs.StringVar(&newKeyFlag, "new-key", "", "replacement encryption key, 64 hex chars")
	fs.BoolVar(&versionFlag, "version", false, "print version and exit")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "rein-rotate-keys %s\n\n", version)
		_, _ = fmt.Fprintln(stderr, "Offline rotation of the Rein keystore encryption key.")
		_, _ = fmt.Fprintln(stderr, "Stop the rein server before running this tool.")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Usage:")
		_, _ = fmt.Fprintln(stderr, "  rein-rotate-keys --db sqlite:./rein.db --old-key $OLD_HEX --new-key $NEW_HEX")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Flags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(argv); err != nil {
		return err
	}
	if versionFlag {
		_, _ = fmt.Fprintln(stdout, version)
		return nil
	}
	if dbFlag == "" {
		fs.Usage()
		return errors.New("--db is required")
	}
	if oldKeyFlag == "" {
		fs.Usage()
		return errors.New("--old-key is required")
	}
	if newKeyFlag == "" {
		fs.Usage()
		return errors.New("--new-key is required")
	}
	if oldKeyFlag == newKeyFlag {
		return errors.New("--old-key and --new-key must differ (rotation into the same key would be a no-op that still rewrites the column)")
	}

	path, err := parseSQLiteURL(dbFlag)
	if err != nil {
		return err
	}

	oldCipher, err := cipherFromHex(oldKeyFlag)
	if err != nil {
		return fmt.Errorf("invalid --old-key: %w", err)
	}
	newCipher, err := cipherFromHex(newKeyFlag)
	if err != nil {
		return fmt.Errorf("invalid --new-key: %w", err)
	}

	// SIGINT/SIGTERM cancel the context so a mid-rotation abort rolls the
	// transaction back cleanly instead of leaving the DB write lock held.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	res, err := keys.RotateEncryption(ctx, path, oldCipher, newCipher)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "rotated=%d skipped=%d duration=%s\n",
		res.Rotated, res.Skipped, res.Duration.Round(1_000_000).String(),
	)
	return nil
}

// parseSQLiteURL accepts the same "sqlite:<path>" scheme as REIN_DB_URL in
// cmd/rein. The rotation tool does not support the "memory" scheme because
// a memory keystore has no durable state to rotate.
func parseSQLiteURL(dbURL string) (string, error) {
	trimmed := strings.TrimSpace(dbURL)
	path, ok := strings.CutPrefix(trimmed, "sqlite:")
	if !ok {
		return "", fmt.Errorf("unsupported --db scheme %q (want sqlite:<path>)", dbURL)
	}
	if strings.TrimSpace(path) == "" {
		return "", errors.New("--db sqlite:<path> requires a non-empty path")
	}
	return path, nil
}

// cipherFromHex parses a 64-char hex string into an AES-256-GCM cipher, the
// same format accepted by REIN_ENCRYPTION_KEY.
func cipherFromHex(h string) (keys.Cipher, error) {
	raw, err := keys.DecodeHexKey(h)
	if err != nil {
		return nil, err
	}
	return keys.NewAESGCM(raw)
}
