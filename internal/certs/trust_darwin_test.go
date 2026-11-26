//go:build darwin && cgo

package certs

import "testing"

func TestMacExactTrustQueryForNeverInstalledCertificate(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range []darwinTrustDomain{darwinUserTrust, darwinAdminTrust} {
		record, err := queryDarwinTrust(ca.Cert, domain)
		if err != nil || record.present || record.ssl != 0 {
			t.Fatalf("never-installed certificate: domain=%d record=%+v err=%v", domain, record, err)
		}
	}
}
