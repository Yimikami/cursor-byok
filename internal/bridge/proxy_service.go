package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"cursor-byok/internal/agent"
	"cursor-byok/internal/certs"
	"cursor-byok/internal/cursor"
	"cursor-byok/internal/relay"
)

const (
	defaultListenAddr = "127.0.0.1:18080"
	defaultUpstream   = "https://api2.cursor.sh"
)

type ProxyService struct {
	mu              sync.RWMutex
	state           ProxyState
	proxy           proxyServer
	lifecycle       *proxyLifecycle
	recoveryPending bool
	ca              *certs.CA
	gateway         *relay.Gateway
	cfgDir          string
	onStateChange   func(running bool)
	gatewayAdapters func() []relay.AdapterInfo
	// quitCb / hideCb are wired up by main.go so the frontend's close
	// dialog can drive the window without needing direct access to the
	// Wails application handle. Both run in a goroutine so the Wails RPC
	// that triggered them can return cleanly before we tear the app down.
	quitCb func()
	hideCb func()
}

// adapterListFor reads the user config and returns the BYOK adapter list
// suitable for relay rewrites and SQLite pinning.
func adapterListFor(cfgDir string) []relay.AdapterInfo {
	c, err := readConfig(cfgDir)
	if err != nil {
		return nil
	}
	return adapterListFromConfig(c)
}

func adapterListFromConfig(c UserConfig) []relay.AdapterInfo {
	out := make([]relay.AdapterInfo, 0, len(c.ModelAdapters))
	for _, a := range c.ModelAdapters {
		if a.ModelID == "" {
			continue
		}
		out = append(out, relay.AdapterInfo{
			DisplayName:             a.DisplayName,
			Type:                    a.Type,
			ModelID:                 a.ModelID,
			BaseURL:                 a.BaseURL,
			APIKey:                  a.APIKey,
			SupportedThinkingLevels: a.SupportedThinkingLevels,
			ServiceTier:             a.ServiceTier,
			MaxOutputTokens:         parseIntSafe(a.MaxOutputTokens),
			ThinkingBudget:          parseIntSafe(a.ThinkingBudget),
		})
	}
	return prioritizeActiveAdapter(out, strings.TrimSpace(c.ActiveModelID))
}

func prioritizeActiveAdapter(adapters []relay.AdapterInfo, activeModelID string) []relay.AdapterInfo {
	if len(adapters) < 2 || activeModelID == "" {
		return adapters
	}
	idx := -1
	for i, a := range adapters {
		if strings.EqualFold(strings.TrimSpace(a.ModelID), activeModelID) {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return adapters
	}
	out := make([]relay.AdapterInfo, 0, len(adapters))
	out = append(out, adapters[idx])
	out = append(out, adapters[:idx]...)
	out = append(out, adapters[idx+1:]...)
	return out
}

func selectedModelFor(cfgDir string, purpose string) string {
	c, err := readConfig(cfgDir)
	if err != nil {
		return ""
	}
	switch purpose {
	case "commit":
		return strings.TrimSpace(c.CommitModelID)
	case "review":
		return strings.TrimSpace(c.ReviewModelID)
	default:
		return ""
	}
}

func (s *ProxyService) SetStateCallback(cb func(running bool)) {
	s.mu.Lock()
	s.onStateChange = cb
	s.mu.Unlock()
}

// SetQuitCallback registers the function main.go uses to fully tear the
// app down (Stop proxy, remove tray icon, quit the Wails application).
// Invoked from RequestQuit so the frontend close dialog can exit cleanly.
func (s *ProxyService) SetQuitCallback(cb func()) {
	s.mu.Lock()
	s.quitCb = cb
	s.mu.Unlock()
}

// SetHideCallback registers the function main.go uses to hide the main
// window to the system tray (keeping the proxy and tray icon alive).
func (s *ProxyService) SetHideCallback(cb func()) {
	s.mu.Lock()
	s.hideCb = cb
	s.mu.Unlock()
}

// GetCloseAction returns the persisted close-behaviour preference
// ("" = ask, "quit", "tray"). Unbound errors from config.json are swallowed
// — a missing or malformed config just maps to "ask the user".
func (s *ProxyService) GetCloseAction() string {
	cfg, err := readConfig(s.cfgDir)
	if err != nil {
		return ""
	}
	switch cfg.CloseAction {
	case "quit", "tray":
		return cfg.CloseAction
	default:
		return ""
	}
}

// SetCloseAction persists the user's choice from the close dialog. "quit"
// and "tray" are the only meaningful values; anything else is normalised
// to "" (which makes the dialog reappear on the next close — a safe reset).
func (s *ProxyService) SetCloseAction(action string) error {
	cfg, err := readConfig(s.cfgDir)
	if err != nil {
		return err
	}
	switch action {
	case "quit", "tray":
		cfg.CloseAction = action
	default:
		cfg.CloseAction = ""
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.cfgDir, "config.json"), b, 0o600)
}

// RequestQuit triggers the registered quit callback. Called from the
// frontend close dialog's "Quit" button. Runs the callback on its own
// goroutine so the Wails RPC returns before the app actually tears down.
func (s *ProxyService) RequestQuit() {
	s.mu.RLock()
	cb := s.quitCb
	s.mu.RUnlock()
	if cb != nil {
		go cb()
	}
}

// RequestHide triggers the registered hide-to-tray callback. Called from
// the frontend close dialog's "Minimize to tray" button.
func (s *ProxyService) RequestHide() {
	s.mu.RLock()
	cb := s.hideCb
	s.mu.RUnlock()
	if cb != nil {
		go cb()
	}
}

func detectCAInstallMode() string {
	switch runtime.GOOS {
	case "windows":
		return "auto-user-store"
	case "darwin":
		return "auto-login-keychain"
	case "linux":
		if certs.IsInstalledUserRoot() {
			return "auto-nss"
		}
		return "manual-or-nss"
	default:
		return "manual"
	}
}

func buildCAWarning() string {
	switch runtime.GOOS {
	case "linux":
		if certs.IsInstalledUserRoot() {
			return "Linux trust is currently wired through the user's NSS DB (~/.pki/nssdb). Other apps may still need a manual system-store import of the CA PEM from the settings folder."
		}
		return "Linux has no single cross-desktop trust store. cursor-byok can import into the user's NSS DB when certutil is installed, but other apps may still require a manual system-store import of the CA PEM from the settings folder."
	case "darwin":
		return "macOS trust is installed into the current user's login keychain. If Keychain prompts for permission, approve it for Cursor to trust the local MITM CA."
	default:
		return ""
	}
}

// NewProxyService must be called after acquiring the application's
// single-instance lock because it recovers persisted settings from a crash.
func NewProxyService() (*ProxyService, error) {
	dir, err := cfgDir()
	if err != nil {
		return nil, err
	}
	return newProxyService(dir, RecoverInterruptedSession)
}

func newProxyService(dir string, recoverSession func(*ProxyService) error) (*ProxyService, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Do not let a damaged CA pair strand the system proxy from a prior run.
	recovery := &ProxyService{cfgDir: dir}
	recoveryErr := recoverSession(recovery)
	ca, err := certs.LoadOrCreate(filepath.Join(dir, "ca"))
	if err != nil {
		return nil, errors.Join(err, recoveryErr)
	}
	gw := relay.NewGateway()
	// Persist chat turns under %APPDATA%/cursor-byok/history so conversations
	// survive restart. The agent package walks this on first use and loads
	// every <conv_id>.jsonl back into its in-memory session history.
	agent.InitHistoryDir(filepath.Join(dir, "history"))
	cfg, _ := readConfig(dir)
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultUpstream
	}
	svc := &ProxyService{
		cfgDir:          dir,
		ca:              ca,
		gateway:         gw,
		recoveryPending: recovery.recoveryPending,
		state: ProxyState{
			ListenAddr:    defaultListenAddr,
			BaseURL:       baseURL,
			CAFingerprint: ca.Fingerprint(),
			CAPath:        filepath.Join(ca.Dir(), "ca.crt"),
			CAInstalled:   certs.IsInstalledUserRoot(filepath.Join(ca.Dir(), "ca.crt")),
			CAInstallMode: detectCAInstallMode(),
			CAWarning:     buildCAWarning(),
			LastError:     recovery.state.LastError,
		},
	}
	svc.gatewayAdapters = func() []relay.AdapterInfo { return adapterListFor(svc.cfgDir) }
	gw.SetAdapterProvider(func() []relay.AdapterInfo {
		c, err := readConfig(svc.cfgDir)
		if err != nil {
			return nil
		}
		return adapterListFromConfig(c)
	})
	return svc, nil
}

func (s *ProxyService) GetState() ProxyState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.state
	st.CAInstalled = certs.IsInstalledUserRoot(filepath.Join(s.ca.Dir(), "ca.crt"))
	st.CAInstallMode = detectCAInstallMode()
	st.CAWarning = buildCAWarning()
	return st
}

func (s *ProxyService) InstallCA() (ProxyState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	certPath := filepath.Join(s.ca.Dir(), "ca.crt")
	if err := certs.InstallUserRoot(certPath); err != nil {
		s.state.LastError = err.Error()
		return s.state, err
	}
	s.state.CAInstalled = certs.IsInstalledUserRoot(certPath)
	if !s.state.CAInstalled {
		err := errors.New("CA installation could not be verified; the current local CA is not trusted")
		s.state.LastError = err.Error()
		return s.state, err
	}
	s.state.LastError = ""
	return s.state, nil
}

func (s *ProxyService) UninstallCA() (ProxyState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Running {
		err := errors.New("stop the service before uninstalling its CA")
		s.state.LastError = err.Error()
		return s.state, err
	}
	if err := certs.UninstallUserRoot(filepath.Join(s.ca.Dir(), "ca.crt")); err != nil {
		s.state.LastError = err.Error()
		return s.state, err
	}
	s.state.CAInstalled = false
	s.state.LastError = ""
	return s.state, nil
}

// authBackupPath identifies the legacy auth sidecar for this Cursor profile.
// New sessions preserve the user's genuine account.
func (s *ProxyService) authBackupPath() string {
	return filepath.Join(s.cfgDir, cursor.ProfileBackupName("cursor-auth-backup.json"))
}

func (s *ProxyService) modelBackupPath() string {
	return filepath.Join(s.cfgDir, cursor.ProfileBackupName("cursor-model-backup.json"))
}

func (s *ProxyService) SetBaseURL(url string) (ProxyState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if url == "" {
		return s.state, errors.New("base URL cannot be empty")
	}
	s.state.BaseURL = url
	return s.state, nil
}

func (s *ProxyService) LoadUserConfig() (UserConfig, error) {
	return readConfig(s.cfgDir)
}

func (s *ProxyService) SaveUserConfig(cfg UserConfig) error {
	cfg.ActiveModelID = strings.TrimSpace(cfg.ActiveModelID)
	if cfg.ActiveModelID != "" {
		found := false
		for _, a := range cfg.ModelAdapters {
			if strings.EqualFold(strings.TrimSpace(a.ModelID), cfg.ActiveModelID) {
				found = true
				break
			}
		}
		if !found {
			cfg.ActiveModelID = ""
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.cfgDir, "config.json"), b, 0o600)
}

func (s *ProxyService) ExportCAPEM() string {
	return string(s.ca.CertPEM())
}

func (s *ProxyService) ConfigDir() string {
	return s.cfgDir
}

func (s *ProxyService) OpenSettingsFolder() error {
	path := s.cfgDir
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

func (s *ProxyService) GetCursorSettingsStatus() CursorSettingsStatus {
	st := cursor.GetStatus()
	return CursorSettingsStatus{
		Path:            st.Path,
		Found:           st.Found,
		Error:           st.Error,
		ProxySet:        st.ProxySet,
		ProxyValue:      st.ProxyValue,
		StrictSSLOff:    st.StrictSSLOff,
		ProxySupportOn:  st.ProxySupportOn,
		SystemCertsV2On: st.SystemCertsV2On,
		UseHTTP1:        st.UseHTTP1,
		DisableHTTP2:    st.DisableHTTP2,
		ProxyKerberos:   st.ProxyKerberos,
	}
}

func (s *ProxyService) ApplyCursorTweaks() (CursorSettingsStatus, error) {
	if err := cursor.ApplyCursorTweaks(s.state.ListenAddr); err != nil {
		return s.GetCursorSettingsStatus(), err
	}
	return s.GetCursorSettingsStatus(), nil
}

func (s *ProxyService) RevertCursorTweaks() (CursorSettingsStatus, error) {
	if err := cursor.RevertCursorTweaks(); err != nil {
		return s.GetCursorSettingsStatus(), err
	}
	return s.GetCursorSettingsStatus(), nil
}

func (s *ProxyService) TestAdapter(index int) (ModelAdapterConfig, error) {
	cfg, err := readConfig(s.cfgDir)
	if err != nil {
		return ModelAdapterConfig{}, err
	}
	if index < 0 || index >= len(cfg.ModelAdapters) {
		return ModelAdapterConfig{}, errors.New("adapter index out of range")
	}
	a := cfg.ModelAdapters[index]

	if a.APIKey == "" {
		a.LastTestResult = "missing API key"
	} else if a.BaseURL == "" {
		a.LastTestResult = "missing base URL"
	} else {
		a.LastTestResult = runAdapterPing(a)
	}

	a.LastTestedAt = time.Now().Unix()
	cfg.ModelAdapters[index] = a
	_ = s.SaveUserConfig(cfg)
	return a, nil
}

func runAdapterPing(a ModelAdapterConfig) string {
	if a.ModelID == "" {
		return "missing model id"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := runAdapterInferenceTest(ctx, a)
	if err != nil {
		return err.Error()
	}
	return "ok"
}

func runAdapterInferenceTest(ctx context.Context, a ModelAdapterConfig) (string, error) {
	opts := agent.AdapterOpts{MaxOutputTokens: 512, ServiceTier: a.ServiceTier}
	// The UI's default "None" choice is not evidence that a provider accepts
	// reasoning_effort. Keep the connectivity probe on the basic API contract.
	return agent.ProbeAdapter(ctx, agent.AdapterTarget{
		ProviderType: a.Type, BaseURL: a.BaseURL, APIKey: a.APIKey,
		Model: a.ModelID, Opts: opts,
	})
}

func trimErr(err error) string {
	msg := err.Error()
	if len(msg) > 160 {
		return msg[:160] + "…"
	}
	return msg
}

// GetUsageStats returns the aggregate token-use snapshot the Stats tab in the
// Wails UI renders. Computed on demand from disk; safe to call any time
// (even while the proxy is stopped — history persists across restarts).
func (s *ProxyService) GetUsageStats() UsageStats {
	snap := agent.ComputeUsageStats()
	out := UsageStats{
		TotalPromptTokens:     snap.TotalPromptTokens,
		TotalCompletionTokens: snap.TotalCompletionTokens,
		TotalTokens:           snap.TotalTokens,
		ConversationCount:     snap.ConversationCount,
		TurnCount:             snap.TurnCount,
		PerModel:              make([]ModelUsageEntry, 0, len(snap.PerModel)),
		Last7Days:             make([]DailyUsageEntry, 0, len(snap.Last7Days)),
	}
	for _, m := range snap.PerModel {
		out.PerModel = append(out.PerModel, ModelUsageEntry{
			Model:            m.Model,
			Provider:         m.Provider,
			PromptTokens:     m.PromptTokens,
			CompletionTokens: m.CompletionTokens,
			TurnCount:        m.TurnCount,
		})
	}
	for _, d := range snap.Last7Days {
		out.Last7Days = append(out.Last7Days, DailyUsageEntry{
			Date:             d.Date,
			PromptTokens:     d.PromptTokens,
			CompletionTokens: d.CompletionTokens,
		})
	}
	return out
}

func (s *ProxyService) Shutdown() error {
	_, err := s.StopProxy()
	return err
}

// Wails invokes this lifecycle hook for native application quit/shutdown,
// including paths that do not pass through our tray or close-dialog callback.
func (s *ProxyService) ServiceShutdown() error {
	return s.Shutdown()
}

func readConfig(dir string) (UserConfig, error) {
	cfg := UserConfig{BaseURL: defaultUpstream}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config.json: %w", err)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultUpstream
	}
	return cfg, nil
}

func parseIntSafe(s string) int {
	if s == "" {
		return 0
	}
	var n int
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int(ch-'0')
		}
	}
	return n
}

func cfgDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cursor-byok"), nil
}
