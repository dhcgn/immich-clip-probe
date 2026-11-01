// Command clip-check asks a clip-probe server whether local images are already
// in Immich: for each file, folder or set of files it prints the nearest Immich
// asset and its CLIP distance.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/dhcgn/immich-clip-probe/tools/check"
	"github.com/urfave/cli/v3"
)

// version is set at build time via -ldflags.
var version = "dev"

// flagKeys maps each config flag to its key in the config file.
var flagKeys = map[string]string{
	"server": "server", "token": "token", "max-distance": "max_distance",
	"limit": "limit", "workers": "workers", "magick": "magick",
}

func main() {
	cmd := &cli.Command{
		Name:      "clip-check",
		Usage:     "check whether local images are already in Immich, via clip-probe",
		ArgsUsage: "<file|folder>...",
		Version:   version,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Usage: "config file (default " + check.DefaultPath() + ")"},
			&cli.StringFlag{Name: "server", Usage: "clip-probe base URL"},
			&cli.StringFlag{Name: "token", Usage: "clip-probe API token (prefer CLIP_CHECK_TOKEN or the config file)"},
			&cli.FloatFlag{Name: "max-distance", Usage: "cosine distance up to which the nearest asset counts as a match (default 0.01)", HideDefault: true},
			&cli.IntFlag{Name: "limit", Usage: "nearest assets to list per file (default 1)", HideDefault: true},
			&cli.IntFlag{Name: "workers", Usage: "files checked in parallel (default 4)", HideDefault: true},
			&cli.StringFlag{Name: "magick", Usage: `ImageMagick binary, or "off" (default: found on PATH or in Program Files)`},
			&cli.BoolFlag{Name: "json", Usage: "print one JSON result per line"},
		},
		Action: run,
		Commands: []*cli.Command{{
			Name:   "health",
			Usage:  "show the server's state and whether ImageMagick was found",
			Action: health,
		}, {
			Name:   "version",
			Usage:  "print the clip-check version",
			Action: printVersion,
		}},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cmd.Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "clip-check:", err)
		os.Exit(1)
	}
}

// config is the file, then CLIP_CHECK_* env, then whatever flags were given.
func config(cmd *cli.Command) (check.Config, error) {
	v, err := check.Load(cmd.String("config"))
	if err != nil {
		return check.Config{}, err
	}
	for flag, key := range flagKeys {
		if cmd.IsSet(flag) {
			v.Set(key, cmd.Value(flag))
		}
	}
	return check.Decode(v)
}

func run(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() == 0 {
		return cli.ShowAppHelp(cmd)
	}
	cfg, err := config(cmd)
	if err != nil {
		return err
	}
	files, err := check.Collect(cmd.Args().Slice())
	if err != nil {
		return err
	}

	out := cmd.Root().Writer
	enc := json.NewEncoder(out)
	counts := map[check.Status]int{}
	err = check.Run(ctx, cfg, files, func(r check.Result) {
		counts[r.Status]++
		if cmd.Bool("json") {
			enc.Encode(r)
			return
		}
		printResult(out, r)
	})
	if err != nil {
		return err
	}
	if !cmd.Bool("json") {
		fmt.Fprintf(out, "%d files: %d match, %d no match, %d skipped, %d errors\n", len(files),
			counts[check.StatusMatch], counts[check.StatusNoMatch], counts[check.StatusSkipped], counts[check.StatusError])
	}
	if n := counts[check.StatusError]; n > 0 {
		return fmt.Errorf("%d of %d files failed", n, len(files))
	}
	return ctx.Err()
}

func printResult(w io.Writer, r check.Result) {
	switch r.Status {
	case check.StatusSkipped:
		fmt.Fprintf(w, "SKIP          %s  (%s)\n", r.Path, r.Error)
	case check.StatusError:
		fmt.Fprintf(w, "ERROR         %s  (%s)\n", r.Path, r.Error)
	default:
		label := "MATCH"
		if r.Status == check.StatusNoMatch {
			label = "NO"
		}
		if len(r.Matches) == 0 {
			fmt.Fprintf(w, "%-5s    -    %s  (nothing indexed)\n", label, r.Path)
			return
		}
		for i, m := range r.Matches {
			if i == 0 {
				fmt.Fprintf(w, "%-5s %.4f  %s → %s", label, m.Distance, r.Path, describe(m))
			} else {
				fmt.Fprintf(w, "      %.4f  %*s → %s", m.Distance, len(r.Path), "", describe(m))
			}
			fmt.Fprintln(w)
		}
	}
}

func describe(m check.Match) string {
	s := m.OriginalFileName
	if s == "" {
		s = m.AssetID
	}
	if m.LocalDateTime != nil {
		s += " " + m.LocalDateTime.Format("2006-01-02")
	}
	if m.Links != nil {
		s += " " + m.Links.Web
	}
	return s
}

func printVersion(_ context.Context, cmd *cli.Command) error {
	root := cmd.Root()
	fmt.Fprintf(root.Writer, "%s %s\n", root.Name, root.Version)
	return nil
}

func health(ctx context.Context, cmd *cli.Command) error {
	cfg, err := config(cmd)
	if err != nil {
		return err
	}
	w := cmd.Root().Writer
	fmt.Fprintf(w, "server   %s\n", cfg.Server)
	if magick := check.FindMagick(cfg.Magick); magick != "" {
		fmt.Fprintf(w, "magick   %s\n", magick)
	} else {
		fmt.Fprintln(w, "magick   not found: HEIC, RAW and other formats clip-probe cannot decode are skipped")
	}
	if cfg.Server == "" {
		return fmt.Errorf("no server configured: set server in %s or pass --server", check.DefaultPath())
	}
	h, err := check.NewClient(cfg.Server, cfg.Token).Health(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "status   %s (db %s, ml %s)\n", h.Status, h.DB, h.ML)
	fmt.Fprintf(w, "model    %s (%d dimensions)\n", h.Model, h.Dimensions)
	fmt.Fprintf(w, "indexed  %d assets\n", h.IndexedAssets)
	if h.Message != "" {
		fmt.Fprintf(w, "message  %s\n", h.Message)
	}
	if h.Status != "ok" {
		return fmt.Errorf("server is not healthy")
	}
	return nil
}
