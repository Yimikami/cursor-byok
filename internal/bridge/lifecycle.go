package bridge

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"cursor-byok/internal/agent"
	"cursor-byok/internal/certs"
	"cursor-byok/internal/cursor"
	"cursor-byok/internal/mitm"
)

type proxyServer interface {
	Start() error
	Stop(context.Context) error
}

// Keep OS changes behind a small seam so failure and rollback paths can be
// exercised without changing the test machine's trust store or proxy settings.
type proxyLifecycle struct {
	caInstalled  func() bool
	newServer    func() (proxyServer, error)
	applyTweaks  func(string) error
	revertTweaks func() error
	prepareAuth  func(string) error
	restoreAuth  func(string) error
	authAccess   func() error
	restoreModel func(string) error
	selectModel  func(string) error
	enableProxy  func(string) error
	disableProxy func() error
}

func (s *ProxyService) lifecycleOps() *proxyLifecycle {
	if s.lifecycle != nil {
		return s.lifecycle
	}
	return &proxyLifecycle{
		caInstalled: func() bool {
			return certs.IsInstalledUserRoot(filepath.Join(s.ca.Dir(), "ca.crt"))
		},
		newServer: func() (proxyServer, error) {
			resolver := func() []agent.AdapterTarget {
				ads := s.gatewayAdapters()
				out := make([]agent.AdapterTarget, 0, len(ads))
				for _, a := range ads {
					out = append(out, agent.AdapterTargetFromRelay(a))
				}
				return out
			}
			selectedModel := func(purpose string) string { return selectedModelFor(s.cfgDir, purpose) }
			return mitm.New(defaultListenAddr, s.ca, s.gateway, resolver, selectedModel)
		},
		applyTweaks:  cursor.ApplyCursorTweaks,
		revertTweaks: cursor.RevertCursorTweaks,
		prepareAuth:  cursor.PrepareCursorAuthWithoutInteraction,
		restoreAuth:  cursor.RestoreFakeProUser,
		authAccess:   cursor.RequestCursorKeychainAccess,
		selectModel: func(modelID string) error {
			var knownIDs []string
			for _, adapter := range s.gatewayAdapters() {
				knownIDs = append(knownIDs, adapter.StableID())
			}
			return cursor.ForceModelSelectionForAdapters(modelID, s.modelBackupPath(), knownIDs)
		},
		restoreModel: cursor.RestoreModelSelection,
		enableProxy:  cursor.EnableSystemProxy,
		disableProxy: cursor.DisableSystemProxy,
	}
}

// State callbacks may call GetState. Invoke them after releasing the lock.
func (s *ProxyService) unlockLifecycle(previouslyRunning bool, previousError string) {
	cb, running, lastError := s.onStateChange, s.state.Running, s.state.LastError
	s.mu.Unlock()
	if cb != nil && (running != previouslyRunning || lastError != previousError) {
		cb(running)
	}
}

// RecoverInterruptedSession restores persisted backups after a crash. Call
// only after acquiring the application's single-instance lock: a second app
// launch must never revert the first instance's active proxy settings.
func RecoverInterruptedSession(s *ProxyService) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Running {
		return nil
	}
	return s.recoverInterruptedSessionLocked()
}

func (s *ProxyService) recoverInterruptedSessionLocked() error {
	return s.recoverInterruptedSessionWithAuthLocked(s.lifecycleOps().restoreAuth)
}

func (s *ProxyService) recoverInterruptedSessionWithAuthLocked(restoreAuth func(string) error) error {
	ops := s.lifecycleOps()
	var cleanup []error
	var routingPending bool
	if err := ops.disableProxy(); err != nil {
		routingPending = true
		cleanup = append(cleanup, fmt.Errorf("restore system proxy: %w", err))
	}
	if err := ops.revertTweaks(); err != nil {
		routingPending = true
		cleanup = append(cleanup, fmt.Errorf("restore Cursor settings: %w", err))
	}
	if err := ops.restoreModel(s.modelBackupPath()); err != nil {
		cleanup = append(cleanup, fmt.Errorf("restore Cursor models: %w", err))
	}
	if err := restoreAuth(s.authBackupPath()); err != nil {
		cleanup = append(cleanup, fmt.Errorf("restore Cursor auth: %w", err))
	}
	return s.finishRecoveryLocked(routingPending, cleanup)
}

// A model/auth sidecar can need another cleanup attempt after routing has
// already been restored. Keep that pending work separate from an active
// listener, which is required only while a client may still route through it.
func (s *ProxyService) finishRecoveryLocked(routingPending bool, cleanup []error) error {
	if !routingPending && s.proxy != nil {
		if err := s.proxy.Stop(context.Background()); err != nil {
			cleanup = append(cleanup, fmt.Errorf("stop listener: %w", err))
		} else {
			s.proxy = nil
		}
	}
	s.state.Running = routingPending && s.proxy != nil
	if !s.state.Running {
		s.state.StartedAt = 0
	}
	err := errors.Join(cleanup...)
	s.recoveryPending = err != nil
	s.state.LastError = ""
	if err != nil {
		s.state.LastError = "Recovery incomplete. Retry Stop: " + err.Error()
		if s.state.Running {
			s.state.LastError = "Routing recovery incomplete; listener kept running. Retry Stop: " + err.Error()
		}
	}
	return err
}

func (s *ProxyService) StartProxy() (ProxyState, error) {
	s.mu.Lock()
	defer s.unlockLifecycle(s.state.Running, s.state.LastError)
	fail := func(err error) (ProxyState, error) {
		s.state.LastError = err.Error()
		return s.state, err
	}
	ops := s.lifecycleOps()
	var permission cursor.CursorKeychainPermissionAttempt
	runAuth := func(action func() error) error {
		return permission.Run(action, ops.authAccess)
	}
	if s.recoveryPending {
		// Automatic startup recovery may have retained an encrypted legacy
		// session because Keychain access was denied. This explicit Start can
		// request access to finish that recovery before applying new routing.
		if err := s.recoverInterruptedSessionWithAuthLocked(func(path string) error {
			return runAuth(func() error { return ops.restoreAuth(path) })
		}); err != nil {
			return s.state, err
		}
	}
	if s.state.Running {
		return s.state, nil
	}
	s.state.CAInstalled = ops.caInstalled()
	// Linux also supports a manual system-store import that the NSS probe
	// cannot inspect. Windows and macOS have a single supported trust path.
	if !s.state.CAInstalled && runtime.GOOS != "linux" {
		return fail(errors.New("the current local CA is not trusted; click Install CA before starting the service"))
	}
	if _, err := readConfig(s.cfgDir); err != nil {
		return fail(err)
	}
	adapters := s.gatewayAdapters()
	if len(adapters) == 0 {
		return fail(errors.New("add a model adapter before starting the service"))
	}
	s.state.ListenAddr = defaultListenAddr
	srv, err := ops.newServer()
	if err != nil {
		return fail(err)
	}
	if err := srv.Start(); err != nil {
		return fail(err)
	}

	var tweaksAttempted, authAttempted, modelAttempted, proxyAttempted bool
	rollback := func(cause error) (ProxyState, error) {
		var cleanup []error
		var routingPending bool
		if proxyAttempted {
			if err := ops.disableProxy(); err != nil {
				routingPending = true
				cleanup = append(cleanup, fmt.Errorf("restore system proxy: %w", err))
			}
		}
		if tweaksAttempted {
			if err := ops.revertTweaks(); err != nil {
				routingPending = true
				cleanup = append(cleanup, fmt.Errorf("restore Cursor settings: %w", err))
			}
		}
		if modelAttempted {
			if err := ops.restoreModel(s.modelBackupPath()); err != nil {
				cleanup = append(cleanup, fmt.Errorf("restore Cursor models: %w", err))
			}
		}
		if authAttempted {
			if err := ops.restoreAuth(s.authBackupPath()); err != nil {
				cleanup = append(cleanup, fmt.Errorf("restore Cursor auth: %w", err))
			}
		}
		s.proxy = srv
		if err := s.finishRecoveryLocked(routingPending, cleanup); err != nil {
			if s.state.Running {
				s.state.StartedAt = time.Now().Unix()
			}
			return fail(fmt.Errorf("%w; %s", cause, s.state.LastError))
		}
		return fail(cause)
	}

	// Complete Cursor's prerequisites before routing other applications
	// through this process. Failed operations may have partially written,
	// so their restoration is attempted too.
	// Verify encrypted account access before modifying either Cursor's proxy
	// preferences or the system proxy. Only this explicit Start may request
	// Keychain permission; automatic recovery and request guards never do.
	authAttempted = true
	if err := runAuth(func() error { return ops.prepareAuth(s.authBackupPath()) }); err != nil {
		return rollback(fmt.Errorf("configure Cursor auth: %w", err))
	}
	tweaksAttempted = true
	if err := ops.applyTweaks(s.state.ListenAddr); err != nil {
		return rollback(fmt.Errorf("configure Cursor settings: %w", err))
	}
	proxyAttempted = true
	if err := ops.enableProxy(s.state.ListenAddr); err != nil {
		return rollback(fmt.Errorf("enable system proxy: %w", err))
	}
	// Keep model changes last, with their own recoverable snapshot.
	modelAttempted = true
	if err := ops.selectModel(adapters[0].StableID()); err != nil {
		return rollback(fmt.Errorf("configure Cursor model: %w", err))
	}
	s.proxy = srv
	s.state.Running = true
	s.state.StartedAt = time.Now().Unix()
	s.state.LastError = ""
	s.recoveryPending = false
	return s.state, nil
}

func (s *ProxyService) StopProxy() (ProxyState, error) {
	s.mu.Lock()
	defer s.unlockLifecycle(s.state.Running, s.state.LastError)
	if !s.state.Running && !s.recoveryPending {
		return s.state, nil
	}
	err := s.recoverInterruptedSessionLocked()
	return s.state, err
}
