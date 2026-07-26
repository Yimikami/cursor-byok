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

// TestCursorHostClassification guards the request-handler routing layer: the
// api2/auth classification must agree with the CONNECT-time MITM decision for
// differently-cased hosts and non-443 ports, so an intercepted Cursor request
// is never left to fall through to the upstream.
func TestCursorHostClassification(t *testing.T) {
	api2 := []string{"api2.cursor.sh", "api2.cursor.sh:443", "API2.CURSOR.SH:443", "api2.cursor.sh:8443"}
	for _, h := range api2 {
		if !isCursorAPI2Host(h) {
			t.Errorf("isCursorAPI2Host(%q) = false, want true", h)
		}
		if isCursorAuthHost(h) {
			t.Errorf("isCursorAuthHost(%q) = true, want false", h)
		}
	}

	auth := []string{"authentication.cursor.sh:443", "AUTHENTICATION.CURSOR.SH:443", "prod.authentication.cursor.sh:8443"}
	for _, h := range auth {
		if !isCursorAuthHost(h) {
			t.Errorf("isCursorAuthHost(%q) = false, want true", h)
		}
		if isCursorAPI2Host(h) {
			t.Errorf("isCursorAPI2Host(%q) = true, want false", h)
		}
	}

	for _, h := range []string{"discord.com:443", "cursor.sh:443", "api2.cursor.sh.attacker.example:443"} {
		if isCursorAPI2Host(h) || isCursorAuthHost(h) {
			t.Errorf("host %q must not classify as a Cursor host", h)
		}
	}
}
