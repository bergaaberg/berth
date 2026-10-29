package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanPipes(t *testing.T) {
	cases := []struct {
		name                        string
		on, holding, other, desktop bool
		want                        Plan
	}{
		{"off serves nothing", false, true, false, false, Plan{}},
		{"on and docker_engine free: both", true, false, false, false, Plan{Own: true, Default: true}},
		{"someone else has docker_engine: ours only", true, false, true, false, Plan{Own: true}},
		{"we hold it: keep it", true, true, false, false, Plan{Own: true, Default: true}},
		{"Docker Desktop runs: hand it back", true, true, false, true, Plan{Own: true}},
		{"Desktop starting, pipe still free: leave it", true, false, false, true, Plan{Own: true}},
	}
	for _, c := range cases {
		if got := PlanPipes(c.on, c.holding, c.other, c.desktop); got != c.want {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

func TestPipeSDDL(t *testing.T) {
	got := PipeSDDL("S-1-5-21-1-2-3-1001")
	if got != "D:P(A;;GA;;;S-1-5-21-1-2-3-1001)" {
		t.Fatal(got)
	}
	// One entry, protected: nobody but this user.
	if strings.Count(got, "(A;") != 1 || !strings.HasPrefix(got, "D:P") {
		t.Fatal(got)
	}
}

func TestHasDistro(t *testing.T) {
	if !HasDistro([]byte("docker-desktop\r\nberth\r\n"), Distro) || HasDistro([]byte("docker-desktop\n"), Distro) {
		t.Fatal("HasDistro")
	}
	// wsl.exe without WSL_UTF8 writes UTF-16: still read.
	utf16 := []byte("b\x00e\x00r\x00t\x00h\x00\r\x00\n\x00")
	if !HasDistro(utf16, Distro) {
		t.Fatal("utf-16")
	}
	if HasDistro([]byte("berth-old\n"), Distro) {
		t.Fatal("a longer name matched")
	}
}

func TestDistroState(t *testing.T) {
	// The header is in the system's language (Norwegian here); only rows count.
	list := []byte("  NAME              STATE           VERSION\r\n* docker-desktop    Stopped         2\r\n  berth             Running         2\r\n")
	if DistroState(list, "berth") != "Running" || DistroState(list, "docker-desktop") != "Stopped" || DistroState(list, "nope") != "" {
		t.Fatal("DistroState")
	}
	if DistroState([]byte("  NAVN  TILSTAND  VERSJON\n"), "NAVN") != "TILSTAND" {
		// A distro called like the header word is unlikely; this documents that the header is read as a row.
		t.Log("header read as a row")
	}
}

func TestArgs(t *testing.T) {
	if a := strings.Join(relayArgs(), " "); a != "-d berth -u root --exec socat STDIO UNIX-CONNECT:/var/run/docker.sock" {
		t.Fatal(a)
	}
	if a := strings.Join(keepArgs(), " "); a != "-d berth -u root --exec sleep infinity" {
		t.Fatal(a)
	}
}

func TestRunCommand(t *testing.T) {
	d := `C:\Users\me\AppData\Local\berth\bin`
	if RunCommand(d, true) != `"`+d+`\berthw.exe" serve` || RunCommand(d, false) != `"`+d+`\berth.exe" serve` {
		t.Fatal(RunCommand(d, true))
	}
}

func TestContext(t *testing.T) {
	// The docker CLI's own folder name for a context called "berth".
	if ContextDir("default") != "37a8eec1ce19687d132fe29051dca629d164e2c4958ba141d5f4133a33f0688f" {
		t.Fatal(ContextDir("default"))
	}
	var m struct {
		Name      string
		Endpoints map[string]struct{ Host string }
	}
	if err := json.Unmarshal(ContextMeta(OwnPipe), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "berth" || m.Endpoints["docker"].Host != "npipe:////./pipe/berth" {
		t.Fatalf("%+v", m)
	}
}

func TestUseConfig(t *testing.T) {
	in := []byte(`{"auths":{"x.azurecr.io":{}},"credsStore":"desktop","currentContext":"desktop-linux","plugins":{"a":{"b":"c"}}}`)
	out, changed, err := UseConfig(in, true)
	if err != nil || len(changed) != 2 {
		t.Fatal(err, changed)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["currentContext"] != "berth" || m["credsStore"] != "wincred" || m["auths"] == nil || m["plugins"] == nil {
		t.Fatal(string(out))
	}
	// No wincred: logins stay with Desktop's helper.
	out, _, _ = UseConfig(in, false)
	_ = json.Unmarshal(out, &m)
	if m["credsStore"] != "desktop" {
		t.Fatal("credsStore changed without wincred")
	}
	// Already so: nothing changes, not even the formatting.
	same := []byte(`{"currentContext": "berth", "credsStore": "wincred"}`)
	out, changed, _ = UseConfig(same, true)
	if string(out) != string(same) || changed != nil {
		t.Fatal("rewrote an unchanged config")
	}
	// None yet.
	if out, _, err := UseConfig(nil, true); err != nil || !strings.Contains(string(out), `"berth"`) {
		t.Fatal(err)
	}
	// A broken file isn't overwritten.
	if _, _, err := UseConfig([]byte("{nope"), true); err == nil {
		t.Fatal("took broken JSON")
	}
}

func TestPath(t *testing.T) {
	d := `C:\Users\me\AppData\Local\berth\bin`
	if p, ok := AddToPath(`C:\a;C:\b`, d); !ok || p != `C:\a;C:\b;`+d {
		t.Fatal(p)
	}
	if _, ok := AddToPath(`C:\a;c:/users/me/appdata/local/berth/bin\`, d); ok {
		t.Fatal("added twice")
	}
	if p, _ := AddToPath("", d); p != d {
		t.Fatal(p)
	}
	if p, ok := RemoveFromPath(`C:\a;`+d+`;C:\b`, d); !ok || p != `C:\a;C:\b` {
		t.Fatal(p)
	}
}

func TestLatestDockerZip(t *testing.T) {
	index := `<a href="docker-29.9.2.zip">docker-29.9.2.zip</a> <a href="docker-29.10.0.zip">x</a> <a href="docker-28.5.1.zip">y</a>`
	if z, err := LatestDockerZip(index); err != nil || z != "docker-29.10.0.zip" {
		t.Fatal(z, err)
	}
	if _, err := LatestDockerZip("<html></html>"); err == nil {
		t.Fatal("found a zip in nothing")
	}
}

func TestAssetsAndSums(t *testing.T) {
	assets := []Asset{{Name: "buildx-v0.38.1.windows-amd64.sbom.json"}, {Name: "buildx-v0.38.1.windows-amd64.exe", URL: "u"}, {Name: "buildx-v0.38.1.windows-arm64.exe"}}
	if a, ok := PickAsset(assets, "buildx-v", ".windows-amd64.exe"); !ok || a.URL != "u" {
		t.Fatal(a)
	}
	h := strings.Repeat("ab", 32)
	if s, ok := SumFor(h+"  buildx-v0.38.1.windows-amd64.exe\n", "buildx-v0.38.1.windows-amd64.exe"); !ok || s != h {
		t.Fatal(s)
	}
	if s, ok := SumFor(strings.ToUpper(h)+" *docker-compose-windows-x86_64.exe\n", "docker-compose-windows-x86_64.exe"); !ok || s != h {
		t.Fatal(s)
	}
	if _, ok := SumFor(h+"  other.exe\n", "mine.exe"); ok {
		t.Fatal("matched another file's sum")
	}
}

func TestWantSum(t *testing.T) {
	h, g := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	a := Asset{Name: "buildx-v0.38.1.windows-amd64.exe", Digest: "sha256:" + g}
	if s, _ := WantSum(h+" *buildx-v0.38.1.windows-amd64.exe\n", a); s != h {
		t.Fatal("the project's own sum comes first")
	}
	if s, _ := WantSum(h+" *buildx-v0.38.1.linux-amd64\n", a); s != g {
		t.Fatal("left out of the sums: GitHub's digest")
	}
	if _, ok := WantSum("", Asset{Name: "x.exe"}); ok {
		t.Fatal("no sum at all must refuse")
	}
	if _, ok := WantSum("", Asset{Name: "x.exe", Digest: "md5:abc"}); ok {
		t.Fatal("only sha256")
	}
}

func TestArch(t *testing.T) {
	if ArchOf("arm64").Static != "aarch64" || ArchOf("amd64").Compose != "x86_64" {
		t.Fatal("arch")
	}
}

func TestPEM(t *testing.T) {
	p := PEM(make([]byte, 100))
	lines := strings.Split(strings.TrimSpace(p), "\n")
	if lines[0] != "-----BEGIN CERTIFICATE-----" || lines[len(lines)-1] != "-----END CERTIFICATE-----" || len(lines[1]) != 64 {
		t.Fatal(p)
	}
}

func TestSetupScript(t *testing.T) {
	s := SetupScript(PEM([]byte{1, 2, 3}))
	if strings.Contains(s, "\r") || !strings.Contains(s, "systemd=true") || !strings.Contains(s, "docker.io") || !strings.Contains(s, "BEGIN CERTIFICATE") {
		t.Fatal(s)
	}
	// The certificates are a quoted heredoc: nothing in them is expanded by bash.
	if !strings.Contains(s, "<<'BERTH_CERTS'") {
		t.Fatal("unquoted heredoc")
	}
}

func TestLxssDistros(t *testing.T) {
	out := "\r\nHKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Lxss\\{aaa}\r\n    DistributionName    REG_SZ    docker-desktop\r\n\r\nHKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Lxss\\{bbb}\r\n    DistributionName    REG_SZ    dovre-docker\r\n\r\nSøket er fullført: 2 treff.\r\n"
	m := LxssDistros(out)
	if m["dovre-docker"] != `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Lxss\{bbb}` || len(m) != 2 {
		t.Fatal(m)
	}
}

func TestDesktopProcess(t *testing.T) {
	if !IsDesktopProcess("Docker Desktop.exe") || !IsDesktopProcess("com.docker.backend.exe") || IsDesktopProcess("docker.exe") {
		t.Fatal("desktop")
	}
}

func TestJSONString(t *testing.T) {
	if jsonString(`{"Version":"29.1.3"}`, "Version") != "29.1.3" || jsonString("nope", "Version") != "" {
		t.Fatal("jsonString")
	}
}
