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

	"cursor-byok/internal/cursor"
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
			prepareAuth:  func(string) error { return run("auth") },
			restoreAuth:  func(string) error { return run("restore-auth") },
			authAccess:   func() error { return run("auth-access") },
			selectModel:  func(string) error { return run("model") },
			restoreModel: func(string) error { return run("restore-model") },
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
		{"settings", []string{"create", "listen", "auth", "settings", "restore-settings", "restore-auth", "close"}},
		{"auth", []string{"create", "listen", "auth", "restore-auth", "close"}},
		{"model", []string{"create", "listen", "auth", "settings", "proxy", "model", "restore-proxy", "restore-settings", "restore-model", "restore-auth", "close"}},
		{"proxy", []string{"create", "listen", "auth", "settings", "proxy", "restore-proxy", "restore-settings", "restore-auth", "close"}},
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

func TestAuthPreflightFailureLeavesRoutingUntouchedAndCanRetry(t *testing.T) {
	s, events, failures := testLifecycle(t)
	denied := errors.New("Cursor Safe Storage is unavailable without Keychain access")
	failures["auth"] = denied
	state, err := s.StartProxy()
	want := []string{"create", "listen", "auth", "restore-auth", "close"}
	if !errors.Is(err, denied) || state.Running || s.proxy != nil || state.LastError == "" || !reflect.DeepEqual(*events, want) {
		t.Fatalf("failed auth preflight changed routing or hid its error: state=%+v err=%v events=%v", state, err, *events)
	}
	// An explicit retry after access becomes available should perform the
	// preflight again and only then apply either proxy configuration.
	delete(failures, "auth")
	*events = nil
	state, err = s.StartProxy()
	want = []string{"create", "listen", "auth", "settings", "proxy", "model"}
	if err != nil || !state.Running || state.LastError != "" || !reflect.DeepEqual(*events, want) {
		t.Fatalf("auth preflight retry failed: state=%+v err=%v events=%v", state, err, *events)
	}
}

func TestExplicitStartAuthorizesPendingAuthRecovery(t *testing.T) {
	s, events, failures := testLifecycle(t)
	denied := &cursor.CursorKeychainAccessError{Status: -25308}
	failures["restore-auth"] = denied
	if err := RecoverInterruptedSession(s); !errors.Is(err, denied) || !s.recoveryPending {
		t.Fatalf("automatic recovery did not retain the denied state: %v", err)
	}
	if _, err := s.StopProxy(); !errors.Is(err, denied) {
		t.Fatalf("noninteractive Stop did not retain the denied state: %v", err)
	}
	if strings.Contains(strings.Join(*events, ","), "auth-access") {
		t.Fatal("automatic recovery or Stop requested Keychain permission")
	}
	s.lifecycle.authAccess = func() error {
		*events = append(*events, "auth-access")
		delete(failures, "restore-auth")
		return nil
	}
	*events = nil
	state, err := s.StartProxy()
	want := []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth", "auth-access", "restore-auth", "create", "listen", "auth", "settings", "proxy", "model"}
	if err != nil || !state.Running || s.recoveryPending || !reflect.DeepEqual(*events, want) {
		t.Fatalf("explicit Start did not authorize recovery before routing: state=%+v err=%v events=%v", state, err, *events)
	}
}

func TestPendingAuthRecoveryPermissionDenialRetainsBackupAndRouting(t *testing.T) {
	s, events, failures := testLifecycle(t)
	denied := &cursor.CursorKeychainAccessError{Status: -25293}
	failures["restore-auth"] = denied
	failures["auth-access"] = denied
	backup := []byte("fixture legacy recovery data")
	if err := os.WriteFile(s.authBackupPath(), backup, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedSession(s); !errors.Is(err, denied) {
		t.Fatal("expected noninteractive recovery denial")
	}
	*events = nil
	state, err := s.StartProxy()
	want := []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth", "auth-access"}
	if !errors.Is(err, denied) || state.Running || !s.recoveryPending || s.proxy != nil || !reflect.DeepEqual(*events, want) {
		t.Fatalf("permission denial changed routing or lost pending recovery: state=%+v err=%v events=%v", state, err, *events)
	}
	if got, err := os.ReadFile(s.authBackupPath()); err != nil || string(got) != string(backup) {
		t.Fatal("denied recovery removed or changed its backup")
	}
	*events = nil
	if _, err := s.StopProxy(); !errors.Is(err, denied) || strings.Contains(strings.Join(*events, ","), "auth-access") {
		t.Fatalf("Stop repeated the permission request: err=%v events=%v", err, *events)
	}
}

func TestExplicitStartSharesPermissionBudgetWithAuthPreflight(t *testing.T) {
	s, events, failures := testLifecycle(t)
	denied := &cursor.CursorKeychainAccessError{Status: -25308}
	failures["restore-auth"] = denied
	failures["auth"] = denied
	if err := RecoverInterruptedSession(s); !errors.Is(err, denied) {
		t.Fatal("expected noninteractive recovery denial")
	}
	s.lifecycle.authAccess = func() error {
		*events = append(*events, "auth-access")
		delete(failures, "restore-auth")
		return nil
	}
	*events = nil
	state, err := s.StartProxy()
	want := []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth", "auth-access", "restore-auth", "create", "listen", "auth", "restore-auth", "close"}
	if !errors.Is(err, denied) || !strings.Contains(err.Error(), "Always Allow") || state.Running || s.proxy != nil || !reflect.DeepEqual(*events, want) {
		t.Fatalf("preflight repeated the prompt or enabled routing after denial: state=%+v err=%v events=%v", state, err, *events)
	}
}

func TestExplicitStartCanAuthorizeAuthPreflight(t *testing.T) {
	s, events, failures := testLifecycle(t)
	failures["auth"] = &cursor.CursorKeychainAccessError{Status: -25308}
	s.lifecycle.authAccess = func() error {
		*events = append(*events, "auth-access")
		delete(failures, "auth")
		return nil
	}
	state, err := s.StartProxy()
	want := []string{"create", "listen", "auth", "auth-access", "auth", "settings", "proxy", "model"}
	if err != nil || !state.Running || !reflect.DeepEqual(*events, want) {
		t.Fatalf("preflight permission did not precede routing: state=%+v err=%v events=%v", state, err, *events)
	}
}

func TestStopRestorationFailureCanBeRetried(t *testing.T) {
	for _, step := range []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth"} {
		t.Run(step, func(t *testing.T) {
			s, events, failures := testLifecycle(t)
			if _, err := s.StartProxy(); err != nil {
				t.Fatal(err)
			}
			*events = nil
			failures[step] = errors.New("temporarily denied")
			state, err := s.StopProxy()
			want := []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth"}
			routingPending := step == "restore-proxy" || step == "restore-settings"
			if !routingPending {
				want = append(want, "close")
			}
			if err == nil || state.Running != routingPending || !s.recoveryPending || !reflect.DeepEqual(*events, want) {
				t.Fatalf("state=%+v err=%v events=%v", state, err, *events)
			}
			delete(failures, step)
			*events = nil
			state, err = s.StopProxy()
			want = []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth"}
			if routingPending {
				want = append(want, "close")
			}
			if err != nil || state.Running || s.recoveryPending || state.LastError != "" || !reflect.DeepEqual(*events, want) {
				t.Fatalf("state=%+v err=%v events=%v", state, err, *events)
			}
		})
	}
}

func TestStartAfterFailedStopRecoversThenReappliesRouting(t *testing.T) {
	for _, failure := range []string{"restore-model", "restore-proxy"} {
		t.Run(failure, func(t *testing.T) {
			s, events, failures := testLifecycle(t)
			if _, err := s.StartProxy(); err != nil {
				t.Fatal(err)
			}
			failures[failure] = errors.New("temporarily denied")
			if _, err := s.StopProxy(); err == nil {
				t.Fatal("expected incomplete Stop")
			}
			*events = nil
			if _, err := s.StartProxy(); err == nil {
				t.Fatal("Start reported success despite pending recovery")
			}
			want := []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth"}
			if !reflect.DeepEqual(*events, want) {
				t.Fatalf("Start applied settings before recovery: %v", *events)
			}
			delete(failures, failure)
			*events = nil
			state, err := s.StartProxy()
			if failure == "restore-proxy" {
				want = append(want, "close")
			}
			want = append(want, "create", "listen", "auth", "settings", "proxy", "model")
			if err != nil || !state.Running || s.recoveryPending || !reflect.DeepEqual(*events, want) {
				t.Fatalf("did not reapply routing after recovery: state=%+v err=%v events=%v", state, err, *events)
			}
		})
	}
}

func TestFailedStartWithModelRecoveryPendingClosesUnusedListener(t *testing.T) {
	s, events, failures := testLifecycle(t)
	failures["model"] = errors.New("model pin failed")
	failures["restore-model"] = errors.New("model backup removal denied")
	state, err := s.StartProxy()
	if err == nil || state.Running || !s.recoveryPending || s.proxy != nil || (*events)[len(*events)-1] != "close" {
		t.Fatalf("model-only rollback left an active service: state=%+v err=%v events=%v", state, err, *events)
	}
	delete(failures, "restore-model")
	state, err = s.StopProxy()
	if err != nil || state.Running || s.recoveryPending {
		t.Fatalf("stopped recovery was not retryable: state=%+v err=%v", state, err)
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
	want := []string{"create", "listen", "auth", "settings", "proxy", "model", "restore-proxy", "restore-settings", "restore-model", "restore-auth", "close"}
	if !reflect.DeepEqual(*events, want) || !reflect.DeepEqual(notified, []bool{true, false}) {
		t.Fatalf("events=%v notified=%v", *events, notified)
	}
}

func TestInterruptedSessionRecoveryAttemptsAllRestorations(t *testing.T) {
	s, events, failures := testLifecycle(t)
	failures["restore-proxy"] = errors.New("locked registry")
	err := RecoverInterruptedSession(s)
	want := []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth"}
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
	want := []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth"}
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
	want := []string{"restore-proxy", "restore-settings", "restore-model", "restore-auth"}
	if err == nil || state.Running || !reflect.DeepEqual(*events, want) {
		t.Fatalf("state=%+v err=%v events=%v", state, err, *events)
	}
	delete(failures, "restore-proxy")
	*events = nil
	state, err = s.StartProxy()
	want = append(want, "create", "listen", "auth", "settings", "proxy", "model")
	if err != nil || !state.Running || s.recoveryPending || !reflect.DeepEqual(*events, want) {
		t.Fatalf("state=%+v err=%v pending=%v events=%v", state, err, s.recoveryPending, *events)
	}
}
