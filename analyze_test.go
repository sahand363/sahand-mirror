package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func findings3xui(t *testing.T) []Finding {
	b, err := os.ReadFile("testdata/3x-ui-install.sh") // real MHSanaei/3x-ui master install.sh
	if err != nil {
		t.Fatal(err)
	}
	return analyzeScripts([]ScriptDoc{{URL: "https://raw.githubusercontent.com/MHSanaei/3x-ui/master/install.sh", Body: string(b)}})
}

func has(fs []Finding, kind, pm, name string) bool {
	for _, f := range fs {
		if f.Kind == kind && f.PM == pm && f.Name == name {
			return true
		}
	}
	return false
}

func TestAnalyze3xui(t *testing.T) {
	fs := findings3xui(t)
	for _, p := range []string{"cron", "curl", "tar", "tzdata", "socat", "ca-certificates", "openssl", "postgresql-client"} {
		if !has(fs, "pkg", "apt", p) {
			t.Errorf("apt package %s not found", p)
		}
	}
	for _, c := range [][2]string{{"dnf", "cronie"}, {"yum", "cronie"}, {"pacman", "cronie"}, {"zypper", "timezone"}, {"apk", "dcron"}} {
		if !has(fs, "pkg", c[0], c[1]) {
			t.Errorf("%s package %s not found", c[0], c[1])
		}
	}
	for _, h := range []string{"api4.ipify.org", "ipv4.icanhazip.com", "v4.api.ipinfo.io/ip", "ipv4.myexternalip.com/raw", "4.ident.me", "check-host.net/ip"} {
		if !has(fs, "ipdetect", "", h) {
			t.Errorf("ip detect %s not found", h)
		}
	}
	if !has(fs, "ssl", "", "acme.sh") {
		t.Error("acme.sh not flagged")
	}
	if !has(fs, "aptrepo", "", "apt.postgresql.org.sh") {
		t.Error("pgdg apt repo not flagged")
	}
	for _, f := range fs {
		if strings.Contains(f.Name, "://") && f.Kind == "pkg" {
			t.Errorf("URL taken as package: %+v", f)
		}
		if f.Kind == "pkg" && (f.Name == "-y" || f.Name == "-q" || f.Name == "install" || f.Name == "update") {
			t.Errorf("bad package token: %+v", f)
		}
	}
}

func TestPackageParsing(t *testing.T) {
	script := `#!/bin/bash
PKGS="jq   unzip"
EXTRA=(git wget)
sudo DEBIAN_FRONTEND=noninteractive apt-get -o Dpkg::Options::=--force-confold install -y --no-install-recommends \
    nginx=1.24* python3-pip:amd64 $PKGS "${EXTRA[@]}" ${UNKNOWN} >/dev/null 2>&1
apt update && apt -y install htop || true
apt-get remove -y apache2
dnf -y --setopt=install_weak_deps=False install vim-enhanced
apk add --no-cache bash
pacman -Syu --noconfirm base-devel
pacman -Ss foo
zypper -n in screen
pip3 install --upgrade requests==2.31 -r req.txt
npm install -g pm2
go install github.com/foo/bar@latest
docker run -d --name web -p 80:80 -v /x:/y nginx:1.25
docker pull ghcr.io/foo/bar:latest
echo "deb [signed-by=/k.gpg] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list
my_ip=$(curl -s ifconfig.me)
`
	fs := analyzeScripts([]ScriptDoc{{URL: "x/setup.sh", Body: script}})
	want := [][3]string{
		{"pkg", "apt", "jq"}, {"pkg", "apt", "unzip"}, {"pkg", "apt", "python3-pip"}, {"pkg", "apt", "htop"},
		{"pkg", "dnf", "vim-enhanced"}, {"pkg", "apk", "bash"}, {"pkg", "pacman", "base-devel"}, {"pkg", "zypper", "screen"},
		{"lang", "pip", "requests"}, {"lang", "npm", "pm2"}, {"lang", "go", "github.com/foo/bar@latest"},
		{"docker", "image", "nginx:1.25"}, {"docker", "image", "ghcr.io/foo/bar:latest"},
		{"aptrepo", "", "https://download.docker.com/linux/ubuntu"}, {"ipdetect", "", "ifconfig.me"},
		{"pkg", "apt", "git"}, {"pkg", "apt", "wget"},
	}
	for _, w := range want {
		if !has(fs, w[0], w[1], w[2]) {
			t.Errorf("missing %v", w)
		}
	}
	for _, bad := range []string{"apache2", "foo", "nginx=1.24*", "req.txt"} {
		for _, f := range fs {
			if f.Name == bad {
				t.Errorf("unexpected %+v", f)
			}
		}
	}
	dyn := false
	for _, f := range fs {
		if f.Dynamic && f.Name == "${UNKNOWN}" {
			dyn = true
		}
	}
	if !dyn {
		t.Error("dynamic package not reported")
	}
	if pk, ok := installArgs("apt", strings.Fields("-y -q install curl")); !ok || len(pk) != 1 {
		t.Errorf("installArgs: %v %v", pk, ok)
	}
}

func TestIPDetectMatch(t *testing.T) {
	yes := []string{"https://api4.ipify.org", "https://api.ipify.org/", "ifconfig.me", "http://ifconfig.me/ip", "https://ipinfo.io/ip",
		"https://v4.api.ipinfo.io/ip", "ip.sb", "checkip.amazonaws.com", "https://check-host.net/ip", "https://api.ipify.org?format=json"}
	no := []string{"https://ipinfo.io", "https://ipinfo.io/json", "https://check-host.net/check-http", "https://example.com/ip", "https://github.com"}
	for _, u := range yes {
		if !ipDetectMatch(u) {
			t.Errorf("%s should match", u)
		}
	}
	for _, u := range no {
		if ipDetectMatch(u) {
			t.Errorf("%s should not match", u)
		}
	}
}

func TestEvaluateReadiness(t *testing.T) {
	a := &App{ID: "3x-ui", Archs: []string{"amd64"}, Readiness: &Readiness{Findings: findings3xui(t)},
		Externals: []string{"https://get.acme.sh", "https://api4.ipify.org", "https://${domain}/x"}}
	st := newState()
	st.Ext["https://get.acme.sh"] = &Ext{URL: "https://get.acme.sh"}
	st.Ext["https://${domain}/x"] = &Ext{URL: "https://${domain}/x", Dynamic: true}
	cfg := defaultConfig()
	cfg.AptTargets = []string{"ubuntu/noble"}
	items, sum := evaluateReadiness(a, st, cfg)
	get := func(cat, name string) *ReadyItem {
		for i := range items {
			if items[i].Category == cat && items[i].Name == name {
				return &items[i]
			}
		}
		return nil
	}
	if it := get("pkg", "socat"); it == nil || it.Status != "action" || it.Fix != "prefetch" {
		t.Errorf("socat before prefetch: %+v", it)
	}
	if it := get("ipdetect", "api4.ipify.org"); it == nil || it.Status != "handled" {
		t.Errorf("ipify: %+v", it)
	}
	if it := get("ssl", "acme.sh"); it == nil || it.Fix != "cert" {
		t.Errorf("ssl: %+v", it)
	}
	if it := get("ext", "https://get.acme.sh"); it == nil || it.Fix != "mirror-ext" {
		t.Errorf("ext: %+v", it)
	}
	if it := get("ext", "https://api4.ipify.org"); it != nil {
		t.Error("ip detect URL listed as external")
	}
	if it := get("ext", "https://${domain}/x"); it == nil || it.Status != "offline" {
		t.Errorf("dynamic ext: %+v", it)
	}
	// after a prefetch for ubuntu/noble/amd64 socat is handled
	st.Apt = &AptState{Targets: map[string]*AptTargetState{"ubuntu/noble/amd64": {Packages: map[string]string{"socat": "socat=1.8"}}}}
	items, sum2 := evaluateReadiness(a, st, cfg)
	if it := get("pkg", "socat"); it == nil || it.Status != "handled" {
		t.Errorf("socat after prefetch: %+v", it)
	}
	if sum2.Handled != sum.Handled+1 {
		t.Errorf("summary %+v -> %+v", sum, sum2)
	}
	if pk := aptPackagesOf(a); !contains(pk, "socat") || contains(pk, "cronie") {
		t.Errorf("aptPackagesOf: %v", pk)
	}
}

func TestAIOpenAICompatible(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		content := "```json\n{\"items\":[{\"category\":\"network\",\"name\":\"NTP\",\"detail\":\"timedatectl\",\"suggestion\":\"use internal NTP\",\"status\":\"action\"}]}\n```"
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	defer srv.Close()
	cfg := defaultConfig()
	cfg.AIProvider, cfg.AIBaseURL, cfg.AIModel, cfg.AIKey = "openai", srv.URL+"/v1", "test-model", "sk-test"
	items, model, err := aiReview(context.Background(), cfg, &App{ID: "x"}, []ScriptDoc{{URL: "a.sh", Body: "apt-get install -y curl"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "NTP" || model != "openai/test-model" || gotAuth != "Bearer sk-test" || gotModel != "test-model" {
		t.Errorf("items=%+v model=%s auth=%s", items, model, gotAuth)
	}
	// offline / not configured: clear errors
	cfg.AIBaseURL = "http://127.0.0.1:1/v1"
	if _, _, err := aiReview(context.Background(), cfg, &App{ID: "x"}, nil, nil); err == nil || !strings.Contains(err.Error(), "در دسترس نیست") {
		t.Errorf("offline error: %v", err)
	}
	cfg.AIKey = ""
	if _, _, err := aiReview(context.Background(), cfg, &App{ID: "x"}, nil, nil); err == nil {
		t.Error("expected not-configured error")
	}
}

func TestTruncateScripts(t *testing.T) {
	big := strings.Repeat("a", 100000)
	out := truncateScripts([]ScriptDoc{{URL: "big.sh", Body: big}, {URL: "small.sh", Body: "echo hi"}}, 1000, 5000)
	if len(out) > 5000 || !strings.Contains(out, "truncated") || !strings.Contains(out, "echo hi") {
		t.Errorf("truncate: %d bytes", len(out))
	}
}
