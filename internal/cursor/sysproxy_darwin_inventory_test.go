//go:build darwin

package cursor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testDarwinInventory(t *testing.T) darwinNetworkInventory {
	t.Helper()
	inventory, err := parseDarwinNetworkInventory([]byte(`{
  "CurrentSet":"/Sets/current",
  "NetworkServices":{
   "wifi-id":{"UserDefinedName":"Renamed Wi-Fi"},
   "disabled-id":{"UserDefinedName":"USB Ethernet","__INACTIVE__":1},
   "other-location-id":{"UserDefinedName":"Office Wi-Fi"},
   "new-id":{"UserDefinedName":"Deleted Adapter"}
  },
  "Sets":{"current":{"Network":{"Service":{"wifi-id":{},"disabled-id":{},"new-id":{}}}},"office":{"Network":{"Service":{"other-location-id":{}}}}}
 }`))
	if err != nil {
		t.Fatal(err)
	}
	return inventory.withNativeServiceNames(map[string]string{
		"wifi-id":           "Renamed Wi-Fi",
		"disabled-id":       "USB Ethernet",
		"other-location-id": "Office Wi-Fi",
		"new-id":            "Deleted Adapter",
	})
}

func TestDarwinInventoryUsesNativeNamesByIdentity(t *testing.T) {
	inventory, err := parseDarwinNetworkInventory([]byte(`{
     "CurrentSet":"/Sets/current",
     "NetworkServices":{
      "phone-id":{"UserDefinedName":"iPhone","Interface":{"UserDefinedName":"iPhone USB"}},
      "work-phone-id":{"UserDefinedName":"Work phone","Interface":{"UserDefinedName":"iPhone USB"}},
      "legacy-id":{"UserDefinedName":"Built-in Ethernet","Interface":{"UserDefinedName":"Built-in Ethernet"}},
      "adaptor-id":{"UserDefinedName":"Ethernet Adaptor","Interface":{"UserDefinedName":"Ethernet Adaptor"}},
      "nameless-id":{"Interface":{}},
      "wifi-id":{"UserDefinedName":"Wi-Fi","Interface":{"UserDefinedName":"Wi-Fi"}},
      "work-wifi-id":{"UserDefinedName":"Work Wi-Fi","Interface":{"UserDefinedName":"Wi-Fi"}}
     },
     "Sets":{"current":{"Network":{"Service":{"phone-id":{},"work-phone-id":{},"legacy-id":{},"adaptor-id":{},"nameless-id":{},"wifi-id":{},"work-wifi-id":{}}}}}
    }`))
	if err != nil {
		t.Fatal(err)
	}
	nativeNames := map[string]string{
		"phone-id": "iPhone USB", "work-phone-id": "Work phone",
		"legacy-id": "Ethernet intégré", "adaptor-id": "Ethernet Adapter",
		"nameless-id": "USB 10/100 LAN", "wifi-id": "Wi-Fi", "work-wifi-id": "Work Wi-Fi",
	}
	inventory = inventory.withNativeServiceNames(nativeNames)
	// Duplicate interface aliases must not compete with the name supplied for
	// another UUID. Legacy/localized/fallback names need no heuristic matching.
	for id, wantName := range nativeNames {
		got, err := inventory.idForName(wantName)
		if err != nil || got != id {
			t.Fatalf("native name %q resolved to %q, want %q: %v", wantName, got, id, err)
		}
		name, deleted, err := inventory.restoreName(darwinServiceProxy{ServiceID: id, Service: "Old name"})
		if err != nil || deleted || name != wantName {
			t.Fatalf("restore did not use native name: %q deleted=%v err=%v", name, deleted, err)
		}
	}
}

func TestDarwinInventoryRejectsAmbiguousNativeNames(t *testing.T) {
	inventory := darwinNetworkInventory{
		services: map[string]string{"one": "Phone 1", "two": "Phone 2"},
		current:  map[string]bool{"one": true, "two": true},
	}
	inventory = inventory.withNativeServiceNames(map[string]string{"one": "iPhone USB", "two": "iPhone USB"})
	if _, err := inventory.idForName("iPhone USB"); err == nil {
		t.Fatal("ambiguous native name accepted")
	}
	backup := darwinSysProxyBackup{Services: []darwinServiceProxy{
		{ServiceID: "one", Service: "Phone 1"}, {ServiceID: "two", Service: "Phone 2"},
	}}
	remaining, err := restoreDarwinServiceProxies(backup, inventory, func(...string) error {
		t.Fatal("restored an ambiguous native name")
		return nil
	})
	if err == nil || !reflect.DeepEqual(remaining, backup) {
		t.Fatalf("ambiguous identities were not retained: remaining=%v error=%v", remaining, err)
	}
}

func TestDarwinRecoveryNativeNameFailureIsScopedAndRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	backup := darwinSysProxyBackup{Services: []darwinServiceProxy{
		{ServiceID: "wifi-id", Service: "Wi-Fi"},
		{ServiceID: "disabled-id", Service: "USB Ethernet"},
	}}
	if err := saveDarwinProxyBackup(path, backup); err != nil {
		t.Fatal(err)
	}
	// Native lookup can fail for one ID, or a service can disappear between
	// snapshots. Keep the known UUID but do not reuse its stored/previous name.
	inventory := testDarwinInventory(t).withNativeServiceNames(map[string]string{
		"disabled-id":   "USB Ethernet",
		"unobserved-id": "Renamed Wi-Fi",
	})
	if _, exists := inventory.services["unobserved-id"]; exists {
		t.Fatal("native names changed authoritative inventory membership")
	}
	var restored []string
	err := disableDarwinSystemProxyAt(path, func() (darwinNetworkInventory, error) { return inventory, nil }, func(args ...string) error {
		restored = append(restored, args[1])
		return nil
	})
	if err == nil || !reflect.DeepEqual(restored, []string{"USB Ethernet", "USB Ethernet"}) {
		t.Fatalf("unrelated recovery blocked or unresolved name used: names=%v error=%v", restored, err)
	}
	remaining, err := loadDarwinProxyBackup(path)
	if err != nil || !reflect.DeepEqual(remaining.Services, backup.Services[:1]) {
		t.Fatalf("unresolved identity not retained: remaining=%v error=%v", remaining, err)
	}
	err = disableDarwinSystemProxyAt(path, func() (darwinNetworkInventory, error) { return testDarwinInventory(t), nil }, func(args ...string) error {
		if args[1] != "Renamed Wi-Fi" {
			t.Fatalf("retried completed service: %v", args)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("successful recovery retained backup: %v", err)
	}
}

func TestDarwinInventoryDoesNotGuessNativeNamesFromStoredNames(t *testing.T) {
	inventory, err := parseDarwinNetworkInventory([]byte(`{
        "CurrentSet":"/Sets/current",
        "NetworkServices":{"wifi-id":{"UserDefinedName":"Built-in Wi-Fi","Interface":{"UserDefinedName":"Wi-Fi"}}},
        "Sets":{"current":{"Network":{"Service":{"wifi-id":{}}}}}
    }`))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Built-in Wi-Fi", "Wi-Fi"} {
		if _, err := inventory.idForName(name); err == nil {
			t.Fatalf("guessed native name %q", name)
		}
	}
	_, deleted, err := inventory.restoreName(darwinServiceProxy{ServiceID: "wifi-id", Service: "Wi-Fi"})
	if err == nil || deleted {
		t.Fatalf("missing native name was not retained: deleted=%v error=%v", deleted, err)
	}
}

func TestDarwinProxyRecoveryResolvesRenameAndKeepsDisabledServices(t *testing.T) {
	bk := darwinSysProxyBackup{Services: []darwinServiceProxy{
		{Service: "Wi-Fi", ServiceID: "wifi-id", HTTPEnabled: true, HTTPServer: "corp.example", HTTPPort: "8888"},
		{Service: "USB Ethernet", ServiceID: "disabled-id"},
		// The old UUID was deleted even though a new service reused its name.
		{Service: "Deleted Adapter", ServiceID: "deleted-id"},
	}}
	var calls [][]string
	remaining, err := restoreDarwinServiceProxies(bk, testDarwinInventory(t), func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	})
	if err != nil || len(remaining.Services) != 0 {
		t.Fatalf("remaining=%+v err=%v", remaining, err)
	}
	want := [][]string{
		{"-setwebproxy", "Renamed Wi-Fi", "corp.example", "8888"},
		{"-setwebproxystate", "Renamed Wi-Fi", "on"},
		{"-setsecurewebproxystate", "Renamed Wi-Fi", "off"},
		{"-setwebproxystate", "USB Ethernet", "off"},
		{"-setsecurewebproxystate", "USB Ethernet", "off"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v; want %v", calls, want)
	}
}

func TestDarwinRecoveryRetainsAmbiguousLegacyAndOtherLocations(t *testing.T) {
	bk := darwinSysProxyBackup{Services: []darwinServiceProxy{
		{Service: "Original old name"},
		{Service: "Office Wi-Fi", ServiceID: "other-location-id"},
	}}
	called := false
	remaining, err := restoreDarwinServiceProxies(bk, testDarwinInventory(t), func(...string) error { called = true; return nil })
	if err == nil || !reflect.DeepEqual(remaining, bk) || called {
		t.Fatalf("remaining=%+v err=%v called=%v", remaining, err, called)
	}
}

func TestDarwinRecoveryPersistsOnlyCommandFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	bk := darwinSysProxyBackup{Services: []darwinServiceProxy{
		{Service: "Wi-Fi", ServiceID: "wifi-id"},
		{Service: "USB Ethernet", ServiceID: "disabled-id"},
		{Service: "Deleted Adapter", ServiceID: "deleted-id"},
	}}
	if err := saveDarwinProxyBackup(path, bk); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	inventory := func() (darwinNetworkInventory, error) { return testDarwinInventory(t), nil }
	failure := errors.New("permission temporarily denied")
	err := disableDarwinSystemProxyAt(path, inventory, func(args ...string) error {
		attempts++
		if args[1] == "USB Ethernet" {
			return failure
		}
		return nil
	})
	if !errors.Is(err, failure) {
		t.Fatalf("got %v", err)
	}
	remaining, err := loadDarwinProxyBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Services) != 1 || remaining.Services[0].ServiceID != "disabled-id" {
		t.Fatalf("wrong recovery entries: %+v", remaining)
	}
	if attempts != 4 {
		t.Fatalf("unexpected restore attempts: %d", attempts)
	}
	// Permission becomes available; a retry touches only the unresolved service.
	err = disableDarwinSystemProxyAt(path, inventory, func(args ...string) error {
		if args[1] != "USB Ethernet" {
			t.Fatalf("retried completed service: %v", args)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("successful recovery retained backup: %v", err)
	}
}

func TestDarwinInventoryFailureDoesNotDiscardBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	bk := darwinSysProxyBackup{Services: []darwinServiceProxy{{Service: "Wi-Fi", ServiceID: "wifi-id"}}}
	if err := saveDarwinProxyBackup(path, bk); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("plutil could not read preferences")
	err = disableDarwinSystemProxyAt(path, func() (darwinNetworkInventory, error) { return darwinNetworkInventory{}, failure }, func(...string) error { t.Fatal("mutated settings without inventory"); return nil })
	if !errors.Is(err, failure) {
		t.Fatalf("got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("backup changed after failed inventory")
	}
}

func TestDarwinMissingBackupDoesNotInspectOrMutateOS(t *testing.T) {
	err := disableDarwinSystemProxyAt(filepath.Join(t.TempDir(), "missing.json"), func() (darwinNetworkInventory, error) {
		t.Fatal("unexpected OS inventory")
		return darwinNetworkInventory{}, nil
	}, func(...string) error { t.Fatal("unexpected OS write"); return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestDarwinInventoryRejectsIncompleteDeletionEvidence(t *testing.T) {
	for _, input := range []string{`{}`, `{"NetworkServices":{}}`, `{"CurrentSet":"/Sets/missing","NetworkServices":{},"Sets":{}}`, `{"CurrentSet":"/Sets/current","NetworkServices":{},"Sets":{"current":{"Network":{"Service":{"unknown-id":{}}}}}}`} {
		if _, err := parseDarwinNetworkInventory([]byte(input)); err == nil {
			t.Fatalf("accepted incomplete inventory: %s", input)
		}
	}
	// A complete, empty inventory does establish that a previously known UUID
	// is gone. Being disabled, however, does not remove a UUID from the map.
	inventory, err := parseDarwinNetworkInventory([]byte(`{"CurrentSet":"/Sets/current","NetworkServices":{},"Sets":{"current":{"Network":{"Service":{}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, deleted, err := inventory.restoreName(darwinServiceProxy{Service: "Wi-Fi", ServiceID: "wifi-id"})
	if err != nil || !deleted {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
}

func TestDarwinRestoreFailureNeverReenablesUnrestoredDestination(t *testing.T) {
	bk := darwinSysProxyBackup{Services: []darwinServiceProxy{{Service: "Wi-Fi", ServiceID: "wifi-id", HTTPEnabled: true, HTTPServer: "corp", HTTPPort: "8888"}}}
	var calls []string
	remaining, err := restoreDarwinServiceProxies(bk, testDarwinInventory(t), func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "-setwebproxy" {
			return errors.New("denied")
		}
		return nil
	})
	if err == nil || len(remaining.Services) != 1 {
		t.Fatalf("remaining=%v err=%v", remaining, err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "-setwebproxystate Renamed Wi-Fi off") {
		t.Fatalf("unrestored proxy not disabled: %v", calls)
	}
}
