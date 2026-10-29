# berth

Docker Engine on Windows without Docker Desktop. Stock Docker Engine (Ubuntu's `docker.io`) runs in a
WSL distro of its own, and berth serves it on Windows named pipes:

- `\\.\pipe\berth`, always while it's on, and
- `\\.\pipe\docker_engine`, Docker's default, whenever nothing else serves it. So the docker CLI,
  Testcontainers, Rider and anything else that looks for Docker on Windows find it without settings.
  While Docker Desktop runs, berth leaves that pipe to it and takes it back once Desktop quits.

One user, no admin. The pipes accept only your user (their security descriptor names your SID and
nobody else), and refuse remote clients.

## Setup

Needs WSL 2 (`wsl --install --no-distribution` if it isn't there). Get `berth.exe` and `berthw.exe`
from [Releases](https://github.com/bergaaberg/berth/releases) (or build them, below), then:

```
berth setup
```

That:

1. puts berth in `%LOCALAPPDATA%\berth\bin` and on your PATH;
2. makes the `berth` distro (Ubuntu 24.04 with Docker Engine, compose and buildx), or adopts Dovre's
   `dovre-docker` distro with its images, containers and volumes;
3. has the distro trust what Windows trusts, so pulls work behind a TLS-inspecting proxy;
4. fetches the Windows docker CLI, compose and buildx (`~/.docker/cli-plugins`) and the wincred
   credential helper, checked against their releases' sums;
5. adds a `berth` docker context, starts berth at login, and starts it;
6. switches the docker CLI to berth (`berth use`), unless Docker Desktop is running.

## Use

| Command | What it does |
|---|---|
| `berth status` | the distro, the engine, the pipes, who holds `docker_engine` (`--json` for programs) |
| `berth stop` | stops the engine and its containers. Nothing starts it again but `berth start`, across logins too |
| `berth start` | serves the engine again |
| `berth use` | the docker CLI's context to `berth`, and Docker Desktop's credential helper to wincred (both keep logins in Credential Manager, so you stay logged in) |
| `berth cli` | the newest docker CLI, compose, buildx and wincred |
| `berth update` | upgrades Docker Engine in the distro, then the CLI |
| `berth install` / `uninstall` | start at login or don't (the HKCU Run key; no admin) |
| `berth log` | the last lines of `%LOCALAPPDATA%\berth\berth.log` |
| `berth remove` | unregisters the distro, with its images, containers and volumes |

## Leaving Docker Desktop

1. `berth setup` while Desktop is still installed (it can even fetch Ubuntu through Desktop's docker
   if WSL's own download is blocked).
2. Quit Docker Desktop: within seconds berth serves `docker_engine` too.
3. `berth use`, then uninstall Docker Desktop. Its `docker.exe` goes with it; berth's stays on your
   PATH.

## Good to know

- **Ports** published by containers are on `localhost` (WSL 2 forwards them).
- **Bind mounts** from Windows paths work (`-v C:\src\app:/app` reaches `/mnt/c/...`) but are slow.
  For heavy I/O, keep the project inside the distro: `wsl -d berth`.
- **Memory and CPU** are WSL's, set in `%USERPROFILE%\.wslconfig`. That file is global to WSL: it
  limits every distro, not just berth's.
- **WSL stops** a distro shortly after the last Windows process attached to it ends. berth keeps one
  attached while it's on, so containers keep running with no terminal open.

## How it works

`berth serve` (started at login as `berthw.exe serve`, with no window) checks every two seconds what
it should serve: nothing when stopped; its own pipe when on; `docker_engine` too when it's free and
Docker Desktop isn't running. Each connection is relayed into the distro by `wsl.exe` running `socat`
against `/var/run/docker.sock`. The plain logic (what to serve, file formats, command lines) is in
`logic.go` and tested on any OS; the Windows calls are in `win.go`.

## Build

```
go test ./...
go build -o berth.exe .
go build -ldflags -H=windowsgui -o berthw.exe .
```

## Dovre

[Dovre](https://github.com/bergaaberg/dovre)'s Docker page uses berth when it's installed: it reads
berth's pipe, and Start and Stop Docker run `berth start` and `berth stop`.

## License

MIT
