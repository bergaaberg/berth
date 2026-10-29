package main

// The plain logic: names, command lines, file formats and decisions. No Windows calls here, so
// all of it is tested on any OS (logic_test.go). The Windows side is in win.go, serve.go and cmd.go.

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Distro is the WSL distro berth makes (or adopts, renamed from Dovre's dovre-docker).
// A var only so a test build can point at another distro (-X main.Distro=...).
var Distro = "berth"

// OldDistro: the distro Dovre made before berth existed. `berth setup` offers to adopt it.
const OldDistro = "dovre-docker"

// The pipes. berth always serves its own; it also serves Docker's default pipe whenever nothing
// else does, so the docker CLI, Testcontainers and Rider find it without settings.
const (
	OwnPipe     = "berth"
	DefaultPipe = "docker_engine"
	Context     = "berth"
)

func PipePath(name string) string { return `\\.\pipe\` + name }

// PipeHost: the docker CLI's form of a pipe's address.
func PipeHost(name string) string { return "npipe:////./pipe/" + name }

// PipeSDDL: full access for this user's SID and nobody else, not even administrators or SYSTEM
// (protected, so nothing is inherited).
func PipeSDDL(sid string) string { return "D:P(A;;GA;;;" + sid + ")" }

// wslText: wsl.exe writes UTF-16 unless WSL_UTF8=1; read either.
func wslText(b []byte) string { return strings.ReplaceAll(string(b), "\x00", "") }

// HasDistro reads `wsl --list --quiet`.
func HasDistro(list []byte, name string) bool {
	for _, line := range strings.Split(wslText(list), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), name) {
			return true
		}
	}
	return false
}

// DistroState reads `wsl --list --verbose` for one distro: "Running", "Stopped", … or "" when
// it isn't there. The header and its words are in the system's language, so only the rows count:
// an optional "*" (the default distro), the name, the state, the version.
func DistroState(list []byte, name string) string {
	for _, line := range strings.Split(wslText(list), "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if len(f) >= 3 && strings.EqualFold(f[0], name) {
			return f[1]
		}
	}
	return ""
}

// relayArgs: what wsl.exe runs for one connection. --exec: no shell in between.
func relayArgs() []string {
	return []string{"-d", Distro, "-u", "root", "--exec", "socat", "STDIO", "UNIX-CONNECT:/var/run/docker.sock"}
}

// keepArgs: the process that keeps the distro (and so the engine) running. WSL stops a distro a
// few seconds after the last Windows process attached to it ends, and its containers go with it.
func keepArgs() []string {
	return []string{"-d", Distro, "-u", "root", "--exec", "sleep", "infinity"}
}

// Plan: which pipes to serve now.
//   - Off (`berth stop`): none, so clients hear "not running" and nothing starts the distro again.
//   - On: always our own. docker_engine too when we hold it already, or nobody serves it; but
//     not while Docker Desktop runs: it wants that pipe back, and gets it.
type Plan struct{ Own, Default bool }

func PlanPipes(on, holdingDefault, defaultServedByOther, desktopRunning bool) Plan {
	if !on {
		return Plan{}
	}
	return Plan{Own: true, Default: !desktopRunning && (holdingDefault || !defaultServedByOther)}
}

// IsDesktopProcess: a process that means Docker Desktop is running (or starting).
func IsDesktopProcess(exe string) bool {
	switch strings.ToLower(exe) {
	case "docker desktop.exe", "com.docker.backend.exe", "com.docker.build.exe":
		return true
	}
	return false
}

// RunCommand: the Run key's command line, the console-less build when there is one.
func RunCommand(dir string, haveGUI bool) string {
	exe := dir + `\berth.exe`
	if haveGUI {
		exe = dir + `\berthw.exe`
	}
	return `"` + exe + `" serve`
}

// ContextMeta: a docker context, as the CLI keeps it in ~/.docker/contexts/meta/<ContextDir>/meta.json.
func ContextMeta(pipe string) []byte {
	m := map[string]any{
		"Name":      Context,
		"Metadata":  map[string]any{"Description": "berth: Docker Engine in WSL (" + Distro + ")"},
		"Endpoints": map[string]any{"docker": map[string]any{"Host": PipeHost(pipe), "SkipTLSVerify": false}},
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return b
}

// ContextDir: the docker CLI names a context's folder by the sha256 of its name.
func ContextDir(name string) string {
	s := sha256.Sum256([]byte(name))
	return hex.EncodeToString(s[:])
}

// UseConfig makes berth the docker CLI's engine in ~/.docker/config.json: current context berth,
// and Docker Desktop's credential helper swapped for wincred (both keep logins in Windows'
// Credential Manager, so logins stay). Everything else is left as it was. It says what changed.
func UseConfig(in []byte, haveWincred bool) ([]byte, []string, error) {
	cfg := map[string]any{}
	if len(strings.TrimSpace(string(in))) > 0 {
		if err := json.Unmarshal(in, &cfg); err != nil {
			return nil, nil, fmt.Errorf("~/.docker/config.json isn't JSON: %w", err)
		}
	}
	var changed []string
	if cfg["currentContext"] != Context {
		cfg["currentContext"] = Context
		changed = append(changed, "current context: "+Context)
	}
	if cs, _ := cfg["credsStore"].(string); cs == "desktop" && haveWincred {
		cfg["credsStore"] = "wincred"
		changed = append(changed, "credentials: wincred instead of Docker Desktop's helper (same Credential Manager entries)")
	}
	if len(changed) == 0 {
		return in, nil, nil
	}
	out, err := json.MarshalIndent(cfg, "", "\t")
	return out, changed, err
}

// AddToPath: a PATH with dir added at the end, unless it's in it already (any case, either slash).
func AddToPath(path, dir string) (string, bool) {
	norm := func(s string) string { return strings.TrimRight(strings.ToLower(strings.ReplaceAll(s, "/", `\`)), `\`) }
	for _, p := range strings.Split(path, ";") {
		if norm(p) == norm(dir) {
			return path, false
		}
	}
	if strings.TrimSpace(path) == "" {
		return dir, true
	}
	return strings.TrimRight(path, ";") + ";" + dir, true
}

// RemoveFromPath: the PATH without dir.
func RemoveFromPath(path, dir string) (string, bool) {
	norm := func(s string) string { return strings.TrimRight(strings.ToLower(strings.ReplaceAll(s, "/", `\`)), `\`) }
	var keep []string
	found := false
	for _, p := range strings.Split(path, ";") {
		if norm(p) == norm(dir) {
			found = true
			continue
		}
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, ";"), found
}

var zipRe = regexp.MustCompile(`docker-(\d+)\.(\d+)\.(\d+)\.zip`)

// LatestDockerZip: the newest docker-X.Y.Z.zip in download.docker.com's index page.
func LatestDockerZip(index string) (string, error) {
	type v struct {
		name string
		n    [3]int
	}
	var all []v
	for _, m := range zipRe.FindAllStringSubmatch(index, -1) {
		var x v
		x.name = m[0]
		for i := range 3 {
			x.n[i], _ = strconv.Atoi(m[i+1])
		}
		all = append(all, x)
	}
	if len(all) == 0 {
		return "", errors.New("no docker zip in the index")
	}
	sort.Slice(all, func(i, j int) bool {
		for k := range 3 {
			if all[i].n[k] != all[j].n[k] {
				return all[i].n[k] < all[j].n[k]
			}
		}
		return false
	})
	return all[len(all)-1].name, nil
}

// Asset: one file of a GitHub release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// PickAsset: the release's file whose name starts with prefix and ends with suffix (".exe", say).
func PickAsset(assets []Asset, prefix, suffix string) (Asset, bool) {
	for _, a := range assets {
		if strings.HasPrefix(a.Name, prefix) && strings.HasSuffix(a.Name, suffix) {
			return a, true
		}
	}
	return Asset{}, false
}

// SumFor reads a checksums file (`<hex>  <name>`, or `<hex> *<name>`, or one bare hex) for name.
func SumFor(sums, name string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 1 && len(f[0]) == 64 {
			return strings.ToLower(f[0]), true
		}
		if len(f) == 2 && len(f[0]) == 64 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// Arch: the names each project uses for this machine's CPU.
type Arch struct{ Go, Static, Compose string }

func ArchOf(goarch string) Arch {
	if goarch == "arm64" {
		return Arch{Go: "arm64", Static: "aarch64", Compose: "aarch64"}
	}
	return Arch{Go: "amd64", Static: "x86_64", Compose: "x86_64"}
}

// PEM: one certificate in the form update-ca-certificates reads.
func PEM(der []byte) string {
	var b strings.Builder
	b.WriteString("-----BEGIN CERTIFICATE-----\n")
	s := base64.StdEncoding.EncodeToString(der)
	for len(s) > 64 {
		b.WriteString(s[:64] + "\n")
		s = s[64:]
	}
	b.WriteString(s + "\n-----END CERTIFICATE-----\n")
	return b.String()
}

// SetupScript: what runs in the distro as root (bash -euo pipefail, on stdin). It installs Docker
// Engine from Ubuntu's archive (plain http, so it works before any certificate is in), trusts what
// Windows trusts (certs: PEM text; behind a TLS-inspecting proxy Linux needs that proxy's root),
// and has systemd start dockerd with the distro.
func SetupScript(certs string) string {
	s := `export DEBIAN_FRONTEND=noninteractive
if ! command -v dockerd >/dev/null || ! command -v socat >/dev/null || ! command -v update-ca-certificates >/dev/null || ! docker compose version >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends ca-certificates systemd systemd-sysv dbus iptables docker.io docker-compose-v2 docker-buildx socat >/dev/null
fi
mkdir -p /usr/local/share/ca-certificates/berth
rm -f /usr/local/share/ca-certificates/berth/*.crt /usr/local/share/ca-certificates/zscaler*.crt
cat > /tmp/berth-certs.pem <<'BERTH_CERTS'
` + certs + `BERTH_CERTS
# One certificate per file, or update-ca-certificates skips it.
awk '/BEGIN CERTIFICATE/{n++} n{print > ("/usr/local/share/ca-certificates/berth/windows-" n ".crt")}' /tmp/berth-certs.pem
rm -f /tmp/berth-certs.pem
update-ca-certificates >/dev/null 2>&1 || true
# systemd starts dockerd with the distro; root is the only user (nobody logs in here).
cat > /etc/wsl.conf <<'EOF'
[boot]
systemd=true
[user]
default=root
[interop]
appendWindowsPath=false
EOF
systemctl enable docker.service docker.socket >/dev/null 2>&1 || true
`
	return strings.ReplaceAll(s, "\r", "")
}

// WaitScript: waits for the engine after a (re)start and says which it is.
const WaitScript = `for i in $(seq 1 60); do [ -S /var/run/docker.sock ] && docker info >/dev/null 2>&1 && break; sleep 1; done
docker version --format "Docker Engine {{.Server.Version}} in $(. /etc/os-release; echo $PRETTY_NAME)"
`

// UpdateScript: upgrades the engine's packages, then restarts it.
const UpdateScript = `export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --only-upgrade docker.io docker-compose-v2 docker-buildx socat ca-certificates >/dev/null
systemctl restart docker.service
`

// LxssDistros reads `reg query HKCU\...\Lxss /s /v DistributionName`: each distro's key by name.
func LxssDistros(out string) map[string]string {
	keys := map[string]string{}
	key := ""
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, `HKEY_`) {
			key = t
			continue
		}
		f := strings.Fields(t)
		if key != "" && len(f) >= 3 && f[0] == "DistributionName" && f[1] == "REG_SZ" {
			keys[strings.Join(f[2:], " ")] = key
		}
	}
	return keys
}

// State: what the serving process tells `berth status` and Dovre (state.json beside it).
type State struct {
	PID   int      `json:"pid"`
	On    bool     `json:"on"`
	Pipes []string `json:"pipes"`
	Since string   `json:"since"`
}

// Status: `berth status --json`, read by Dovre's Docker page.
type Status struct {
	Distro         string   `json:"distro"`         // Running, Stopped, or "" (none)
	On             bool     `json:"on"`             // not stopped with `berth stop`
	Serving        bool     `json:"serving"`        // the background process runs
	Pipes          []string `json:"pipes"`          // the pipes it serves now
	Engine         string   `json:"engine"`         // Docker Engine's version, when it answers
	DesktopRunning bool     `json:"desktopRunning"` // Docker Desktop is running
	DefaultHeldBy  string   `json:"defaultHeldBy"`  // docker_engine: "berth", "other" or ""
	AtLogin        bool     `json:"atLogin"`
	Context        bool     `json:"context"` // the docker CLI's current context is berth
}

// jsonString: one string field of a JSON object, or "".
func jsonString(body, field string) string {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return ""
	}
	s, _ := m[field].(string)
	return s
}
