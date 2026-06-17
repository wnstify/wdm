package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// writeFile writes data to a fresh file under dir and returns its path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

func TestRun_KeygenSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "release.key")
	pubPath := filepath.Join(dir, "release.pub")

	if err := run([]string{"keygen", "--private", privPath, "--public", pubPath}); err != nil {
		t.Fatalf("keygen: %v", err)
	}

	// The private key must be owner-only (0600).
	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("private key mode = %o, want 600", got)
	}

	input := writeFile(t, dir, "SHA256SUMS", []byte("checksum-file content over every release asset\n"))
	sigPath := filepath.Join(dir, "SHA256SUMS.sig")

	if err := run([]string{"sign", "--private", privPath, "--in", input, "--out", sigPath}); err != nil {
		t.Fatalf("sign: %v", err)
	}

	// The signature on disk is a raw 64-byte Ed25519 signature.
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatalf("reading signature: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}

	if err := run([]string{"verify", "--public", pubPath, "--in", input, "--sig", sigPath}); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestRun_KeygenRefusesExistingPrivateKey guards the file mode of the private
// key against the os.WriteFile pitfall: os.WriteFile applies its mode only when
// it creates the file, so writing over a pre-existing 0644 path would leave the
// secret world-readable. keygen now creates the private key exclusively, so it
// must refuse an existing path and leave its content and mode untouched.
func TestRun_KeygenRefusesExistingPrivateKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "release.key")
	pubPath := filepath.Join(dir, "release.pub")

	// Pre-create the private path at a loose, group/world-readable mode.
	const existing = "not a key\n"
	if err := os.WriteFile(privPath, []byte(existing), 0o644); err != nil {
		t.Fatalf("seeding private path: %v", err)
	}

	if err := run([]string{"keygen", "--private", privPath, "--public", pubPath}); err == nil {
		t.Fatal("keygen overwrote an existing private key, want an error")
	}

	// The pre-existing file must be untouched: no secret was written into it,
	// and its loose mode was never tightened-after-the-fact over key bytes.
	got, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("reading private path: %v", err)
	}
	if string(got) != existing {
		t.Error("keygen modified the contents of the pre-existing private path")
	}

	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat private path: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("pre-existing private path mode = %o, want 644 (unchanged)", perm)
	}
}

func TestRun_VerifyKeyHappyPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "release.key")
	pubPath := filepath.Join(dir, "release.pub")
	if err := run([]string{"keygen", "--private", privPath, "--public", pubPath}); err != nil {
		t.Fatalf("keygen: %v", err)
	}

	if err := run([]string{"verify-key", "--public", pubPath}); err != nil {
		t.Errorf("verify-key: %v", err)
	}
}

func TestRun_VerifyRejectsBadInput(t *testing.T) {
	t.Parallel()

	// A shared signed fixture: one key pair, one input, one valid signature.
	dir := t.TempDir()
	privPath := filepath.Join(dir, "release.key")
	pubPath := filepath.Join(dir, "release.pub")
	if err := run([]string{"keygen", "--private", privPath, "--public", pubPath}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	input := writeFile(t, dir, "SHA256SUMS", []byte("the canonical checksum payload\n"))
	sigPath := filepath.Join(dir, "SHA256SUMS.sig")
	if err := run([]string{"sign", "--private", privPath, "--in", input, "--out", sigPath}); err != nil {
		t.Fatalf("sign: %v", err)
	}

	tests := []struct {
		name  string
		setup func(t *testing.T) []string
	}{
		{
			name: "tampered signature",
			setup: func(t *testing.T) []string {
				t.Helper()
				sig, err := os.ReadFile(sigPath)
				if err != nil {
					t.Fatalf("reading signature: %v", err)
				}
				sig[0] ^= 0xff
				bad := writeFile(t, dir, "tampered.sig", sig)
				return []string{"verify", "--public", pubPath, "--in", input, "--sig", bad}
			},
		},
		{
			name: "wrong-length signature",
			setup: func(t *testing.T) []string {
				t.Helper()
				bad := writeFile(t, dir, "short.sig", []byte("too short"))
				return []string{"verify", "--public", pubPath, "--in", input, "--sig", bad}
			},
		},
		{
			name: "tampered input",
			setup: func(t *testing.T) []string {
				t.Helper()
				other := writeFile(t, dir, "other-input", []byte("a different payload\n"))
				return []string{"verify", "--public", pubPath, "--in", other, "--sig", sigPath}
			},
		},
		{
			name: "wrong key",
			setup: func(t *testing.T) []string {
				t.Helper()
				otherPriv := filepath.Join(dir, "other.key")
				otherPub := filepath.Join(dir, "other.pub")
				if err := run([]string{"keygen", "--private", otherPriv, "--public", otherPub}); err != nil {
					t.Fatalf("keygen: %v", err)
				}
				return []string{"verify", "--public", otherPub, "--in", input, "--sig", sigPath}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := run(tt.setup(t)); err == nil {
				t.Errorf("verify accepted %s, want error", tt.name)
			}
		})
	}
}

func TestRun_VerifyKeyRejectsNonEd25519(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	t.Run("rsa key", func(t *testing.T) {
		t.Parallel()
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generating rsa key: %v", err)
		}
		der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatalf("marshaling rsa key: %v", err)
		}
		path := writeFile(t, dir, "rsa.pub",
			pem.EncodeToMemory(&pem.Block{Type: publicKeyPEMType, Bytes: der}))
		if err := run([]string{"verify-key", "--public", path}); err == nil {
			t.Error("verify-key accepted an rsa key, want error")
		}
	})

	t.Run("wrong-length raw key", func(t *testing.T) {
		t.Parallel()
		// A "PUBLIC KEY" PEM whose body is not a valid PKIX SPKI at all.
		path := writeFile(t, dir, "garbage.pub",
			pem.EncodeToMemory(&pem.Block{Type: publicKeyPEMType, Bytes: []byte("not an spki")}))
		if err := run([]string{"verify-key", "--public", path}); err == nil {
			t.Error("verify-key accepted garbage, want error")
		}
	})
}

func TestRun_UnknownAndMissingSubcommand(t *testing.T) {
	t.Parallel()

	if err := run(nil); err == nil {
		t.Error("run(nil) returned no error, want a missing-subcommand error")
	}
	if err := run([]string{"bogus"}); err == nil {
		t.Error("run(bogus) returned no error, want an unknown-subcommand error")
	}
}
