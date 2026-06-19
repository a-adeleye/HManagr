# VPS Manager

A desktop app to manage multiple VPSes over SSH — browse files, manage Docker containers, and run shell commands from one place.

Built with [Wails v2](https://wails.io) (Go backend + web UI). Binary is ~15 MB.

## Features

- **Local environment** — a built-in "Local (this machine)" entry (always pinned at
  the top of the sidebar) manages the Docker daemon on your own computer with no SSH.
  Docker, the database manager, GitHub deploys, and a native file browser all work
  locally; the interactive terminal and migration are SSH-only. On Windows this needs
  a POSIX shell (Git for Windows bundles one) since commands run through bash/sh.
- Add/edit/delete VPS configs (host, port, user, key or password auth)
- Connect via SSH with key or password
- Browse remote filesystem; download, upload, and delete files via native file dialogs
- View Docker containers; start/stop/restart; view logs
- **Database manager** — browse Postgres/MySQL/MariaDB databases running in Docker
  containers: list databases & tables, page through rows, insert/edit/delete rows
  (primary-key aware), and run arbitrary SQL with a results grid. No DB port needs
  to be exposed — everything goes through `docker exec` + the engine's own CLI, and
  credentials are sniffed from the container environment (never leave the backend).
- **Deploy from GitHub** — Coolify-style: point a deployment at a repo + VPS + path,
  define env vars / secrets, and it clones (or fetches + resets), writes a `.env`,
  and runs `docker compose up -d --build` with a live streamed log. Private repos
  authenticate with a GitHub token that's injected only for the clone and never
  persisted on the VPS.
- **Migration** — move a docker-compose stack (compose file, env files, named
  volumes) from one VPS to another. The "use sudo" checkbox assumes passwordless
  sudo (or a root login) — no password prompting; permission failures surface as-is.
- Interactive PTY terminal (xterm.js) with copy/paste
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
│   ├── config/              # VPS + deployment persistence (JSON in user config dir)
│   ├── ssh/                 # SSH connection pool, command execution, streaming
│   ├── sftp/                # File ops (list, download, upload, delete)
│   ├── docker/              # Docker CLI helpers (over SSH)
│   ├── db/                  # Database manager (docker exec → psql/mysql)
│   ├── deploy/              # GitHub repo → VPS via docker compose
│   └── migration/           # Move a compose stack between VPSes
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

- **Multi-select** file operations (bulk download / delete)
- **System monitoring panel** (`top`, `df -h`, `free -h`, parsed and displayed)
- **Deploy webhooks** — trigger a deploy from a GitHub push event
- **Postgres `pg_dump` migration mode** — logical dump/restore as an alternative to volume archiving (the `PostgresMode` field is already scaffolded in `migration.Plan`)
- **Saved command snippets** per-VPS
- **DB export** — dump a table or query result to CSV locally

## Why these choices?

- **Go + Wails over Electron**: 10× smaller binary, instant startup, no Chromium baggage.
- **SFTP over `scp`**: `pkg/sftp` exposes a Go file API (Open, ReadDir, Create) which is much nicer than shelling out and parsing output.
- **`docker ... --format '{{json .}}'` over Docker SDK**: avoids needing to expose the Docker socket or tunnel it. Works on any host with Docker installed.
- **Vanilla JS + Vite over React**: the UI is straightforward enough that a framework would be overkill. Easy to swap for React/Svelte later — Wails supports all three.

## License

MIT — do whatever you want with it.
