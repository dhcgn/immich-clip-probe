// Command clip-check-desktop is Clip Check: drop images or folders on the
// window and see, for each, the nearest Immich asset and its CLIP distance.
// The work is done by the check package, shared with the clip-check CLI.
package main

import (
	"embed"
	"log"

	"github.com/dhcgn/immich-clip-probe/tools/check"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

// version is set at build time via -ldflags.
var version = "dev"

func init() {
	application.RegisterEvent[check.Result]("result") // one per checked file
	application.RegisterEvent[string]("done")         // empty, or why the check stopped
	application.RegisterEvent[[]string]("dropped")    // paths dropped on the window
}

func main() {
	app := application.New(application.Options{
		Name:        "Clip Check",
		Description: "Check whether local images are already in Immich",
		Services:    []application.Service{application.NewService(&CheckService{})},
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		Mac:         application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: true},
	})

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:          "Clip Check",
		Width:          1100,
		Height:         720,
		EnableFileDrop: true,
		URL:            "/",
	})
	// Drops land on the element marked data-file-drop-target; the page decides
	// what to do with the paths, as it does for the file dialogs.
	win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		app.Event.Emit("dropped", e.Context().DroppedFiles())
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
