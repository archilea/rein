package keys

import "testing"

func TestValidID_AcceptsGenerated(t *testing.T) {
	// A freshly generated ID must satisfy ValidID by construction. If this
	// ever fails, either GenerateID changed its output format or ValidID's
	// regex drifted from that format. Either way the two must agree.
	for i := 0; i < 100; i++ {
		id, err := GenerateID()
		if err != nil {
			t.Fatalf("GenerateID: %v", err)
		}
		if !ValidID(id) {
			t.Fatalf("GenerateID produced %q which ValidID rejected", id)
		}
	}
}

func TestValidID_Rejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"missing prefix", "a1b2c3d4e5f60718"},
		{"wrong prefix", "token_a1b2c3d4e5f60718"},
		{"uppercase hex", "key_A1B2C3D4E5F60718"},
		{"too short", "key_a1b2c3d4e5f6071"},
		{"too long", "key_a1b2c3d4e5f607189"},
		{"non-hex chars", "key_a1b2c3d4e5f607xy"},
		{"trailing newline", "key_a1b2c3d4e5f60718\n"},
		{"leading space", " key_a1b2c3d4e5f60718"},
		{"path traversal", "key_a1b2c3d4e5f60718/../etc/passwd"},
		{"url-encoded newline", "key_a1b2c3d4e5f60718%0a"},
		{"null byte", "key_a1b2c3d4e5f60718\x00"},
		{"ansi escape", "key_a1b2c3d4e5f60718\x1b[31m"},
		{"sql injection attempt", "key_a1b2c3d4e5f60718' OR '1'='1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ValidID(tc.in) {
				t.Errorf("ValidID(%q) = true, want false", tc.in)
			}
		})
	}
}
