package main

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/dhcgn/immich-clip-probe/tools/check"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// CheckService is everything the page calls. Settings live in the same file
// the CLI reads (check.DefaultPath).
type CheckService struct {
	mu      sync.Mutex
	cancel  context.CancelFunc // set while a check runs
	checked map[string]bool    // every file a check was started on: what /thumb/local may show
}

// ServerInfo is what the status bar shows.
type ServerInfo struct {
	Version    string        `json:"version"`
	ConfigPath string        `json:"configPath"`
	Magick     string        `json:"magick"` // empty: not found, or switched off
	Health     *check.Health `json:"health,omitempty"`
	Error      string        `json:"error,omitempty"` // settings or clip-probe
	Immich     *ImmichInfo   `json:"immich,omitempty"`
}

func settings() (check.Config, error) {
	v, err := check.Load("")
	if err != nil {
		return check.Config{}, err
	}
	return check.Decode(v)
}

func (s *CheckService) Settings() (check.Config, error) { return settings() }

func (s *CheckService) SaveSettings(c check.Config) error { return check.Save("", c) }

func (s *CheckService) Info(ctx context.Context) ServerInfo {
	info := ServerInfo{Version: version, ConfigPath: check.DefaultPath()}
	cfg, err := settings()
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Magick = check.FindMagick(cfg.Magick)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	if cfg.ImmichURL != "" && cfg.ImmichKey != "" {
		wg.Go(func() { info.Immich = immichStatus(ctx, cfg) })
	}
	if cfg.Server == "" {
		info.Error = "no server configured"
	} else if info.Health, err = check.NewClient(cfg.Server, cfg.Token).Health(ctx); err != nil {
		info.Error = err.Error()
	}
	wg.Wait()
	return info
}

// Pick opens the system dialog for files, or for one folder. Cancelling it
// returns nothing.
func (s *CheckService) Pick(folder bool) ([]string, error) {
	d := application.Get().Dialog.OpenFile()
	if folder {
		p, err := d.SetTitle("Check a folder").CanChooseDirectories(true).CanChooseFiles(false).PromptForSingleSelection()
		if p == "" {
			return nil, err
		}
		return []string{p}, err
	}
	return d.SetTitle("Check images").PromptForMultipleSelection()
}

// PickMagick lets the user point at ImageMagick's program by hand.
func (s *CheckService) PickMagick() (string, error) {
	d := application.Get().Dialog.OpenFile().SetTitle("Select ImageMagick's program, magick.exe")
	if runtime.GOOS == "windows" {
		d = d.AddFilter("ImageMagick (magick.exe)", "magick.exe")
	}
	return d.PromptForSingleSelection()
}

// Check starts checking paths in the background and returns how many files
// that is. Each file's outcome arrives as a "result" event, the end as "done".
func (s *CheckService) Check(paths []string) (int, error) {
	cfg, err := settings()
	if err != nil {
		return 0, err
	}
	files, err := check.Collect(paths)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return 0, errors.New("a check is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.checked = map[string]bool{}
	for _, f := range files {
		s.checked[f] = true
	}

	app := application.Get()
	go func() {
		err := check.Run(ctx, cfg, files, func(r check.Result) { app.Event.Emit("result", r) })
		msg := ""
		switch {
		case err != nil:
			msg = err.Error()
		case ctx.Err() != nil:
			msg = "cancelled"
		}
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
		cancel()
		app.Event.Emit("done", msg)
	}()
	return len(files), nil
}

func (s *CheckService) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

// OpenURL opens an Immich link in the system browser, where the user is
// signed in; the webview is not.
func (s *CheckService) OpenURL(url string) error { return application.Get().Browser.OpenURL(url) }
