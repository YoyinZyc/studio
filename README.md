# AX Studio

Local web application for [AX](https://github.com/google/ax): set up local or remote agent clusters, compose tasks with guardrails, monitor live execution, review and apply sandbox Git diffs locally, continue multi-turn sessions, and promote tasks across Kubernetes clusters.

AX Studio ships as a **single Go binary (`ax-studio`)** with an embedded React + TypeScript frontend served strictly on `http://127.0.0.1:8080`.

![Activity](docs/screenshots/overview.png)

## Core Workflows

- **Where Agents Run:** Run host preflight checks and start a self-contained local Kind + Agent Substrate + AX sandbox (`ax local start`), or connect directly to any remote team cluster in `~/.kube/config` (`ax install`) with zero local VM footprint.
- **Board & Activity:** Monitor tasks grouped into `Needs You`, `Working`, `Suspended`, and `Done`, inspect conditions, stream live logs, and open interactive `ax ssh` terminal tabs.
- **New Task & Guardrails:** Compose `Task`, `Workspace`, and `Gateway` manifests visually or in YAML, pick prebuilt agent templates, configure outbound HTTPS domain allow-lists, and store API keys as cluster `Secret`s (`ax.io/credential: "true"`) injected by `atenet-router` without exposing keys inside sandboxes.
- **Changes & Continue:** Inspect uncommitted Git diffs inside a running debug-enabled sandbox (`spec.debug: true`), discard individual files, apply patches directly onto a local Git repository (`git apply --check`), or launch a follow-up turn sharing `spec.session_id`.
- **Promote Across Clusters:** Run a 5-check preflight (cluster reachability, Substrate version compatibility, manifest validation, remote credential Secrets, and uncommitted sandbox changes), push dirty working-tree files to a Git branch, and promote `Gateway` + `Workspace` + `Task` onto a team cluster.

## Prerequisites

- [`ax` CLI](https://github.com/google/ax) installed (`~/.ax/bin/ax` or on `PATH`).
- For local sandboxes: Docker or rootful Podman on Linux (`>= 6.8` kernel), or OrbStack / Docker Desktop / Colima on macOS.
- For building from source: Go `1.27+` (`GOTOOLCHAIN=auto`) and Node.js `20+`.

## Quick Start

If you installed AX via `install.sh`, launch AX Studio directly:

```bash
ax studio                            # Opens http://127.0.0.1:8080 in your browser
```

Or build and run from source:

```bash
npm ci
make build
./bin/ax-studio
```

Useful flags:

```bash
./bin/ax-studio --port 8080          # Default loopback port (127.0.0.1:8080)
./bin/ax-studio --no-open            # Do not open a browser automatically (SSH/remote or scripts)
./bin/ax-studio -n ax-system         # Target a custom AX control-plane namespace
```

Stopping `ax studio` / `ax-studio` (`Ctrl-C`) closes only the local web server; any local Kind cluster and running Tasks in Kubernetes continue running unaffected.

## Uninstall & Clean Up

1. **Stop the Web UI:** Press `Ctrl-C` in the terminal running `ax studio` / `ax-studio`.
2. **Delete the local sandbox cluster (if created):**
   ```bash
   ax local delete
   ```
   *(Or click **Delete** on the Local Sandbox card in the **Environments** page before stopping AX Studio.)*
3. **Uninstall AX from a remote cluster (optional):**
   ```bash
   ax --context <remote-context> uninstall
   ```
4. **Remove local binaries and caches:** All files installed by `install.sh` or cached on demand (`ax`, `ax-studio`, tunnel metadata, short-lived `ax ssh` certificates, and any fallback `kind`/`kubectl` binaries not already on your `PATH`) live under `~/.ax/`:
   ```bash
   rm -rf ~/.ax
   ```
   Pre-existing system tools (`docker`, `podman`, system/Homebrew `kubectl` and `kind`) and other `~/.kube/config` contexts are never modified or removed. Remove `export PATH="$HOME/.ax/bin:$PATH"` from `~/.zshrc` or `~/.bashrc` if you added it.

## Development & Testing

```bash
npm run dev                          # Vite dev server for frontend iteration
make test                            # TypeScript check + Go race-detector unit tests
make verify                          # make test + gofmt + go vet
```

## Security Model

- **Loopback-Only BFF:** Refuses to bind to `0.0.0.0` or non-loopback interfaces; rejects non-loopback `Host` headers (DNS rebinding defense) and cross-origin `Origin` headers.
- **Per-Process Session & CSRF:** Issues an ephemeral 256-bit `HttpOnly; SameSite=Strict` session cookie and requires constant-time `X-AX-CSRF-Token` verification on all mutating `POST` endpoints, paired with strict `Content-Security-Policy` (`script-src 'self'`).
- **Allow-Listed Execution & Secret Redaction:** Restricts subprocess calls to allow-listed `ax`, `kubectl`, `kind`, and `git` subcommands, pipes credential Secret values strictly over `stdin`, and scrubs tokens from diagnostic logs.
