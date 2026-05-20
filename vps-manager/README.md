# VPS Manager

A desktop app to manage multiple VPSes over SSH — browse files, manage Docker containers, and run shell commands from one place.

Built with [Wails v2](https://wails.io) (Go backend + web UI). Binary is ~15 MB.

## Features

- Add/edit/delete VPS configs (host, port, user, key or password auth)
- Connect via SSH with key or password
- Browse remote filesystem; download, upload, and delete files via native file dialogs
- View Docker containers; start/stop/restart; view logs
- Run arbitrary shell commands with output history (`↑` / `↓` to navigate)
- Per-VPS persistent connections (no lag from reconnecting on every action)

## Prerequisites

- **Go** 1.22+
- **Node.js** 18+
- **Wails CLI v2**:
  ```bash
  go install github.com/wailsapp/wails/v2/cmd/wails@latest
  ```

Run `wails doctor` to verify your toolchain.

## Run it

```bash
# From the project root:
wails dev
```

The first run will:
1. Download Go modules
2. Install npm packages in `frontend/`
3. Generate the JS bindings into `frontend/wailsjs/`
4. Open a dev window with hot reload

## Build a production binary

```bash
wails build
```

Output lands in `build/bin/vps-manager` (or `.exe` on Windows). It's a single executable — no installer needed for testing.

## Project layout

```
.
├── main.go                  # Wails entrypoint
├── app.go                   # Methods exposed to the frontend
├── wails.json               # Wails config
├── internal/
│   ├── config/              # VPS config persistence (JSON in user config dir)
│   ├── ssh/                 # SSH connection pool, command execution
│   ├── sftp/                # File ops (list, download, upload, delete)
│   └── docker/              # Docker CLI helpers (over SSH)
└── frontend/
    ├── index.html
    ├── package.json
    ├── vite.config.js
    └── src/
        ├── main.js          # UI logic + Wails bindings
        └── style.css        # Dark theme styles
```

## Where your config lives

The app stores VPS configs as JSON:

- Linux: `~/.config/vps-manager/config.json`
- macOS: `~/Library/Application Support/vps-manager/config.json`
- Windows: `%APPDATA%\vps-manager\config.json`

File mode is `0600` so only your user can read it.

## Security caveats — please read before using on real servers

This is a starter project. Before pointing it at production, harden these:

1. **Host key verification** — `internal/ssh/client.go` uses `InsecureIgnoreHostKey()`. Replace it with [`golang.org/x/crypto/ssh/knownhosts`](https://pkg.go.dev/golang.org/x/crypto/ssh/knownhosts) so MITM attacks fail.
2. **Password storage** — passwords are stored plaintext in `config.json`. Swap to [`zalando/go-keyring`](https://github.com/zalando/go-keyring) to use the OS keychain.
3. **SSH agent** — add agent forwarding support so the app never needs to read your private keys directly.
4. **Encrypted private keys** — `ssh.ParsePrivateKey` doesn't handle passphrase-protected keys; use `ssh.ParsePrivateKeyWithPassphrase` and prompt the user.

## Good next features to add

These are not implemented but would slot in cleanly:

- **Interactive PTY terminal** via `xterm.js` + Wails events (the current terminal is one-shot exec; fine for `ls` and `systemctl restart`, no good for `vim` or `htop`)
- **File preview / edit** for text files (download → edit in a modal → upload)
- **Multi-select** file operations (bulk download / delete)
- **System monitoring panel** (`top`, `df -h`, `free -h`, parsed and displayed)
- **`docker compose`** support — list services, up/down individual ones
- **Saved command snippets** per-VPS

## Why these choices?

- **Go + Wails over Electron**: 10× smaller binary, instant startup, no Chromium baggage.
- **SFTP over `scp`**: `pkg/sftp` exposes a Go file API (Open, ReadDir, Create) which is much nicer than shelling out and parsing output.
- **`docker ... --format '{{json .}}'` over Docker SDK**: avoids needing to expose the Docker socket or tunnel it. Works on any host with Docker installed.
- **Vanilla JS + Vite over React**: the UI is straightforward enough that a framework would be overkill. Easy to swap for React/Svelte later — Wails supports all three.

## License

MIT — do whatever you want with it.
