package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"cursor-byok/internal/relay"
)

type lifecycleServer struct {
	run func(string) error
}

func (s *lifecycleServer) Start() error               { return s.run("listen") }
func (s *lifecycleServer) Stop(context.Context) error { return s.run("close") }

func testLifecycle(t *testing.T) (*ProxyService, *[]string, map[string]error) {
	t.Helper()
	var events []string
	failures := map[string]error{}
	run := func(name string) error {
		events = append(events, name)
		return failures[name]
	}
	s := &ProxyService{
		cfgDir: t.TempDir(),
		gatewayAdapters: func() []relay.AdapterInfo {
			return []relay.AdapterInfo{{ModelID: "test-model"}}
		},
		lifecycle: &proxyLifecycle{
			caInstalled: func() bool { return true },
			newServer: func() (proxyServer, error) {
				if err := run("create"); err != nil {
					return nil, err
				}
				return &lifecycleServer{run: run}, nil
			},
			applyTweaks:  func(string) error { return run("settings") },
			revertTweaks: func() error { return run("restore-settings") },
			injectAuth:   func(string) error { return run("auth") },
			restoreAuth:  func(string) error { return run("restore-auth") },
			selectModel:  func(string) error { return run("model") },
			enableProxy:  func(string) error { return run("proxy") },
			disableProxy: func() error { return run("restore-proxy") },
		},
	}
	return s, &events, failures
}

func TestStartProxyChecksPrerequisitesBeforeOSChanges(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*ProxyService)
	}{
		{"no models", func(s *ProxyService) { s.gatewayAdapters = func() []relay.AdapterInfo { return nil } }},
		{"malformed config", func(s *ProxyService) {
			if err := os.WriteFile(filepath.Join(s.cfgDir, "config.json"), []byte(`{"broken":`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	if runtime.GOOS != "linux" {
		tests = append(tests, struct {
			name    string
			prepare func(*ProxyService)
		}{
			"untrusted CA", func(s *ProxyService) { s.lifecycle.caInstalled = func() bool { return false } },
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, events, _ := testLifecycle(t)
			tt.prepare(s)
			state, err := s.StartProxy()
			if err == nil || state.Running || len(*events) != 0 {
				t.Fatalf("state=%+v err=%v events=%v", state, err, *events)
			}
		})
	}
}

func TestStartProxyRollsBackEachFailedStep(t *testing.T) {
	for _, tt := range []struct {
		step string
		want []string
	}{
		{"create", []string{"create"}},
		{"listen", []string{"create", "listen"}},
		{"settings", []string{"create", "listen", "settings", "restore-settings", "close"}},
		{"auth", []string{"create", "listen", "settings", "auth", "restore-settings", "restore-auth", "close"}},
		{"model", []string{"create", "listen", "settings", "auth", "model", "restore-settings", "restore-auth", "close"}},
		{"proxy", []string{"create", "listen", "settings", "auth", "model", "proxy", "restore-proxy", "restore-settings", "restore-auth", "close"}},
	} {
		t.Run(tt.step, func(t *testing.T) {
			s, events, failures := testLifecycle(t)
			failures[tt.step] = errors.New("failed " + tt.step)
			state, err := s.StartProxy()
			if err == nil || state.Running || s.proxy != nil || !reflect.DeepEqual(*events, tt.want) {
				t.Fatalf("state=%+v err=%v events=%v, want %v", state, err, *events, tt.want)
			}
		})
	}
}

func TestFailedProxyRollbackKeepsListenerUntilStopRetry(t *testing.T) {
	s, events, failures := testLifecycle(t)
	failures["proxy"] = errors.New("partial registry write")
	failures["restore-proxy"] = errors.New("registry locked")
	state, err := s.StartProxy()
	if err == nil || !state.Running || s.proxy == nil || strings.Contains(strings.Join(*events, ","), "close") {
		t.Fatalf("state=%+v err=%v events=%v", state, err, *events)
	}
	delete(failures, "restore-proxy")
	state, err = s.StopProxy()
	if err != nil || state.Running || s.proxy != nil || (*events)[len(*events)-1] != "close" {
		t.Fatalf("state=%+v err=%v events=%v", state, err, *events)
	}
}

func TestStopRestorationFailureCanBeRetried(t *testing.T) {
	for _, step := range []string{"restore-proxy", "restore-settings", "restore-auth"} {
		t.Run(step, func(t *testing.T) {
			s, events, failures := testLifecycle(t)
			if _, err := s.StartProxy(); err != nil {
				t.Fatal(err)
			}
			*events = nil
			failures[step] = errors.New("temporarily denied")
			state, err := s.StopProxy()
			want := []string{"restore-proxy", "restore-settings", "restore-auth"}
			if err == nil || !state.Running || !reflect.DeepEqual(*events, want) {
				t.Fatalf("state=%+v err=%v events=%v", state, err, *events)
			}
			delete(failures, step)
			state, err = s.StopProxy()
			if err != nil || state.Running || state.LastError != "" || (*events)[len(*events)-1] != "close" {
				t.Fatalf("state=%+v err=%v events=%v", state, err, *events)
			}
		})
	}
}

func TestSuccessfulLifecycleNotifiesOutsideLock(t *testing.T) {
	s, events, _ := testLifecycle(t)
	var notified []bool
	s.onStateChange = func(running bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		notified = append(notified, running)
	}
	if _, err := s.StartProxy(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartProxy(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopProxy(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopProxy(); err != nil {
		t.Fatal(err)
	}
	want := []string{"create", "listen", "settings", "auth", "model", "proxy", "restore-proxy", "restore-settings", "restore-auth", "close"}
	if !reflect.DeepEqual(*events, want) || !reflect.DeepEqual(notified, []bool{true, false}) {
		t.Fatalf("events=%v notified=%v", *events, notified)
	}
}

func TestInterruptedSessionRecoveryAttemptsAllRestorations(t *testing.T) {
	s, events, failures := testLifecycle(t)
	failures["restore-proxy"] = errors.New("locked registry")
	err := RecoverInterruptedSession(s)
	want := []string{"restore-proxy", "restore-settings", "restore-auth"}
	if err == nil || s.state.Running || s.state.LastError == "" || !reflect.DeepEqual(*events, want) {
		t.Fatalf("state=%+v err=%v events=%v", s.state, err, *events)
	}
	*events = nil
	s.state.Running = true
	if err := RecoverInterruptedSession(s); err != nil || len(*events) != 0 {
		t.Fatalf("recovery touched a running session: err=%v events=%v", err, *events)
	}
}

func TestStartupRecoversBeforeLoadingDamagedCA(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ca"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca", "ca.crt"), []byte("damaged certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered := false
	recoveryFailure := errors.New("recovery failure must remain visible")
	_, err := newProxyService(dir, func(s *ProxyService) error {
		recovered = s.cfgDir == dir
		return recoveryFailure
	})
	if !recovered || err == nil || !errors.Is(err, recoveryFailure) {
		t.Fatalf("recovered=%v err=%v", recovered, err)
	}
}

func TestStoppedRecoveryCanBeRetried(t *testing.T) {
	s, events, failures := testLifecycle(t)
	failures["restore-proxy"] = errors.New("temporary access denial")
	if err := RecoverInterruptedSession(s); err == nil {
		t.Fatal("expected startup recovery failure")
	}
	*events = nil
	state, err := s.StopProxy()
	want := []string{"restore-proxy", "restore-settings", "restore-auth"}
	if err == nil || state.Running || !s.recoveryPending || !reflect.DeepEqual(*events, want) {
		t.Fatalf("state=%+v err=%v pending=%v events=%v", state, err, s.recoveryPending, *events)
	}
	delete(failures, "restore-proxy")
	state, err = s.StopProxy()
	if err != nil || state.Running || s.recoveryPending || state.LastError != "" {
		t.Fatalf("state=%+v err=%v pending=%v", state, err, s.recoveryPending)
	}
}

func TestStartRetriesPendingRecoveryBeforeNewOverrides(t *testing.T) {
	s, events, failures := testLifecycle(t)
	failures["restore-proxy"] = errors.New("temporary access denial")
	if err := RecoverInterruptedSession(s); err == nil {
		t.Fatal("expected startup recovery failure")
	}
	*events = nil
	state, err := s.StartProxy()
	want := []string{"restore-proxy", "restore-settings", "restore-auth"}
	if err == nil || state.Running || !reflect.DeepEqual(*events, want) {
		t.Fatalf("state=%+v err=%v events=%v", state, err, *events)
	}
	delete(failures, "restore-proxy")
	*events = nil
	state, err = s.StartProxy()
	want = append(want, "create", "listen", "settings", "auth", "model", "proxy")
	if err != nil || !state.Running || s.recoveryPending || !reflect.DeepEqual(*events, want) {
		t.Fatalf("state=%+v err=%v pending=%v events=%v", state, err, s.recoveryPending, *events)
	}
}
