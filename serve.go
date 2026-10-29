//go:build windows

package main

// `berth serve`: the background process (started at login by the Run key, or by `berth start`).
// Every two seconds it squares what it serves with what it should (PlanPipes): its own pipe while
// on, docker_engine too when that's free, and one process attached to the distro so WSL keeps it
// (and its containers) running. Stopped (`berth stop`, the off file) it serves nothing and lets
// the distro go. Each connection is relayed into the distro: wsl.exe running socat against
// /var/run/docker.sock.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"
)

func isOn() bool { return !exists(offFile()) }

func serve() error {
	if !TakeLock() {
		return fmt.Errorf("berth is serving already")
	}
	logf("serving (pid %d)", os.Getpid())
	var own, def *Listener
	var keep *exec.Cmd
	var keepEnded chan struct{}
	var last State
	haveDistro, checked := false, time.Time{}
	since := time.Now().Format(time.RFC3339)

	open := func(name string) *Listener {
		l, err := Listen(name, relayOne)
		if err != nil {
			logf("couldn't serve %s: %v", PipePath(name), err)
			return nil
		}
		logf("serving %s", PipePath(name))
		return l
	}
	shut := func(l **Listener) {
		if *l != nil {
			(*l).Close()
			logf("stopped serving %s", PipePath((*l).name))
			*l = nil
		}
	}

	for {
		if time.Since(checked) > 30*time.Second || !haveDistro {
			haveDistro, checked = distroExists(), time.Now()
		}
		on := isOn() && haveDistro
		desktop := DesktopRunning()
		holding := def != nil
		other := !holding && pipeServed(DefaultPipe)
		plan := PlanPipes(on, holding, other, desktop)

		if plan.Own && own == nil {
			own = open(OwnPipe)
		} else if !plan.Own {
			shut(&own)
		}
		if plan.Default && def == nil {
			def = open(DefaultPipe)
		} else if !plan.Default && def != nil {
			if desktop {
				logf("Docker Desktop is running: handing %s back", PipePath(DefaultPipe))
			}
			shut(&def)
		}

		// The process that keeps the distro up.
		if keep != nil {
			select {
			case <-keepEnded:
				keep = nil
			default:
			}
		}
		if on && keep == nil {
			cmd := exec.Command("wsl.exe", keepArgs()...)
			cmd.Env = append(os.Environ(), "WSL_UTF8=1")
			hide(cmd)
			if err := cmd.Start(); err == nil {
				keep, keepEnded = cmd, make(chan struct{})
				go func(c *exec.Cmd, done chan struct{}) { _ = c.Wait(); close(done) }(cmd, keepEnded)
			} else {
				logf("couldn't keep the distro up: %v", err)
			}
		} else if !on && keep != nil {
			_ = keep.Process.Kill()
			keep = nil
		}

		now := State{PID: os.Getpid(), On: on, Since: since}
		for _, l := range []*Listener{own, def} {
			if l != nil {
				now.Pipes = append(now.Pipes, PipePath(l.name))
			}
		}
		if now.On != last.On || !slices.Equal(now.Pipes, last.Pipes) {
			last = now
			if b, err := json.Marshal(now); err == nil {
				_ = os.WriteFile(stateFile(), b, 0o644)
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// relayOne carries one connection into the distro and back.
func relayOne(h syscall.Handle) {
	conn := os.NewFile(uintptr(h), "berth-pipe")
	defer func() {
		if r := recover(); r != nil {
			logf("relay: %v", r)
		}
		endClient(h)
		_ = conn.Close()
	}()
	if !isOn() {
		return
	}
	cmd := exec.Command("wsl.exe", relayArgs()...)
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	hide(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		logf("relay: %v", err)
		return
	}
	go func() {
		_, _ = io.Copy(in, conn)
		_ = in.Close() // socat ends shortly after its input does
	}()
	_, _ = io.Copy(conn, out)
	_ = cmd.Wait()
}

func readState() (State, bool) {
	var s State
	b, err := os.ReadFile(stateFile())
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, false
	}
	return s, true
}
