//go:build windows

package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Where berth keeps itself: %LOCALAPPDATA%\berth (bin\ is on the user's PATH).
func dir() string     { return filepath.Join(os.Getenv("LOCALAPPDATA"), "berth") }
func binDir() string  { return filepath.Join(dir(), "bin") }
func offFile() string { return filepath.Join(dir(), "off") }
func stateFile() string {
	return filepath.Join(dir(), "state.json")
}
func logFile() string { return filepath.Join(dir(), "berth.log") }
func dockerDir() string {
	return filepath.Join(os.Getenv("USERPROFILE"), ".docker")
}

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "berth"
const envKey = `HKCU\Environment`

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func logf(format string, a ...any) {
	_ = os.MkdirAll(dir(), 0o755)
	if st, err := os.Stat(logFile()); err == nil && st.Size() > 1<<20 {
		_ = os.Rename(logFile(), logFile()+".1")
	}
	f, err := os.OpenFile(logFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

func wsl(args ...string) ([]byte, error) {
	cmd := exec.Command("wsl.exe", args...)
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	hide(cmd)
	return cmd.CombinedOutput()
}

func distroExists() bool {
	out, err := wsl("--list", "--quiet")
	return err == nil && HasDistro(out, Distro)
}

func distroState(name string) string {
	out, _ := wsl("--list", "--verbose")
	return DistroState(out, name)
}

// inDistro runs a bash script in the distro as root, its output on ours.
func inDistro(script string) error {
	cmd := exec.Command("wsl.exe", "-d", Distro, "-u", "root", "--", "bash", "-euo", "pipefail", "-s")
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(script, "\r", ""))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the step in %s failed: %w", Distro, err)
	}
	return nil
}

func reg(args ...string) (string, error) {
	cmd := exec.Command("reg.exe", args...)
	hide(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("reg %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func ask(question string, yes bool) bool {
	if yes {
		return true
	}
	fmt.Print(question + " [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

type opts struct {
	yes, noCLI, noLogin, json bool
}

func parseOpts(args []string) (opts, error) {
	var o opts
	for _, a := range args {
		switch a {
		case "-y", "--yes":
			o.yes = true
		case "--no-cli":
			o.noCLI = true
		case "--no-login":
			o.noLogin = true
		case "--json":
			o.json = true
		default:
			return o, fmt.Errorf("unknown option %s", a)
		}
	}
	return o, nil
}

// ---- setup ----

func setup(o opts) error {
	if err := installSelf(); err != nil {
		return err
	}
	if err := makeDistro(o); err != nil {
		return err
	}
	certs, err := RootCerts()
	if err != nil {
		return fmt.Errorf("couldn't read Windows' root certificates: %w", err)
	}
	var pem strings.Builder
	for _, c := range certs {
		pem.WriteString(PEM(c))
	}
	fmt.Printf("Setting up Docker Engine in %s (trusting Windows' %d root certificates)...\n", Distro, len(certs))
	if err := inDistro(SetupScript(pem.String())); err != nil {
		return err
	}
	// The new wsl.conf (systemd) takes effect on the next start.
	_, _ = wsl("--terminate", Distro)
	if err := inDistro(WaitScript); err != nil {
		return err
	}
	if !o.noCLI {
		if err := installCLI(false); err != nil {
			fmt.Println("The docker CLI wasn't installed:", err)
			fmt.Println("Try again with `berth cli`.")
		}
	}
	if err := writeContext(); err != nil {
		return err
	}
	if !o.noLogin {
		if err := installLogin(); err != nil {
			return err
		}
	}
	if err := start(); err != nil {
		return err
	}
	if DesktopRunning() {
		fmt.Println("Docker Desktop is running: it keeps docker_engine, and berth serves " + PipePath(OwnPipe) + ".")
		fmt.Println("Quit or uninstall Docker Desktop and berth takes docker_engine within seconds. `berth use` makes the docker CLI use berth now.")
	} else if err := use(); err != nil {
		return err
	}
	fmt.Println()
	return status(opts{})
}

// installSelf copies this binary (and berthw.exe beside it) into %LOCALAPPDATA%\berth\bin and puts
// that on the user's PATH.
func installSelf() error {
	if err := os.MkdirAll(binDir(), 0o755); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	for _, name := range []string{"berth.exe", "berthw.exe"} {
		src := filepath.Join(filepath.Dir(self), name)
		dst := filepath.Join(binDir(), name)
		if !exists(src) || strings.EqualFold(filepath.Clean(src), filepath.Clean(dst)) {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := replaceFile(dst, b); err != nil {
			return err
		}
	}
	return addUserPath(binDir())
}

// replaceFile writes a program even while the old one runs (Windows lets a running .exe be renamed,
// not overwritten).
func replaceFile(dst string, b []byte) error {
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	if exists(dst) {
		old := dst + ".old"
		_ = os.Remove(old)
		if err := os.Rename(dst, old); err != nil {
			return err
		}
		defer os.Remove(old) // fails quietly while it runs; the next replace clears it
	}
	return os.Rename(tmp, dst)
}

func userPath() (string, string) {
	out, err := reg("query", envKey, "/v", "Path")
	if err != nil {
		return "", "REG_EXPAND_SZ"
	}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.EqualFold(f[0], "Path") && strings.HasPrefix(f[1], "REG_") {
			i := strings.Index(line, f[1]) + len(f[1])
			return strings.TrimSpace(line[i:]), f[1]
		}
	}
	return "", "REG_EXPAND_SZ"
}

func addUserPath(d string) error {
	cur, typ := userPath()
	next, changed := AddToPath(cur, d)
	if !changed {
		return nil
	}
	if _, err := reg("add", envKey, "/v", "Path", "/t", typ, "/d", next, "/f"); err != nil {
		return err
	}
	SettingChanged()
	fmt.Println("Added " + d + " to your PATH (new terminals have it).")
	return nil
}

func removeUserPath(d string) {
	cur, typ := userPath()
	if next, found := RemoveFromPath(cur, d); found {
		_, _ = reg("add", envKey, "/v", "Path", "/t", typ, "/d", next, "/f")
		SettingChanged()
	}
}

func makeDistro(o opts) error {
	out, err := wsl("--list", "--quiet")
	if err != nil {
		return fmt.Errorf("WSL doesn't answer (is it installed? `wsl --install --no-distribution`): %s", strings.TrimSpace(wslText(out)))
	}
	if HasDistro(out, Distro) {
		return nil
	}
	if HasDistro(out, OldDistro) {
		if ask(fmt.Sprintf("Adopt %s (Dovre's engine) as %s, keeping its images, containers and volumes?", OldDistro, Distro), o.yes) {
			return adopt()
		}
	}
	fmt.Printf("Installing Ubuntu 24.04 as %s...\n", Distro)
	cmd := exec.Command("wsl.exe", "--install", "Ubuntu-24.04", "--name", Distro, "--no-launch")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if cmd.Run() == nil && distroExists() {
		return nil
	}
	// A TLS-inspecting proxy may reset WSL's download (0x80072eff). A docker that still works
	// (Docker Desktop's) can pull the same Ubuntu as an image; its files become the distro.
	if exec.Command("docker", "version").Run() != nil {
		return errors.New("wsl --install failed, and there's no running docker to fetch Ubuntu with")
	}
	fmt.Println("The download was refused; taking Ubuntu 24.04 from its Docker image instead...")
	tar := filepath.Join(os.TempDir(), "berth-ubuntu.tar")
	defer os.Remove(tar)
	if err := exec.Command("docker", "pull", "ubuntu:24.04").Run(); err != nil {
		return fmt.Errorf("docker pull ubuntu:24.04: %w", err)
	}
	idb, err := exec.Command("docker", "create", "ubuntu:24.04").Output()
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(idb))
	err = exec.Command("docker", "export", "-o", tar, id).Run()
	_ = exec.Command("docker", "rm", id).Run()
	if err != nil {
		return fmt.Errorf("docker export: %w", err)
	}
	if out, err := wsl("--import", Distro, filepath.Join(dir(), "distro"), tar, "--version", "2"); err != nil {
		return fmt.Errorf("wsl --import: %s", strings.TrimSpace(wslText(out)))
	}
	return nil
}

// adopt renames Dovre's distro to berth's: WSL has no rename, but a distro's name is one value
// under this user's Lxss key. It has to be stopped first, and stay stopped while the name changes.
func adopt() error {
	out, err := reg("query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Lxss`, "/s", "/v", "DistributionName")
	if err != nil {
		return err
	}
	key, ok := LxssDistros(out)[OldDistro]
	if !ok {
		return fmt.Errorf("couldn't find %s under the Lxss key", OldDistro)
	}
	for i := 0; i < 10; i++ {
		_, _ = wsl("--terminate", OldDistro)
		time.Sleep(time.Second)
		if s := distroState(OldDistro); s != "" && s != "Running" {
			break
		}
		if i == 9 {
			return fmt.Errorf("%s keeps running: something holds it (Dovre before berth? Stop Docker on its Docker page, then run this again)", OldDistro)
		}
	}
	if _, err := reg("add", key, "/v", "DistributionName", "/t", "REG_SZ", "/d", Distro, "/f"); err != nil {
		return err
	}
	if !distroExists() {
		return errors.New("WSL doesn't see the new name yet: run `wsl --shutdown` and then `berth setup` again")
	}
	fmt.Printf("Adopted %s as %s.\n", OldDistro, Distro)
	logf("adopted %s as %s", OldDistro, Distro)
	return nil
}

// ---- the docker CLI ----

func get(url string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "berth")
	res, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", url, res.Status)
	}
	return io.ReadAll(res.Body)
}

func release(repo string) ([]Asset, string, error) {
	b, err := get("https://api.github.com/repos/" + repo + "/releases/latest")
	if err != nil {
		return nil, "", err
	}
	var r struct {
		Tag    string  `json:"tag_name"`
		Assets []Asset `json:"assets"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, "", err
	}
	return r.Assets, r.Tag, nil
}

// fetchChecked downloads one release file and checks it against the release's sums (or, when they
// leave it out, GitHub's digest of it).
func fetchChecked(assets []Asset, a Asset, sumsName string) ([]byte, error) {
	sums := ""
	if s, ok := PickAsset(assets, sumsName, ""); ok && s.Name == sumsName {
		b, err := get(s.URL)
		if err != nil {
			return nil, err
		}
		sums = string(b)
	}
	want, ok := WantSum(sums, a)
	if !ok {
		return nil, fmt.Errorf("no sum for %s, in %s or from GitHub", a.Name, sumsName)
	}
	b, err := get(a.URL)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(b)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s doesn't match its sum", a.Name)
	}
	return b, nil
}

// installCLI puts the Windows docker CLI and wincred in berth's bin, and compose and buildx in
// ~/.docker/cli-plugins: what goes when Docker Desktop is uninstalled. With again, it fetches
// the newest of each even when one is there.
func installCLI(again bool) error {
	arch := ArchOf(runtime.GOARCH)
	plugins := filepath.Join(dockerDir(), "cli-plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		return err
	}
	var problems []string

	if dst := filepath.Join(binDir(), "docker.exe"); again || !exists(dst) {
		base := "https://download.docker.com/win/static/stable/" + arch.Static + "/"
		err := func() error {
			index, err := get(base)
			if err != nil {
				return err
			}
			name, err := LatestDockerZip(string(index))
			if err != nil {
				return err
			}
			fmt.Println("Fetching the docker CLI (" + name + ")...")
			z, err := get(base + name)
			if err != nil {
				return err
			}
			r, err := zip.NewReader(bytes.NewReader(z), int64(len(z)))
			if err != nil {
				return err
			}
			for _, f := range r.File {
				if f.Name == "docker/docker.exe" {
					rc, err := f.Open()
					if err != nil {
						return err
					}
					b, err := io.ReadAll(rc)
					rc.Close()
					if err != nil {
						return err
					}
					return replaceFile(dst, b)
				}
			}
			return errors.New(name + " has no docker/docker.exe")
		}()
		if err != nil {
			problems = append(problems, "docker CLI: "+err.Error())
		}
	}

	type piece struct{ repo, prefix, suffix, sums, dst, what string }
	pieces := []piece{
		{"docker/compose", "docker-compose-windows-" + arch.Compose + ".exe", ".exe", "", filepath.Join(plugins, "docker-compose.exe"), "compose"},
		{"docker/buildx", "buildx-v", ".windows-" + arch.Go + ".exe", "checksums.txt", filepath.Join(plugins, "docker-buildx.exe"), "buildx"},
		{"docker/docker-credential-helpers", "docker-credential-wincred-v", ".windows-" + arch.Go + ".exe", "checksums.txt", filepath.Join(binDir(), "docker-credential-wincred.exe"), "wincred"},
	}
	for _, p := range pieces {
		if !again && exists(p.dst) {
			continue
		}
		err := func() error {
			assets, tag, err := release(p.repo)
			if err != nil {
				return err
			}
			a, ok := PickAsset(assets, p.prefix, p.suffix)
			if !ok {
				return fmt.Errorf("no %s…%s in %s %s", p.prefix, p.suffix, p.repo, tag)
			}
			sums := p.sums
			if sums == "" {
				sums = a.Name + ".sha256" // compose: one sum file per binary
			}
			fmt.Printf("Fetching %s %s...\n", p.what, tag)
			b, err := fetchChecked(assets, a, sums)
			if err != nil {
				return err
			}
			return replaceFile(p.dst, b)
		}()
		if err != nil {
			problems = append(problems, p.what+": "+err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// ---- context and config ----

func writeContext() error {
	d := filepath.Join(dockerDir(), "contexts", "meta", ContextDir(Context))
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, "meta.json"), ContextMeta(OwnPipe), 0o644)
}

func configFile() string { return filepath.Join(dockerDir(), "config.json") }

// use makes the docker CLI use berth: its context, and wincred for logins.
func use() error {
	if err := writeContext(); err != nil {
		return err
	}
	in, err := os.ReadFile(configFile())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_, werr := exec.LookPath("docker-credential-wincred.exe")
	wincred := werr == nil || exists(filepath.Join(binDir(), "docker-credential-wincred.exe"))
	out, changed, err := UseConfig(in, wincred)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		fmt.Println("The docker CLI uses berth already.")
		return nil
	}
	if len(in) > 0 {
		_ = os.WriteFile(configFile()+".before-berth", in, 0o644)
	}
	if err := os.WriteFile(configFile(), out, 0o644); err != nil {
		return err
	}
	for _, c := range changed {
		fmt.Println("docker CLI: " + c)
	}
	return nil
}

// ---- login ----

func installLogin() error {
	cmd := RunCommand(binDir(), exists(filepath.Join(binDir(), "berthw.exe")))
	if _, err := reg("add", runKey, "/v", runValue, "/t", "REG_SZ", "/d", cmd, "/f"); err != nil {
		return err
	}
	fmt.Println("berth starts at login (" + runKey + `\` + runValue + ").")
	return nil
}

func uninstallLogin() error {
	if !atLogin() {
		return nil
	}
	_, err := reg("delete", runKey, "/v", runValue, "/f")
	return err
}

func atLogin() bool { _, err := reg("query", runKey, "/v", runValue); return err == nil }

// ---- start, stop, status ----

func serverExe() string {
	for _, d := range []string{binDir(), exeDir()} {
		if w := filepath.Join(d, "berthw.exe"); exists(w) {
			return w
		}
	}
	if b := filepath.Join(binDir(), "berth.exe"); exists(b) {
		return b
	}
	self, _ := os.Executable()
	return self
}

func exeDir() string { self, _ := os.Executable(); return filepath.Dir(self) }

func start() error {
	if !distroExists() {
		return errors.New("no " + Distro + " distro: run `berth setup` first")
	}
	_ = os.Remove(offFile())
	if !Serving() {
		if err := Detached(serverExe(), "serve"); err != nil {
			return err
		}
	}
	fmt.Print("Starting Docker Engine")
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if v, err := EngineVersion(OwnPipe, 3*time.Second); err == nil {
			fmt.Printf(": Docker Engine %s on %s\n", v, PipePath(OwnPipe))
			return nil
		}
		fmt.Print(".")
		time.Sleep(2 * time.Second)
	}
	fmt.Println()
	return errors.New("the engine didn't answer in 90 seconds: see " + logFile())
}

func stop() error {
	if err := os.MkdirAll(dir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(offFile(), nil, 0o644); err != nil {
		return err
	}
	// The server lets go of the pipes and the distro within two seconds.
	for i := 0; i < 10 && Serving(); i++ {
		if s, ok := readState(); ok && !s.On {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if distroState(Distro) == "Running" {
		// Containers get dockerd's own stop (SIGTERM, then 10 s) before the distro goes.
		cmd := exec.Command("wsl.exe", "-d", Distro, "-u", "root", "--exec", "systemctl", "stop", "docker.socket", "docker.service")
		hide(cmd)
		done := make(chan struct{})
		go func() { _ = cmd.Run(); close(done) }()
		select {
		case <-done:
		case <-time.After(45 * time.Second):
			_ = cmd.Process.Kill()
		}
		_, _ = wsl("--terminate", Distro)
	}
	logf("stopped")
	fmt.Println("Docker Engine is stopped; berth serves no pipe until `berth start`.")
	return nil
}

func currentContextIsBerth() bool {
	b, err := os.ReadFile(configFile())
	return err == nil && jsonString(string(b), "currentContext") == Context
}

func getStatus() Status {
	st := Status{Distro: distroState(Distro), On: !exists(offFile()), Serving: Serving(), DesktopRunning: DesktopRunning(), AtLogin: atLogin(), Context: currentContextIsBerth()}
	if s, ok := readState(); ok && st.Serving {
		st.Pipes = s.Pipes
	}
	if st.Pipes == nil {
		st.Pipes = []string{}
	}
	if slices.Contains(st.Pipes, PipePath(DefaultPipe)) {
		st.DefaultHeldBy = "berth"
	} else if pipeServed(DefaultPipe) {
		st.DefaultHeldBy = "other"
	}
	if st.Serving && st.On && slices.Contains(st.Pipes, PipePath(OwnPipe)) {
		st.Engine, _ = EngineVersion(OwnPipe, 3*time.Second)
	}
	return st
}

func status(o opts) error {
	st := getStatus()
	if o.json {
		b, _ := json.Marshal(st)
		fmt.Println(string(b))
		return nil
	}
	yes := func(b bool, y, n string) string {
		if b {
			return y
		}
		return n
	}
	distro := st.Distro
	if distro == "" {
		distro = "none (`berth setup`)"
	}
	engine := st.Engine
	switch {
	case !st.On:
		engine = "stopped (`berth start`)"
	case !st.Serving:
		engine = "not served (`berth start`)"
	case engine == "":
		engine = "not answering (see " + logFile() + ")"
	}
	pipes := strings.Join(st.Pipes, ", ")
	if pipes == "" {
		pipes = "none"
	}
	def := map[string]string{"berth": "berth", "other": "another engine", "": "nobody"}[st.DefaultHeldBy]
	row := func(k, v string) { fmt.Printf("%-14s %s\n", k, v) }
	row("distro", Distro+" "+distro)
	row("engine", engine)
	row("pipes", pipes)
	row(DefaultPipe, def)
	row("docker desktop", yes(st.DesktopRunning, "running", "not running"))
	row("at login", yes(st.AtLogin, "yes", "no (`berth install`)"))
	row("docker cli", yes(st.Context, "context berth", "another context (`berth use`)"))
	return nil
}

// ---- remove ----

func remove(o opts) error {
	if !ask(fmt.Sprintf("Remove berth and the %s distro, with its images, containers and volumes?", Distro), o.yes) {
		return errors.New("nothing removed")
	}
	if s, ok := readState(); ok && Serving() && s.PID > 0 {
		if p, err := os.FindProcess(s.PID); err == nil {
			_ = p.Kill()
		}
	}
	if distroExists() {
		if out, err := wsl("--unregister", Distro); err != nil {
			return fmt.Errorf("wsl --unregister: %s", strings.TrimSpace(wslText(out)))
		}
		fmt.Println("Removed the " + Distro + " distro and everything in it.")
	}
	_ = uninstallLogin()
	_ = os.RemoveAll(filepath.Join(dockerDir(), "contexts", "meta", ContextDir(Context)))
	if b, err := os.ReadFile(configFile()); err == nil && jsonString(string(b), "currentContext") == Context {
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) == nil {
			delete(cfg, "currentContext")
			if out, err := json.MarshalIndent(cfg, "", "\t"); err == nil {
				_ = os.WriteFile(configFile(), out, 0o644)
			}
		}
	}
	removeUserPath(binDir())
	for _, f := range []string{offFile(), stateFile()} {
		_ = os.Remove(f)
	}
	fmt.Println("berth is removed. Its folder, " + dir() + ", holds the docker CLI and the log: delete it when nothing runs from it.")
	fmt.Println("compose and buildx stay in " + filepath.Join(dockerDir(), "cli-plugins") + " (they work with any engine).")
	return nil
}

// ---- update ----

func update() error {
	if !distroExists() {
		return errors.New("no " + Distro + " distro: run `berth setup` first")
	}
	fmt.Println("Upgrading Docker Engine in " + Distro + "...")
	if err := inDistro(UpdateScript); err != nil {
		return err
	}
	if err := inDistro(WaitScript); err != nil {
		return err
	}
	return installCLI(true)
}

func showLog() error {
	b, err := os.ReadFile(logFile())
	if err != nil {
		return errors.New("no log yet")
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	fmt.Println(strings.Join(lines, "\n"))
	return nil
}
