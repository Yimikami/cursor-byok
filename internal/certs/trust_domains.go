package certs

import (
	"crypto/x509"
	"fmt"
)

type darwinTrustDomain int

const (
	darwinUserTrust darwinTrustDomain = iota
	darwinAdminTrust
)

type darwinTrustRecord struct {
	present bool
	ssl     int // -1 denied, 0 unspecified, 1 trusted for SSL.
}

type darwinTrustQuery func(*x509.Certificate, darwinTrustDomain) (darwinTrustRecord, error)

func isDarwinRootTrusted(path string, cert *x509.Certificate, query darwinTrustQuery, run func(string, ...string) ([]byte, error)) bool {
	// User settings take precedence over admin settings. A general SecTrust
	// evaluation can retain trust after these records disappear; Chromium's
	// local-anchor lookup requires the explicit SSL trust record as well.
	for _, domain := range []darwinTrustDomain{darwinUserTrust, darwinAdminTrust} {
		record, err := query(cert, domain)
		if err != nil || record.ssl < 0 {
			return false
		}
		if record.present && record.ssl > 0 {
			_, err := run("security", "verify-cert", "-q", "-L", "-l", "-p", "ssl", "-c", path)
			return err == nil
		}
	}
	return false
}

func uninstallDarwinRoot(path string, cert *x509.Certificate, query darwinTrustQuery, run func(string, ...string) ([]byte, error)) error {
	for _, domain := range []darwinTrustDomain{darwinUserTrust, darwinAdminTrust} {
		record, err := query(cert, domain)
		if err != nil {
			return err
		}
		if !record.present {
			continue
		}
		args := []string{"remove-trusted-cert"}
		label := "user"
		if domain == darwinAdminTrust {
			args = append(args, "-d")
			label = "legacy admin"
		}
		out, removeErr := run("security", append(args, path)...)
		remaining, err := query(cert, domain)
		if err != nil {
			return fmt.Errorf("verify %s CA trust removal: %w", label, err)
		}
		if remaining.present {
			if removeErr != nil {
				return fmt.Errorf("remove %s CA trust: %w (%s)", label, removeErr, trim(out))
			}
			return fmt.Errorf("%s CA trust remains after removal", label)
		}
		// A concurrent removal can make the command report not-found even
		// though the exact domain is now clean. Absence is the success check.
	}
	return nil
}
