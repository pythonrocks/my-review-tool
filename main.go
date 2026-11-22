package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed myreview.example.json
var exampleConfig []byte

// trayIcon is the menu-bar template image (black on transparent; macOS tints it).
//
//go:embed trayicon.png
var trayIcon []byte

// migrationNote is the line resolvePaths leaves for the log when it moved the old data dir.
var migrationNote string

// resolvePaths returns the config and data paths: MYREVIEW_CONFIG and MYREVIEW_DATA_DIR when set
// (relative to the working directory), otherwise under <user config dir>/myReview, created 0700. The old
// uReview dir is moved first whenever a default path is used, so myReview is never created empty in front of it.
func resolvePaths() (configPath, dataDir string, err error) {
	configPath, dataDir = os.Getenv("MYREVIEW_CONFIG"), os.Getenv("MYREVIEW_DATA_DIR")
	if configPath == "" || dataDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", "", err
		}
		dir := filepath.Join(base, "myReview")
		if err := migrateOldDir(filepath.Join(base, "uReview"), dir); err != nil {
			return "", "", err
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", "", err
		}
		if configPath == "" {
			configPath = filepath.Join(dir, "myreview.json")
		}
		if dataDir == "" {
			dataDir = filepath.Join(dir, "myreview-data")
		}
	}
	configPath, err = filepath.Abs(configPath) // the dashboard names the file to edit
	return configPath, dataDir, err
}

// migrateOldDir moves the pre-rename uReview dir to dir, with its config, data dir and log, once:
// only when dir does not exist yet. A failed rename is returned, so the app never starts empty over old data.
func migrateOldDir(old, dir string) error {
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return err // dir exists (nil): nothing to do
	}
	if _, err := os.Lstat(old); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	// Files first, the dir last: after a failed rename dir still does not exist, so the next start retries.
	for _, r := range [][2]string{
		{"ureview.json", "myreview.json"},
		{"ureview-data", "myreview-data"},
		{"myreview-data/ureview.log", "myreview-data/myreview.log"},
	} {
		from, to := filepath.Join(old, r[0]), filepath.Join(old, r[1])
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if _, err := os.Lstat(to); err == nil {
			continue
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	if err := os.Rename(old, dir); err != nil {
		return err
	}
	migrationNote = fmt.Sprintf("moved %s to %s", old, dir)
	return nil
}

// redirectLogs points stdout and stderr, panics included, at <dataDir>/myreview.log.
// shortcut: no rotation, the file grows until deleted; rotate if it gets large.
func redirectLogs(dataDir string) error {
	path := filepath.Join(dataDir, "myreview.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(os.Stderr, "myReview logging to", path)
	for _, fd := range []int{1, 2} {
		if err := dupFD(int(f.Fd()), fd); err != nil {
			return err
		}
	}
	return nil
}

// startup loads the config and opens the store, and returns the URL the window opens. Without a config file (Lstat,
// so a dangling symlink is a startup error, not onboarding)
// (first launch) it starts from the example config and writes nothing; the Settings page saves the file.
func startup() (cfg config, configPath, dataDir string, store resultStore, startURL string, err error) {
	configPath, dataDir, err = resolvePaths()
	if err != nil {
		return
	}
	startURL = "/"
	if _, serr := os.Lstat(configPath); errors.Is(serr, fs.ErrNotExist) {
		startURL = "/settings?first=1"
		cfg, err = parseConfig(configPath, exampleConfig, func(string) ([]byte, error) { return nil, fs.ErrNotExist })
	} else {
		cfg, err = loadConfig(configPath)
	}
	if err != nil {
		return
	}
	store, err = openStore(dataDir)
	return
}

func main() {
	var handler http.Handler = http.NotFoundHandler() // the real one once startup succeeds
	var quit func()                                   // cancels the poller and stops the queue, once they exist
	var win *application.WebviewWindow                // nil until the window exists
	// application.New runs the single-instance check: a second launch exits there, before startup touches the
	// config or the data dir, and the first one shows its window.
	app := application.New(application.Options{
		Name:   "myReview",
		Assets: application.AssetOptions{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) })},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:               "io.github.pythonrocks.myreview",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) { showWindow(win) },
		},
		// every quit: app menu, Cmd-Q, menu bar, and SIGINT/SIGTERM through Wails' signal handler
		OnShutdown: func() {
			if quit != nil {
				quit()
			}
		},
	})
	cfg, configPath, dataDir, store, startURL, err := startup()
	if err == nil {
		err = redirectLogs(dataDir)
	}
	if migrationNote != "" { // stderr is the log once redirected; recorded even when startup failed after the move
		fmt.Fprintln(os.Stderr, migrationNote)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		showStartupError(app, err)
	}
	mdb, err := openMetricsDB(dataDir) // nil (disabled) when it cannot be opened; reviews work without it
	if err != nil {
		fmt.Fprintln(os.Stderr, "metrics.db disabled:", err)
	}
	p := newPoller(cfg.Repositories, ghFetch, ghLogin)
	q := newReviewQueue(cfg.Concurrency, newWorkflow(cfg, ghPolicy, runAgent), store)
	q.quietPeriod = cfg.QuietPeriod.Duration
	q.maxAge = cfg.autoReviewMaxAge
	q.skip = cfg.Skip
	q.record = mdb.record
	q.recordReview = mdb.recordReview
	ch := newConfigHolder(cfg, configPath)
	ch.onApply = func(c config) {
		p.setRepos(c.Repositories)
		q.setWorkflow(newWorkflow(c, ghPolicy, runAgent))
	}
	p.onPoll = func(s pollSnapshot) {
		q.sync(s.PRs)
		q.prune(s.Fetched, s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	handler = newHandler(ch, p, q, ghSubmitReview, ghApproved, ghFiles, ghContent, ghThreads, ghResolveThread)
	quit = func() {
		cancel()
		fmt.Println("myReview stopping")
		q.shutdown(2 * agentWaitDelay) // agents run in their own process groups; shutdown kills them
		mdb.close(2 * time.Second)
	}
	win = openWindow(app, startURL)
	setLoginShellPath()
	go p.run(ctx)
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		showStartupError(application.New(application.Options{Name: "myReview"}), err) // this app cannot run again
	}
}

func showWindow(win *application.WebviewWindow) {
	if win != nil {
		win.Show()
		win.Focus()
	}
}

// openWindow adds to app one window at url that hides on close and stays on the app origin, the app menu
// (Wails' AppMenu role plus Settings…) and the Edit menu, and the menu-bar icon.
func openWindow(app *application.App, url string) *application.WebviewWindow {
	var win *application.WebviewWindow
	menu := application.NewMenu()
	appMenu := menu.AddSubmenu("myReview") // macOS shows the first submenu as the app menu
	appMenu.AddRole(application.About)
	appMenu.AddSeparator()
	appMenu.Add("Settings…").SetAccelerator("CmdOrCtrl+,").OnClick(func(*application.Context) {
		showWindow(win)
		win.SetURL("/settings")
	})
	appMenu.AddSeparator()
	appMenu.AddRole(application.ServicesMenu)
	appMenu.AddSeparator()
	appMenu.AddRole(application.Hide)
	appMenu.AddRole(application.HideOthers)
	appMenu.AddRole(application.UnHide)
	appMenu.AddSeparator()
	appMenu.AddRole(application.Quit)
	menu.AddRole(application.EditMenu)
	app.Menu.Set(menu)
	win = app.Window.NewWithOptions(application.WebviewWindowOptions{Title: "myReview", Width: 1280, Height: 860, URL: url})
	// Closing the window hides it; polling and reviews keep running until Quit.
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		win.Hide()
		e.Cancel()
	})
	// Backstop for links.js: macOS Wails cannot refuse a navigation, so a page that left the app origin
	// (a dropped link, a script) is replaced by the dashboard at once; http(s) goes to the system browser.
	win.OnWindowEvent(events.Mac.WebViewDidCommitNavigation, func(*application.WindowEvent) {
		if back, open := leaveApp(webViewURL(win)); back {
			win.SetURL("/")
			if open != "" {
				app.Browser.OpenURL(open)
			}
		}
	})
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) { showWindow(win) })
	tray := application.NewMenu()
	tray.Add("Open myReview").OnClick(func(*application.Context) { showWindow(win) })
	tray.AddSeparator()
	tray.Add("Quit myReview").OnClick(func(*application.Context) { app.Quit() })
	app.SystemTray.New().SetTemplateIcon(trayIcon).SetMenu(tray)
	return win
}

// leaveApp says whether the window showing url must go back to the app, and the http(s) url to open in the
// system browser instead ("" for any other scheme). "" (no URL yet) stays.
func leaveApp(u string) (back bool, open string) {
	if u == "" || strings.HasPrefix(u, appOrigin+"/") {
		return false, ""
	}
	if pu, err := url.Parse(u); err == nil && (pu.Scheme == "http" || pu.Scheme == "https") && pu.Host != "" {
		return true, u
	}
	return true, ""
}

// showStartupError shows err in a native error dialog on app, which has not run yet, and exits with status 1.
func showStartupError(app *application.App, err error) {
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		d := app.Dialog.Error().SetTitle("myReview cannot start").SetMessage(err.Error())
		d.AddButton("Quit").SetAsDefault().OnClick(func() { os.Exit(1) }) // app.Quit would exit 0 and run OnShutdown
		d.Show()
	})
	app.Run()
	os.Exit(1) // only if the app stops without the dialog's Quit
}
