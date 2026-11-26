package cursor

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestExplicitAuthPreparationRequestsPermissionOnceThenRetries(t *testing.T) {
	for _, status := range []int32{-25293, -25308, -25315} {
		for _, retryFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/retry-fails=%v", status, retryFails), func(t *testing.T) {
				var events []string
				denied := &CursorKeychainAccessError{Status: status}
				prepare := func() error {
					events = append(events, "prepare")
					if len(events) == 1 || retryFails {
						return fmt.Errorf("account preflight: %w", denied)
					}
					return nil
				}
				err := prepareCursorAuthWithPermission(prepare, func() error {
					events = append(events, "request-access")
					return nil
				})
				if retryFails && !errors.Is(err, denied) || !retryFails && err != nil {
					t.Fatalf("retry result lost: %v", err)
				}
				if !reflect.DeepEqual(events, []string{"prepare", "request-access", "prepare"}) {
					t.Fatalf("permission was repeated or preparation skipped: %v", events)
				}
			})
		}
	}
}

func TestAuthPermissionRetryExplainsPersistentAccessOnlyAfterGrant(t *testing.T) {
	for _, status := range []int32{-25293, -25308, -25315} {
		denied := &CursorKeychainAccessError{Status: status}
		for _, grant := range []bool{false, true} {
			err := prepareCursorAuthWithPermission(func() error { return denied }, func() error {
				if grant {
					return nil
				}
				return denied
			})
			if !errors.Is(err, denied) || strings.Contains(err.Error(), "Always Allow") != grant {
				t.Fatalf("unexpected persistent-access hint: granted=%v status=%d err=%v", grant, status, err)
			}
		}
	}
	for _, retryErr := range []error{errors.New("invalid token padding"), &CursorKeychainAccessError{Status: -25300}} {
		calls := 0
		err := prepareCursorAuthWithPermission(func() error {
			calls++
			if calls == 1 {
				return &CursorKeychainAccessError{Status: -25308}
			}
			return retryErr
		}, func() error { return nil })
		if err != retryErr || strings.Contains(err.Error(), "Always Allow") {
			t.Fatalf("nonpermission failure received an approval hint: %v", err)
		}
	}
}

func TestExplicitAuthPreparationStopsImmediatelyOnPermissionDenial(t *testing.T) {
	var events []string
	cancelled := errors.New("user denied Keychain access")
	err := prepareCursorAuthWithPermission(func() error {
		events = append(events, "prepare")
		return &CursorKeychainAccessError{Status: -25308}
	}, func() error {
		events = append(events, "request-access")
		return cancelled
	})
	if !errors.Is(err, cancelled) || !reflect.DeepEqual(events, []string{"prepare", "request-access"}) {
		t.Fatalf("denial triggered dependent work or hid the error: events=%v err=%v", events, err)
	}
}

func TestExplicitAuthPreparationNeverPromptsForOtherResults(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"malformed-envelope", errors.New("invalid Cursor safeStorage envelope")},
		{"no-cgo", errors.New("macOS Keychain support is unavailable in this build")},
		{"missing-item", &CursorKeychainAccessError{Status: -25300}},
		{"unknown-status", &CursorKeychainAccessError{Status: -50}},
		{"already-cancelled", &CursorKeychainAccessError{Status: -128}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepares := 0
			err := prepareCursorAuthWithPermission(func() error {
				prepares++
				return tc.err
			}, func() error {
				t.Fatal("unexpected permission prompt")
				return nil
			})
			if err != tc.err || prepares != 1 {
				t.Fatalf("nonpermission result changed: prepares=%d err=%v", prepares, err)
			}
		})
	}
}
