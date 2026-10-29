// berth: Docker Engine on Windows without Docker Desktop. Stock Docker Engine runs in a WSL distro
// of its own; berth serves it on \\.\pipe\berth, and on \\.\pipe\docker_engine whenever nothing
// else serves that, so the docker CLI, Testcontainers and Rider find it without settings. One user,
// no admin.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

const usage = `berth: Docker Engine on Windows without Docker Desktop

  berth setup       make the engine (adopting Dovre's dovre-docker distro if there is one), the
                    docker CLI with compose and buildx, the berth context, start at login, start
                      --yes       answer yes (adopting a distro)
                      --no-cli    leave the docker CLI and its plugins alone
                      --no-login  don't start at login
  berth start       serve the engine (and start the background process if it isn't running)
  berth stop        stop the engine and its containers; nothing starts it again but berth start
  berth status      the distro, the engine, the pipes, Docker Desktop (--json for programs)
  berth use         make the docker CLI use berth (context berth, wincred for logins)
  berth cli         fetch the newest docker CLI, compose, buildx and wincred
  berth update      upgrade Docker Engine in the distro, then the CLI
  berth install     start at login (uninstall: don't)
  berth log         the last lines of berth's log
  berth remove      unregister the distro, with its images, containers and volumes (--yes)
  berth version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "berth:", err)
		os.Exit(1)
	}
}
