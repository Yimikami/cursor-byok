package cursor

import "testing"

func TestAuthEnvelopeUnknownVersionsFailClosed(t *testing.T) {
	called := false
	decrypt := func(string, []byte) (string, error) { called = true; return "", nil }
	for _, value := range []string{"cursor-safe-storage:v2:AAAA", "cursor-safe-storage:", "cursor-safe-storage:v1:%%%"} {
		if _, err := decodeCursorAuthTokenWith("unused", value, decrypt); err == nil {
			t.Fatal("unknown or malformed encrypted envelope treated as plaintext")
		}
	}
	if called {
		t.Fatal("malformed envelope invoked native credential reader")
	}
	if got, err := decodeCursorAuthTokenWith("unused", "existing-plaintext-session", decrypt); err != nil || got != "existing-plaintext-session" {
		t.Fatal("plain session compatibility changed")
	}
}
