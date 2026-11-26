//go:build !darwin

package cursor

func requestCursorKeychainAccess() error { return nil }
