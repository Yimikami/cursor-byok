package certs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreatePreservesExistingIdentity(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("CA changed on reload")
	}
}

func TestLoadOrCreateNeverReplacesDamagedPair(t *testing.T) {
	for _, mode := range []string{"missing-key", "invalid-key", "mismatched-key"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ca, err := LoadOrCreate(dir)
			if err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(dir, "ca.key")
			switch mode {
			case "missing-key":
				err = os.Remove(keyPath)
			case "invalid-key":
				err = os.WriteFile(keyPath, []byte("broken key"), 0600)
			case "mismatched-key":
				other, e := generate()
				if e != nil {
					t.Fatal(e)
				}
				err = os.WriteFile(keyPath, other.KeyPEM(), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreate(dir); err == nil {
				t.Fatal("damaged CA should fail without regenerating")
			}
			raw, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, ca.CertPEM()) {
				t.Fatal("original CA was overwritten")
			}
			if mode == "missing-key" {
				if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing private key was silently regenerated")
				}
			}
		})
	}
}
