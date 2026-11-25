package cursor

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

type fakeProxyRegistry struct {
	values              map[string]any
	failRead, failWrite string
	writes              []string
}

func (r *fakeProxyRegistry) GetIntegerValue(n string) (uint64, uint32, error) {
	if n == r.failRead {
		return 0, 0, errors.New("access denied")
	}
	v, ok := r.values[n]
	if !ok {
		return 0, 0, os.ErrNotExist
	}
	return uint64(v.(uint32)), 0, nil
}
func (r *fakeProxyRegistry) GetStringValue(n string) (string, uint32, error) {
	if n == r.failRead {
		return "", 0, errors.New("access denied")
	}
	v, ok := r.values[n]
	if !ok {
		return "", 0, os.ErrNotExist
	}
	return v.(string), 0, nil
}
func (r *fakeProxyRegistry) SetDWordValue(n string, v uint32) error  { return r.set(n, v) }
func (r *fakeProxyRegistry) SetStringValue(n string, v string) error { return r.set(n, v) }
func (r *fakeProxyRegistry) set(n string, v any) error {
	r.writes = append(r.writes, n)
	if n == r.failWrite {
		return errors.New("write denied")
	}
	r.values[n] = v
	return nil
}
func (r *fakeProxyRegistry) DeleteValue(n string) error {
	if n == r.failWrite {
		return errors.New("delete denied")
	}
	if _, ok := r.values[n]; !ok {
		return os.ErrNotExist
	}
	delete(r.values, n)
	return nil
}

func TestProxySnapshotFailsClosedOnReadError(t *testing.T) {
	for _, name := range []string{"ProxyEnable", "ProxyServer", "ProxyOverride"} {
		t.Run(name, func(t *testing.T) {
			r := &fakeProxyRegistry{values: map[string]any{}, failRead: name}
			if _, err := readProxyValues(r); err == nil {
				t.Fatal("unreadable value must not become missing backup value")
			}
			if len(r.writes) != 0 {
				t.Fatal("snapshot wrote settings")
			}
		})
	}
}
func TestProxyRestorePreservesExistingSettingsAndAbsence(t *testing.T) {
	for _, initial := range []map[string]any{{}, {"ProxyEnable": uint32(1), "ProxyServer": "corp.example:8888", "ProxyOverride": "*.internal;<local>"}} {
		r := &fakeProxyRegistry{values: map[string]any{}}
		for k, v := range initial {
			r.values[k] = v
		}
		backup, err := readProxyValues(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := applyProxyValues(r, "127.0.0.1:18080"); err != nil {
			t.Fatal(err)
		}
		if r.writes[len(r.writes)-1] != "ProxyEnable" {
			t.Fatal("proxy enabled before destination was configured")
		}
		if err := restoreProxyValues(r, backup); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(r.values, initial) {
			t.Fatalf("restored %v; wanted %v", r.values, initial)
		}
	}
}
func TestFailedProxyRestoreDoesNotReenableBrokenDestination(t *testing.T) {
	r := &fakeProxyRegistry{values: map[string]any{"ProxyEnable": uint32(1), "ProxyServer": "127.0.0.1:18080", "ProxyOverride": "<local>"}, failWrite: "ProxyServer"}
	bk := sysProxyBackup{HasEnable: true, Enable: 1, HasServer: true, Server: "corp.example:8888", HasOverride: true, Override: "*.internal"}
	if err := restoreProxyValues(r, bk); err == nil {
		t.Fatal("failed restore should be surfaced")
	}
	if r.values["ProxyEnable"] != uint32(0) {
		t.Fatal("proxy reenabled with unrecovered destination")
	}
	if r.values["ProxyOverride"] != "*.internal" {
		t.Fatal("independent restore not attempted after failure")
	}
}
