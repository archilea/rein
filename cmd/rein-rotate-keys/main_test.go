package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archilea/rein/internal/keys"
)

// randomHexKey returns a 64-char hex string representing a 32-byte key,
// the exact format the CLI accepts on --old-key / --new-key.
func randomHexKey(t *testing.T) string {
	t.Helper()
	buf := make([]byte, keys.AESKeySize)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

// withPipeOutputs wires temporary pipes as stdout/stderr, runs fn, and
// returns what was written to each. This gives the tests a faithful copy of
// production's *os.File signatures without any string-builder adapter.
func withPipeOutputs(t *testing.T, fn func(stdout, stderr *os.File) error) (string, string, error) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	outDone := make(chan []byte)
	errDone := make(chan []byte)
	go func() { b, _ := io.ReadAll(outR); outDone <- b }()
	go func() { b, _ := io.ReadAll(errR); errDone <- b }()

	runErr := fn(outW, errW)
	_ = outW.Close()
	_ = errW.Close()
	return string(<-outDone), string(<-errDone), runErr
}

func seedRotatableDB(t *testing.T, oldHex string, n int) (path, secret string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "rein.db")

	raw, err := keys.DecodeHexKey(oldHex)
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}
	c, err := keys.NewAESGCM(raw)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	s, err := keys.NewSQLite(path, c)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	secret = "sk-real-provider-key-xyz"
	for i := 0; i < n; i++ {
		id, err := keys.GenerateID()
		if err != nil {
			t.Fatal(err)
		}
		tok, err := keys.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		vk := &keys.VirtualKey{
			ID:          id,
			Token:       tok,
			Name:        "seed",
			Upstream:    keys.UpstreamOpenAI,
			UpstreamKey: secret,
			CreatedAt:   time.Now().UTC(),
		}
		if err := s.Create(context.Background(), vk); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	_ = s.Close()
	return path, secret
}

func TestCLI_HappyPath(t *testing.T) {
	oldHex := randomHexKey(t)
	newHex := randomHexKey(t)
	path, secret := seedRotatableDB(t, oldHex, 3)

	stdout, stderr, err := withPipeOutputs(t, func(stdout, stderr *os.File) error {
		return run([]string{
			"--db", "sqlite:" + path,
			"--old-key", oldHex,
			"--new-key", newHex,
		}, stdout, stderr)
	})
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "rotated=3") || !strings.Contains(stdout, "skipped=0") {
		t.Errorf("stdout summary missing: %q", stdout)
	}

	// Reopen with the new key and confirm every row still round-trips.
	rawNew, _ := keys.DecodeHexKey(newHex)
	cNew, _ := keys.NewAESGCM(rawNew)
	s, err := keys.NewSQLite(path, cNew)
	if err != nil {
		t.Fatalf("reopen with new key: %v", err)
	}
	defer func() { _ = s.Close() }()
	all, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list after rotate: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("row count: got %d want 3", len(all))
	}
	for _, k := range all {
		if k.UpstreamKey != secret {
			t.Errorf("plaintext drift: got %q want %q", k.UpstreamKey, secret)
		}
	}
}

func TestCLI_MissingFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no db", []string{"--old-key", strings.Repeat("a", 64), "--new-key", strings.Repeat("b", 64)}, "--db is required"},
		{"no old", []string{"--db", "sqlite:x", "--new-key", strings.Repeat("b", 64)}, "--old-key is required"},
		{"no new", []string{"--db", "sqlite:x", "--old-key", strings.Repeat("a", 64)}, "--new-key is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := withPipeOutputs(t, func(stdout, stderr *os.File) error {
				return run(tc.args, stdout, stderr)
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%v want contains %q", err, tc.want)
			}
		})
	}
}

func TestCLI_SameOldAndNewKey(t *testing.T) {
	h := randomHexKey(t)
	_, _, err := withPipeOutputs(t, func(stdout, stderr *os.File) error {
		return run([]string{
			"--db", "sqlite:/tmp/does-not-matter.db",
			"--old-key", h,
			"--new-key", h,
		}, stdout, stderr)
	})
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Errorf("err=%v want 'must differ'", err)
	}
}

func TestCLI_UnsupportedScheme(t *testing.T) {
	_, _, err := withPipeOutputs(t, func(stdout, stderr *os.File) error {
		return run([]string{
			"--db", "memory",
			"--old-key", randomHexKey(t),
			"--new-key", randomHexKey(t),
		}, stdout, stderr)
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported --db scheme") {
		t.Errorf("err=%v want unsupported scheme", err)
	}
}

func TestCLI_InvalidHexKey(t *testing.T) {
	_, _, err := withPipeOutputs(t, func(stdout, stderr *os.File) error {
		return run([]string{
			"--db", "sqlite:/tmp/x.db",
			"--old-key", "not-hex",
			"--new-key", randomHexKey(t),
		}, stdout, stderr)
	})
	if err == nil || !strings.Contains(err.Error(), "invalid --old-key") {
		t.Errorf("err=%v want invalid --old-key", err)
	}
}

func TestCLI_NoSecretsInSummary(t *testing.T) {
	oldHex := randomHexKey(t)
	newHex := randomHexKey(t)
	path, secret := seedRotatableDB(t, oldHex, 1)

	stdout, stderr, err := withPipeOutputs(t, func(stdout, stderr *os.File) error {
		return run([]string{
			"--db", "sqlite:" + path,
			"--old-key", oldHex,
			"--new-key", newHex,
		}, stdout, stderr)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	combined := stdout + stderr
	for _, leak := range []string{secret, oldHex, newHex} {
		if strings.Contains(combined, leak) {
			t.Errorf("output leaks secret material: %q", combined)
		}
	}
}

func TestCLI_Version(t *testing.T) {
	stdout, _, err := withPipeOutputs(t, func(stdout, stderr *os.File) error {
		return run([]string{"--version"}, stdout, stderr)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("expected version string on stdout")
	}
}

func TestCLI_IdempotentSecondRun(t *testing.T) {
	oldHex := randomHexKey(t)
	newHex := randomHexKey(t)
	path, _ := seedRotatableDB(t, oldHex, 2)

	args := []string{"--db", "sqlite:" + path, "--old-key", oldHex, "--new-key", newHex}

	if _, _, err := withPipeOutputs(t, func(stdout, stderr *os.File) error {
		return run(args, stdout, stderr)
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	stdout, _, err := withPipeOutputs(t, func(stdout, stderr *os.File) error {
		return run(args, stdout, stderr)
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(stdout, "rotated=0") || !strings.Contains(stdout, "skipped=2") {
		t.Errorf("second run summary: got %q want rotated=0 skipped=2", stdout)
	}
}

// Small sanity check that the buffered helper does not silently drop output,
// which would mask secret-leak regressions if we ever refactored the runner.
func TestCLI_PipeHelperRoundTrips(t *testing.T) {
	stdout, stderr, _ := withPipeOutputs(t, func(stdout, stderr *os.File) error {
		_, _ = stdout.Write([]byte("hello\n"))
		_, _ = stderr.Write([]byte("world\n"))
		return nil
	})
	if !bytes.Equal([]byte(stdout), []byte("hello\n")) || !bytes.Equal([]byte(stderr), []byte("world\n")) {
		t.Errorf("pipe helper dropped data: stdout=%q stderr=%q", stdout, stderr)
	}
}
