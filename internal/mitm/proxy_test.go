package mitm

import "testing"

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"api2.cursor.sh":      "api2.cursor.sh",
		"api2.cursor.sh:443":  "api2.cursor.sh",
		"API2.CURSOR.SH:443":  "api2.cursor.sh",
		"Api2.Cursor.Sh:8443": "api2.cursor.sh",
		"discord.com:443":     "discord.com",
		"[::1]:443":           "::1",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShouldMITMHost(t *testing.T) {
	// The BYOK API host must be intercepted, with or without a port and
	// regardless of case.
	intercepted := []string{
		"api2.cursor.sh",
		"api2.cursor.sh:443",
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
		"authentication.cursor.sh:443",
		"prod.authentication.cursor.sh:443",
		"AUTHENTICATION.CURSOR.SH:443",
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

// TestCursorHostClassification guards the request-handler routing layer: the
// API classification agrees with the CONNECT-time MITM decision. Native auth
// hosts must remain outside this boundary so login and refresh are untouched.
func TestCursorHostClassification(t *testing.T) {
	api2 := []string{"api2.cursor.sh", "api2.cursor.sh:443", "API2.CURSOR.SH:443", "api2.cursor.sh:8443"}
	for _, h := range api2 {
		if !isCursorAPI2Host(h) {
			t.Errorf("isCursorAPI2Host(%q) = false, want true", h)
		}
	}

	auth := []string{"authentication.cursor.sh:443", "AUTHENTICATION.CURSOR.SH:443", "prod.authentication.cursor.sh:8443"}
	for _, h := range auth {
		if isCursorAPI2Host(h) || shouldMITMHost(h) {
			t.Errorf("authentication host %q must remain tunnelled", h)
		}
	}

	for _, h := range []string{"discord.com:443", "cursor.sh:443", "api2.cursor.sh.attacker.example:443"} {
		if isCursorAPI2Host(h) {
			t.Errorf("host %q must not classify as a Cursor host", h)
		}
	}
}
