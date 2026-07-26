package mitm

import "testing"

func TestShouldMITMHost(t *testing.T) {
	// Cursor's own hosts must be intercepted, with or without a port and
	// regardless of case.
	intercepted := []string{
		"api2.cursor.sh",
		"api2.cursor.sh:443",
		"authentication.cursor.sh:443",
		"prod.authentication.cursor.sh:443",
		"API2.CURSOR.SH:443",
	}
	for _, h := range intercepted {
		if !shouldMITMHost(h) {
			t.Errorf("shouldMITMHost(%q) = false, want true", h)
		}
	}

	// Everything else — including unrelated apps and look-alike hostnames
	// that merely contain a Cursor host as a substring — must be tunnelled
	// through untouched so their real TLS is preserved.
	tunnelled := []string{
		"discord.com:443",
		"gateway.discord.gg:443",
		"www.google.com:443",
		"github.com:443",
		"cursor.sh:443",
		"api2.cursor.sh.attacker.example:443",
		"notapi2.cursor.sh:443",
	}
	for _, h := range tunnelled {
		if shouldMITMHost(h) {
			t.Errorf("shouldMITMHost(%q) = true, want false", h)
		}
	}
}
