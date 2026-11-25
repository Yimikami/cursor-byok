package certs

import (
	"crypto/x509"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWindowsTrustChecksExactCertificate(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ca.Dir(), "ca.crt")
	called := false
	run := func(name string, args ...string) ([]byte, error) {
		called = true
		want := []string{"-user", "-store", "Root", certificateStoreID(ca.Cert)}
		if name != "certutil" || !reflect.DeepEqual(args, want) {
			t.Fatalf("command: %s %v", name, args)
		}
		// A same-name old CA can exist, but the requested fingerprint is absent.
		return []byte(caCommonName), errors.New("requested certificate not found")
	}
	if isInstalledUserRoot("windows", path, run) {
		t.Fatal("old same-name certificate incorrectly accepted")
	}
	if !called {
		t.Fatal("certificate store was not checked")
	}
	if !isInstalledUserRoot("windows", path, func(string, ...string) ([]byte, error) { return nil, nil }) {
		t.Fatal("matching root rejected")
	}
}

func TestMacTrustChecksTrustInsteadOfPresence(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ca.Dir(), "ca.crt")
	run := func(name string, args ...string) ([]byte, error) {
		want := []string{"verify-cert", "-q", "-L", "-l", "-p", "ssl", "-c", path}
		if name != "security" || !reflect.DeepEqual(args, want) {
			t.Fatalf("command: %s %v", name, args)
		}
		return nil, errors.New("certificate exists but is untrusted")
	}
	if isDarwinRootTrusted(path, ca.Cert, func(*x509.Certificate, darwinTrustDomain) (darwinTrustRecord, error) {
		return darwinTrustRecord{present: true, ssl: 1}, nil
	}, run) {
		t.Fatal("untrusted keychain certificate accepted")
	}
}

func TestLinuxRejectsStaleNSSCertificate(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	if isInstalledUserRoot("linux", filepath.Join(ca.Dir(), "ca.crt"), func(string, ...string) ([]byte, error) {
		return old.CertPEM(), nil
	}) {
		t.Fatal("same-name stale NSS CA accepted")
	}
}
