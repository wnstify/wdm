// Command release-signing is the CI-side Ed25519 helper for the wdm release
// trust chain. It mints the release signing key pair, signs the SHA256SUMS
// checksum file, and verifies a detached signature or a public key, using
// only the Go standard library.
//
// The signature scheme matches the in-product verifier
// (internal/release.VerifyDetachedSignature): a raw 64-byte Ed25519
// signature over the EXACT input bytes, with no prehash. SHA-256 is the
// algorithm that produces SHA256SUMS content; it is unrelated to the
// signature here.
//
// Keys are PKIX/PKCS#8 PEM: the private key is a PKCS#8 "PRIVATE KEY" block
// written owner-only (0600), and the public key is a PKIX "PUBLIC KEY"
// block (0644). This helper is consumed by the release workflow's signing
// step; it embeds no key and reads none from the repository.
//
// Usage:
//
//	release-signing keygen  --private PATH --public PATH
//	release-signing sign    --private PATH --in PATH --out PATH
//	release-signing verify  --public PATH  --in PATH --sig PATH
//	release-signing verify-key --public PATH
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
)

const (
	privateKeyPEMType = "PRIVATE KEY"
	publicKeyPEMType  = "PUBLIC KEY"

	privateKeyFileMode = 0o600
	publicKeyFileMode  = 0o644
	signatureFileMode  = 0o644
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1) //nolint:forbidigo // CLI entrypoint: a failure must map to a nonzero exit.
	}
}

// run dispatches to the requested subcommand and returns an error rather
// than exiting, so the work is testable and only main() touches the process
// exit code.
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("expected a subcommand: keygen, sign, verify, or verify-key")
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "keygen":
		return runKeygen(rest)
	case "sign":
		return runSign(rest)
	case "verify":
		return runVerify(rest)
	case "verify-key":
		return runVerifyKey(rest)
	default:
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

// runKeygen generates an Ed25519 key pair and writes the PKCS#8 private key
// (owner-only) and the PKIX public key to the requested paths.
func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	privatePath := fs.String("private", "", "path to write the PKCS#8 Ed25519 private key (PEM)")
	publicPath := fs.String("public", "", "path to write the PKIX Ed25519 public key (PEM)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *privatePath == "" || *publicPath == "" {
		return errors.New("keygen requires --private and --public")
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generating ed25519 key: %w", err)
	}

	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshaling private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: privateKeyPEMType, Bytes: privDER})

	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return fmt.Errorf("marshaling public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: publicKeyPEMType, Bytes: pubDER})

	if err := writeNewPrivateKey(*privatePath, privPEM); err != nil {
		return err
	}
	if err := os.WriteFile(*publicPath, pubPEM, publicKeyFileMode); err != nil {
		return fmt.Errorf("writing public key: %w", err)
	}
	return nil
}

// writeNewPrivateKey writes the PEM-encoded private key to path with an
// exclusive create, so the file is always owner-only (0600) and an existing
// signing key is never clobbered. os.WriteFile applies its mode only when it
// creates the file; against a pre-existing path it would leave the old mode,
// so a key written over a 0644 file would stay world-readable. The exclusive
// create also avoids the toctou window a chmod-after-write would open.
func writeNewPrivateKey(path string, pemBytes []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateKeyFileMode) //nolint:gosec // G304: operator-supplied CLI output path
	if err != nil {
		return fmt.Errorf("creating private key file: %w", err)
	}
	defer func() {
		// Surface a close error only when the write itself succeeded:
		// durability of the key bytes matters, and a failed flush on close
		// would otherwise be lost.
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing private key file: %w", cerr)
		}
	}()

	if _, err = f.Write(pemBytes); err != nil {
		return fmt.Errorf("writing private key: %w", err)
	}
	return nil
}

// runSign signs the exact input bytes with the PKCS#8 Ed25519 private key
// and writes the raw 64-byte signature. There is no prehash, matching the
// in-product verifier.
func runSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	privatePath := fs.String("private", "", "path to the PKCS#8 Ed25519 private key (PEM)")
	inPath := fs.String("in", "", "path to the input file to sign")
	outPath := fs.String("out", "", "path to write the raw 64-byte signature")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *privatePath == "" || *inPath == "" || *outPath == "" {
		return errors.New("sign requires --private, --in, and --out")
	}

	priv, err := loadPrivateKey(*privatePath)
	if err != nil {
		return err
	}
	input, err := os.ReadFile(*inPath)
	if err != nil {
		return fmt.Errorf("reading input: %w", err)
	}

	sig := ed25519.Sign(priv, input)
	// G703 is suppressed: --out is an operator-supplied CLI path; writing
	// the signature to it is this build-time helper's whole purpose.
	if err := os.WriteFile(*outPath, sig, signatureFileMode); err != nil { //nolint:gosec // G703: operator-supplied CLI output path
		return fmt.Errorf("writing signature: %w", err)
	}
	return nil
}

// runVerify verifies a detached raw 64-byte Ed25519 signature over the exact
// input bytes against the PKIX public key. A signature of the wrong length
// or one that does not verify returns an error (nonzero exit).
func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	publicPath := fs.String("public", "", "path to the PKIX Ed25519 public key (PEM)")
	inPath := fs.String("in", "", "path to the signed input file")
	sigPath := fs.String("sig", "", "path to the raw 64-byte signature")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *publicPath == "" || *inPath == "" || *sigPath == "" {
		return errors.New("verify requires --public, --in, and --sig")
	}

	pub, err := loadPublicKey(*publicPath)
	if err != nil {
		return err
	}
	input, err := os.ReadFile(*inPath)
	if err != nil {
		return fmt.Errorf("reading input: %w", err)
	}
	sig, err := os.ReadFile(*sigPath)
	if err != nil {
		return fmt.Errorf("reading signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}

	if !ed25519.Verify(pub, input, sig) {
		return errors.New("signature did not verify")
	}
	return nil
}

// runVerifyKey parses a PKIX PEM file and confirms it is an Ed25519 public
// key of the expected length, so CI can assert the committed key's shape
// before a release.
func runVerifyKey(args []string) error {
	fs := flag.NewFlagSet("verify-key", flag.ContinueOnError)
	publicPath := fs.String("public", "", "path to the PKIX Ed25519 public key (PEM)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *publicPath == "" {
		return errors.New("verify-key requires --public")
	}

	if _, err := loadPublicKey(*publicPath); err != nil {
		return err
	}
	return nil
}

// loadPrivateKey reads a PEM file and returns the Ed25519 private key it
// holds, failing if the bytes are not a PKCS#8 Ed25519 key.
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	der, err := readPEM(path, privateKeyPEMType)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not an ed25519 key")
	}
	return priv, nil
}

// loadPublicKey reads a PEM file and returns the Ed25519 public key it
// holds, failing if the bytes are not a PKIX Ed25519 key of the expected
// length.
func loadPublicKey(path string) (ed25519.PublicKey, error) {
	der, err := readPEM(path, publicKeyPEMType)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parsing public key: %w", err)
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("public key is not an ed25519 key")
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ed25519 public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	return pub, nil
}

// readPEM reads path, decodes a single PEM block, and returns its DER bytes,
// failing if the file holds no PEM block of the wanted type.
func readPEM(path, wantType string) ([]byte, error) {
	// G304 is suppressed: path is an operator-supplied CLI key/input path;
	// reading it is this build-time helper's whole purpose.
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied CLI input path
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", wantType, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}
	if block.Type != wantType {
		return nil, fmt.Errorf("expected a %q PEM block, got %q", wantType, block.Type)
	}
	return block.Bytes, nil
}
