package main

import (
	"embed"
	"log"
	"os"
	"runtime"
	"sync"

	"cursor-byok/internal/bridge"

	// Side-effect imports: each gen package's init() registers its message
	// types in the global proto registry so that protocodec can look up
	// response messages by Connect URL path.
	_ "cursor-byok/internal/protocodec/gen/agent/v1"
	_ "cursor-byok/internal/protocodec/gen/aiserver/v1"
	_ "cursor-byok/internal/protocodec/gen/anyrun/v1"
	_ "cursor-byok/internal/protocodec/gen/internapi/v1"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var trayIcon []byte

func init() {
	application.RegisterEvent[bool]("proxyState")
	// Fired by main.go's WindowClosing hook when the user clicks the close
	// button AND hasn't pinned a preference yet. The frontend listens for
	// it and renders the "Quit or minimize to tray?" modal.
	application.RegisterEvent[bool]("closeRequested")
}

func main() {
	// Fix WebKitGTK buffer creation issues on Linux
	if runtime.GOOS == "linux" {
		os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
	}

	var window *application.WebviewWindow
	var requestQuit func()

	app := application.New(application.Options{
		Name:        "cursor-byok",
		Description: "cursor-byok - Local MITM proxy & BYOK gateway for Cursor IDE",
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		ShouldQuit: func() bool {
			if requestQuit == nil {
				return true
			}
			// Cocoa invokes this on its UI thread. Veto immediately and
			// perform restoration through the same worker as tray Quit.
			go requestQuit()
			return false
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "cursor-byok",
			OnSecondInstanceLaunch: func(_ application.SecondInstanceData) {
				if window != nil {
					window.Show()
					window.Focus()
				}
			},
		},
	})
	// Acquire the single-instance lock before recovery. NewProxyService
	// restores stale settings even if loading the local CA subsequently fails.
	proxySvc, err := bridge.NewProxyService()
	if err != nil {
		log.Fatalf("failed to init proxy service: %v", err)
	}
	app.RegisterService(application.NewService(proxySvc))

	window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:         "cursor-byok",
		Width:         1000,
		Height:        540,
		DisableResize: true,
		Frameless:     true,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(9, 9, 11),
		URL:              "/",
	})

	var quitMu sync.Mutex
	forceQuit := func() {
		if !quitMu.TryLock() {
			return
		}
		defer quitMu.Unlock()
		_, err := proxySvc.StopProxy()
		if err != nil {
			window.Show()
			app.Dialog.Warning().
				SetTitle("cursor-byok").
				SetMessage("Could not finish restoring your saved settings. Retry Stop or Quit:\n" + err.Error()).
				Show()
			return
		}
		window.Hide()
		os.Exit(0)
	}
	requestQuit = forceQuit

	// Clicking the window close button routes through the user's saved
	// preference. First-run = unset → cancel the close and ask the frontend
	// to show the "Quit or minimize to tray?" modal. After the user picks
	// once (with "Don't ask again" checked), GetCloseAction returns the
	// pinned value and we short-circuit straight to the chosen action.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		switch proxySvc.GetCloseAction() {
		case "tray":
			window.Hide()
			e.Cancel()
		case "quit":
			// Keep the app available when restoration fails so the user can
			// retry without leaving a dead system proxy behind.
			e.Cancel()
			go forceQuit()
		default:
			// No pinned preference yet. Keep the window visible while the
			// frontend modal is open so the user can see what they're
			// choosing; the modal's buttons then call RequestQuit or
			// RequestHide via the Wails bindings.
			e.Cancel()
			app.Event.Emit("closeRequested", true)
		}
	})

	// Frontend modal callbacks. Both run in a Wails goroutine (see
	// RequestQuit / RequestHide), so window operations below are safe.
	proxySvc.SetHideCallback(func() {
		window.Hide()
	})
	proxySvc.SetQuitCallback(forceQuit)

	// ---------------- System tray ----------------
	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("cursor-byok - stopped")

	menu := app.NewMenu()

	menu.Add("Show cursor-byok").OnClick(func(_ *application.Context) {
		window.Show()
		window.Focus()
	})
	menu.AddSeparator()

	startItem := menu.Add("Start service")
	stopItem := menu.Add("Stop service")

	startItem.OnClick(func(_ *application.Context) {
		go func() {
			if _, err := proxySvc.StartProxy(); err != nil {
				app.Dialog.Warning().
					SetTitle("cursor-byok").
					SetMessage("Failed to start:\n" + err.Error()).
					Show()
			}
		}()
	})
	stopItem.OnClick(func(_ *application.Context) {
		go func() {
			if _, err := proxySvc.StopProxy(); err != nil {
				app.Dialog.Warning().
					SetTitle("cursor-byok").
					SetMessage("Failed to stop:\n" + err.Error()).
					Show()
			}
		}()
	})

	menu.AddSeparator()
	menu.Add("Open settings folder").OnClick(func(_ *application.Context) {
		_ = proxySvc.OpenSettingsFolder()
	})
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(_ *application.Context) {
		go forceQuit()
	})

	tray.SetMenu(menu)

	// Left-click the tray icon brings the window to front.
	tray.OnClick(func() {
		window.Show()
		window.Focus()
	})

	applyTrayState := func(running bool) {
		if running {
			tray.SetTooltip("cursor-byok - running on 127.0.0.1:18080")
		} else {
			tray.SetTooltip("cursor-byok - stopped")
		}
		startItem.SetEnabled(!running)
		stopItem.SetEnabled(running || proxySvc.GetState().LastError != "")
	}
	applyTrayState(false)

	proxySvc.SetStateCallback(func(running bool) {
		applyTrayState(running)
		app.Event.Emit("proxyState", running)
	})

	if err := app.Run(); err != nil {
		proxySvc.Shutdown()
		log.Fatal(err)
	}
	proxySvc.Shutdown()
}
