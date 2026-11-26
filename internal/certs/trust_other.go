//go:build !darwin || !cgo

package certs

import (
	"crypto/x509"
	"fmt"
)

func queryDarwinTrust(*x509.Certificate, darwinTrustDomain) (darwinTrustRecord, error) {
	return darwinTrustRecord{}, fmt.Errorf("exact macOS CA trust inspection requires a build with Keychain support")
}
