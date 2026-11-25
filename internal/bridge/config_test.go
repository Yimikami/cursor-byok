package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestConfigSavePublishesNewSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not permit replacing this deliberately open file")
	}
	s := &ProxyService{cfgDir: t.TempDir()}
	before := UserConfig{BaseURL: "https://before.example", CloseAction: "tray"}
	if err := s.SaveUserConfig(before); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.cfgDir, "config.json")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := s.SetCloseAction("quit"); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	var old UserConfig
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if old.CloseAction != "tray" {
		t.Fatal("save mutated an already opened configuration snapshot")
	}
	current, err := s.LoadUserConfig()
	if err != nil || current.CloseAction != "quit" || current.BaseURL != before.BaseURL {
		t.Fatalf("new snapshot not published: config=%+v err=%v", current, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("config mode: %v", st.Mode())
	}
}

func TestConfigConcurrentSavesKeepRoutingReadable(t *testing.T) {
	s := &ProxyService{cfgDir: t.TempDir()}
	config := func(n int) UserConfig {
		id := fmt.Sprintf("model-%d", n)
		return UserConfig{ActiveModelID: id, ModelAdapters: []ModelAdapterConfig{{ModelID: id, Notes: strings.Repeat(id, 8000)}}}
	}
	if err := s.SaveUserConfig(config(0)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for worker := 0; worker < 3; worker++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				if err := s.SaveUserConfig(config(n)); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
		go func() {
			defer wg.Done()
			for i := 0; i < 120; i++ {
				cfg, err := readConfig(s.cfgDir)
				if err != nil {
					errs <- err
					return
				}
				if len(cfg.ModelAdapters) != 1 || cfg.ActiveModelID != cfg.ModelAdapters[0].ModelID || cfg.ModelAdapters[0].Notes != strings.Repeat(cfg.ActiveModelID, 8000) {
					errs <- fmt.Errorf("reader observed an incomplete configuration")
					return
				}
				if len(adapterListFor(s.cfgDir)) != 1 {
					errs <- fmt.Errorf("routing lost the configured adapter")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestConfigFailedPublicationRemovesTemporarySnapshot(t *testing.T) {
	s := &ProxyService{cfgDir: t.TempDir()}
	if err := os.Mkdir(filepath.Join(s.cfgDir, "config.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveUserConfig(UserConfig{}); err == nil {
		t.Fatal("expected replacement failure")
	}
	entries, err := os.ReadDir(s.cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatal("failed save left a temporary configuration")
	}
}

func TestAdapterProbeDoesNotRevertConcurrentEdits(t *testing.T) {
	for _, changeAdapter := range []bool{false, true} {
		t.Run(fmt.Sprintf("adapter_changed=%t", changeAdapter), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()
			s := &ProxyService{cfgDir: t.TempDir()}
			cfg := UserConfig{ModelAdapters: []ModelAdapterConfig{{Type: "openai", BaseURL: srv.URL, ModelID: "fixture", APIKey: "fixture-key"}}}
			if err := s.SaveUserConfig(cfg); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := s.TestAdapter(0); done <- err }()
			<-started
			cfg.CloseAction = "tray"
			if changeAdapter {
				cfg.ModelAdapters[0].ModelID = "replacement"
			}
			if err := s.SaveUserConfig(cfg); err != nil {
				close(release)
				t.Fatal(err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			got, err := s.LoadUserConfig()
			if err != nil {
				t.Fatal(err)
			}
			wantResult := "ok"
			if changeAdapter {
				wantResult = ""
			}
			if got.CloseAction != "tray" || got.ModelAdapters[0].ModelID != cfg.ModelAdapters[0].ModelID || got.ModelAdapters[0].LastTestResult != wantResult {
				t.Fatal("completed probe overwrote newer settings or attached a result to a changed adapter")
			}
		})
	}
}
