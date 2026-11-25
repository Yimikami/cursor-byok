package certs

import (
	"bytes"
	"crypto/x509"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMacTrustDoesNotConfuseCachedEvaluationWithExplicitSSLTrust(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ca.Dir(), "ca.crt")
	for _, tc := range []struct {
		name          string
		user, admin   darwinTrustRecord
		want          bool
		wantEvaluated bool
	}{
		{"removed-but-evaluator-cached", darwinTrustRecord{}, darwinTrustRecord{}, false, false},
		{"user-ssl", darwinTrustRecord{true, 1}, darwinTrustRecord{}, true, true},
		{"legacy-admin-ssl", darwinTrustRecord{}, darwinTrustRecord{true, 1}, true, true},
		{"user-deny-overrides-admin", darwinTrustRecord{true, -1}, darwinTrustRecord{true, 1}, false, false},
		{"non-ssl-only", darwinTrustRecord{true, 0}, darwinTrustRecord{}, false, false},
		{"non-ssl-user-allows-admin-fallback", darwinTrustRecord{true, 0}, darwinTrustRecord{true, 1}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evaluated := false
			query := func(cert *x509.Certificate, domain darwinTrustDomain) (darwinTrustRecord, error) {
				if !bytes.Equal(cert.Raw, ca.Cert.Raw) {
					t.Fatal("queried a different certificate")
				}
				if domain == darwinUserTrust {
					return tc.user, nil
				}
				return tc.admin, nil
			}
			got := isDarwinRootTrusted(path, ca.Cert, query, func(name string, args ...string) ([]byte, error) {
				evaluated = true
				if name != "security" || !reflect.DeepEqual(args, []string{"verify-cert", "-q", "-L", "-l", "-p", "ssl", "-c", path}) {
					t.Fatalf("wrong certificate or policy evaluation: %s %v", name, args)
				}
				return nil, nil // Reproduce the stale-success evaluator from the live failure.
			})
			if got != tc.want || evaluated != tc.wantEvaluated {
				t.Fatalf("installed=%v evaluated=%v", got, evaluated)
			}
		})
	}
}

func TestMacUninstallRemovesOnlyPresentExactTrustDomains(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ca.Dir(), "ca.crt")
	for _, tc := range []struct {
		name         string
		user, admin  bool
		wantCommands [][]string
	}{
		{"already-removed", false, false, nil},
		{"user-only-no-admin-prompt", true, false, [][]string{{"remove-trusted-cert", path}}},
		{"legacy-admin-only", false, true, [][]string{{"remove-trusted-cert", "-d", path}}},
		{"both-domains", true, true, [][]string{{"remove-trusted-cert", path}, {"remove-trusted-cert", "-d", path}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			present := map[darwinTrustDomain]bool{darwinUserTrust: tc.user, darwinAdminTrust: tc.admin}
			var commands [][]string
			query := func(cert *x509.Certificate, domain darwinTrustDomain) (darwinTrustRecord, error) {
				if !bytes.Equal(cert.Raw, ca.Cert.Raw) {
					t.Fatal("queried a different certificate")
				}
				return darwinTrustRecord{present: present[domain]}, nil
			}
			err := uninstallDarwinRoot(path, ca.Cert, query, func(name string, args ...string) ([]byte, error) {
				if name != "security" {
					t.Fatalf("unexpected command %s", name)
				}
				commands = append(commands, args)
				domain := darwinUserTrust
				if len(args) == 3 {
					domain = darwinAdminTrust
				}
				present[domain] = false
				return nil, nil
			})
			if err != nil || !reflect.DeepEqual(commands, tc.wantCommands) || present[darwinUserTrust] || present[darwinAdminTrust] {
				t.Fatalf("removal commands=%v err=%v remaining=%v", commands, err, present)
			}
		})
	}
}

func TestMacUninstallVerifiesRemovalAndAllowsRetry(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ca.Dir(), "ca.crt")
	present := true
	query := func(*x509.Certificate, darwinTrustDomain) (darwinTrustRecord, error) {
		return darwinTrustRecord{present: present}, nil
	}
	denied := errors.New("permission denied")
	for _, commandErr := range []error{nil, denied} {
		if err := uninstallDarwinRoot(path, ca.Cert, query, func(string, ...string) ([]byte, error) { return nil, commandErr }); err == nil {
			t.Fatal("reported success while exact trust record remained")
		}
	}
	// A retry can race another successful remover. Check absence instead of
	// rejecting a harmless command-level not-found response.
	if err := uninstallDarwinRoot(path, ca.Cert, query, func(string, ...string) ([]byte, error) {
		present = false
		return nil, errors.New("item not found")
	}); err != nil {
		t.Fatal(err)
	}
	if isDarwinRootTrusted(path, ca.Cert, func(*x509.Certificate, darwinTrustDomain) (darwinTrustRecord, error) {
		return darwinTrustRecord{}, denied
	}, func(string, ...string) ([]byte, error) { t.Fatal("evaluation after query failure"); return nil, nil }) {
		t.Fatal("failed trust query accepted")
	}
}
