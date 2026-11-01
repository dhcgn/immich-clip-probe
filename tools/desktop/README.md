# Clip Check (desktop)

The desktop face of `clip-check`; see the root README, "Tools: clip-check". All
the work happens in [`../check`](../check), shared with the CLI. This module only
adds the window, the dialogs, file drops and the Svelte page in `frontend/`.

Needs Go, Node 22, [Task](https://taskfile.dev) and the Wails CLI
(`go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0`); on Linux also
`libgtk-4-dev libwebkitgtk-6.0-dev`.

```bash
task dev      # hot reload
task build    # bin/clip-check-desktop[.exe]; also regenerates frontend/bindings, which are committed
```

The tools module comes from `../` through the `replace` in `go.mod`, so this
module builds only inside the repository.
