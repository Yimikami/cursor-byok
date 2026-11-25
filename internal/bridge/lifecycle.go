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
	injectAuth   func(string) error
	restoreAuth  func(string) error
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
		injectAuth:   cursor.InjectFakeProUser,
		restoreAuth:  cursor.RestoreFakeProUser,
		selectModel:  cursor.ForceModelSelection,
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
	ops := s.lifecycleOps()
	var cleanup []error
	if err := ops.disableProxy(); err != nil {
		cleanup = append(cleanup, fmt.Errorf("restore system proxy: %w", err))
	}
	if err := ops.revertTweaks(); err != nil {
		cleanup = append(cleanup, fmt.Errorf("restore Cursor settings: %w", err))
	}
	if err := ops.restoreAuth(s.authBackupPath()); err != nil {
		cleanup = append(cleanup, fmt.Errorf("restore Cursor auth: %w", err))
	}
	err := errors.Join(cleanup...)
	s.recoveryPending = err != nil
	if err != nil {
		s.state.LastError = "Could not recover the previous session. Retry Stop: " + err.Error()
	} else {
		s.state.LastError = ""
	}
	return err
}

func (s *ProxyService) StartProxy() (ProxyState, error) {
	s.mu.Lock()
	defer s.unlockLifecycle(s.state.Running, s.state.LastError)
	if s.state.Running {
		return s.state, nil
	}
	fail := func(err error) (ProxyState, error) {
		s.state.LastError = err.Error()
		return s.state, err
	}
	if s.recoveryPending {
		if err := s.recoverInterruptedSessionLocked(); err != nil {
			return s.state, err
		}
	}
	ops := s.lifecycleOps()
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

	var tweaksAttempted, authAttempted, proxyAttempted bool
	rollback := func(cause error) (ProxyState, error) {
		var cleanup []error
		if proxyAttempted {
			if err := ops.disableProxy(); err != nil {
				cleanup = append(cleanup, fmt.Errorf("restore system proxy: %w", err))
			}
		}
		if tweaksAttempted {
			if err := ops.revertTweaks(); err != nil {
				cleanup = append(cleanup, fmt.Errorf("restore Cursor settings: %w", err))
			}
		}
		if authAttempted {
			if err := ops.restoreAuth(s.authBackupPath()); err != nil {
				cleanup = append(cleanup, fmt.Errorf("restore Cursor auth: %w", err))
			}
		}
		if len(cleanup) > 0 {
			// A client may still point at this port. Keep it reachable, retain
			// the backups, and let Stop retry instead of stranding its traffic.
			s.proxy = srv
			s.state.Running = true
			s.state.StartedAt = time.Now().Unix()
			return fail(fmt.Errorf("%w; recovery incomplete, service kept running; retry Stop: %w", cause, errors.Join(cleanup...)))
		}
		if err := srv.Stop(context.Background()); err != nil {
			cause = errors.Join(cause, fmt.Errorf("stop listener: %w", err))
		}
		return fail(cause)
	}

	// Complete Cursor's prerequisites before routing other applications
	// through this process. Failed operations may have partially written,
	// so their restoration is attempted too.
	tweaksAttempted = true
	if err := ops.applyTweaks(s.state.ListenAddr); err != nil {
		return rollback(fmt.Errorf("configure Cursor settings: %w", err))
	}
	authAttempted = true
	if err := ops.injectAuth(s.authBackupPath()); err != nil {
		return rollback(fmt.Errorf("configure Cursor auth: %w", err))
	}
	if err := ops.selectModel(adapters[0].StableID()); err != nil {
		return rollback(fmt.Errorf("configure Cursor model: %w", err))
	}
	proxyAttempted = true
	if err := ops.enableProxy(s.state.ListenAddr); err != nil {
		return rollback(fmt.Errorf("enable system proxy: %w", err))
	}
	s.proxy = srv
	s.state.Running = true
	s.state.StartedAt = time.Now().Unix()
	s.state.LastError = ""
	return s.state, nil
}

func (s *ProxyService) StopProxy() (ProxyState, error) {
	s.mu.Lock()
	defer s.unlockLifecycle(s.state.Running, s.state.LastError)
	if !s.state.Running {
		if s.recoveryPending {
			err := s.recoverInterruptedSessionLocked()
			return s.state, err
		}
		return s.state, nil
	}
	ops := s.lifecycleOps()
	var cleanup []error
	if err := ops.disableProxy(); err != nil {
		cleanup = append(cleanup, fmt.Errorf("restore system proxy: %w", err))
	}
	if err := ops.revertTweaks(); err != nil {
		cleanup = append(cleanup, fmt.Errorf("restore Cursor settings: %w", err))
	}
	if err := ops.restoreAuth(s.authBackupPath()); err != nil {
		cleanup = append(cleanup, fmt.Errorf("restore Cursor auth: %w", err))
	}
	if err := errors.Join(cleanup...); err != nil {
		s.state.LastError = "Recovery incomplete; service kept running. Retry Stop: " + err.Error()
		return s.state, errors.New(s.state.LastError)
	}
	var stopErr error
	if s.proxy != nil {
		stopErr = s.proxy.Stop(context.Background())
		s.proxy = nil
	}
	s.state.Running = false
	s.state.StartedAt = 0
	s.state.LastError = ""
	if stopErr != nil {
		s.state.LastError = "Settings restored, but listener shutdown failed: " + stopErr.Error()
	}
	return s.state, stopErr
}
