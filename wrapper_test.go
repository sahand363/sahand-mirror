package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// rewrite runs the real bash rewrite function on a URL.
func rewrite(t *testing.T, env, u string) string {
	t.Helper()
	script := "MIRROR_URL=http://10.0.0.5:8686\n__SM_EXT=$'https://get.acme.sh\\nhttps://get.acme.sh/'\n" + env + "\n" + wrapperFuncs +
		"\n__sm_rw \"$1\"; echo \"$?|$REPLY\""
	out, err := exec.Command("bash", "-c", script, "x", u).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRewrite(t *testing.T) {
	h := "http://10.0.0.5:8686"
	cases := map[string]string{
		"https://github.com/MHSanaei/3x-ui/releases/latest":                   "0|" + h + "/gh/web/MHSanaei/3x-ui/releases/latest",
		"http://github.com/o/r/releases/download/v1/a.tar.gz":                 "0|" + h + "/gh/web/o/r/releases/download/v1/a.tar.gz",
		"https://api.github.com/repos/MHSanaei/3x-ui/releases/latest":         "0|" + h + "/gh/api/repos/MHSanaei/3x-ui/releases/latest",
		"https://raw.githubusercontent.com/MHSanaei/3x-ui/v2.5.0/x-ui.sh":     "0|" + h + "/gh/raw/MHSanaei/3x-ui/v2.5.0/x-ui.sh",
		"https://objects.githubusercontent.com/github-production-release/x?y": "0|" + h + "/gh/objects/github-production-release/x?y",
		"https://codeload.github.com/o/r/tar.gz/refs/heads/main":              "0|" + h + "/gh/codeload/o/r/tar.gz/refs/heads/main",
		"https://get.acme.sh":                 "0|" + h + "/ext?u=https%3A%2F%2Fget.acme.sh",
		"https://api4.ipify.org":              "1|https://api4.ipify.org",
		"-o":                                  "1|-o",
		"/tmp/x-ui-linux-amd64.tar.gz":        "1|/tmp/x-ui-linux-amd64.tar.gz",
		"http://10.0.0.5:8686/i/app/recipe":   "0|http://10.0.0.5:8686/i/app/recipe",
		"https://githubusercontent.com.evil/": "1|https://githubusercontent.com.evil/",
	}
	for in, want := range cases {
		if got := rewrite(t, "", in); got != want {
			t.Errorf("%s\n got  %s\n want %s", in, got, want)
		}
	}
	// scripts running with `set -u` (e.g. loadinme) must not break on unset helper vars
	if got := rewrite(t, "set -u; unset __SM_PIN_TAG __SM_TRACE __SM_KEY", "https://github.com/o/r/releases/latest"); got != "0|"+h+"/gh/web/o/r/releases/latest" {
		t.Errorf("set -u: %s", got)
	}
	pin := "__SM_PIN_REPO=mhsanaei/3x-ui\n__SM_PIN_TAG=v2.4.0"
	if got := rewrite(t, pin, "https://github.com/MHSanaei/3x-ui/releases/latest"); got != "0|"+h+"/gh/web/mhsanaei/3x-ui/releases/tag/v2.4.0" {
		t.Errorf("pin web: %s", got)
	}
	if got := rewrite(t, pin, "https://api.github.com/repos/MHSanaei/3x-ui/releases/latest"); got != "0|"+h+"/gh/api/repos/mhsanaei/3x-ui/releases/tags/v2.4.0" {
		t.Errorf("pin api: %s", got)
	}
}

func TestWrapperScript(t *testing.T) {
	a := &App{ID: "3x-ui", Command: "bash <(curl -Ls https://raw.githubusercontent.com/mhsanaei/3x-ui/master/install.sh)"}
	s := buildWrapper(wrapperOpts{Hub: "http://10.0.0.5:8686/", App: a, Env: map[string]string{"XUI_PORT": "2053", "bad name": "x"},
		Args: []string{"v2.4.0"}, Externs: []string{"https://get.acme.sh"}})
	for _, want := range []string{"export MIRROR_URL='http://10.0.0.5:8686'", "export -f __sm_enc __sm_ip __sm_rw __sm_real __sm_run __sm_apt_prep __sm_apt curl wget",
		"export XUI_PORT='2053'", `eval 'bash <(curl -Ls https://raw.githubusercontent.com/mhsanaei/3x-ui/master/install.sh) '\''v2.4.0'\'''`} {
		if !strings.Contains(s, want) {
			t.Errorf("wrapper missing %q", want)
		}
	}
	if strings.Contains(s, "bad name") {
		t.Error("invalid env name exported")
	}
	if out, err := exec.Command("bash", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("wrapper syntax: %v %s", err, out)
	}
	a.Recipe = "#!/bin/bash\necho edited"
	s = buildWrapper(wrapperOpts{Hub: "http://h:1", App: a})
	if !strings.Contains(s, "bash <(curl -Ls http://h:1/i/3x-ui/recipe)") {
		t.Errorf("recipe command not used:\n%s", s)
	}
}

// runWrapped builds the wrapper, replaces its final eval with body and runs it.
func runWrapped(t *testing.T, o wrapperOpts, prelude, body string) string {
	t.Helper()
	w := buildWrapper(o)
	w = w[:strings.LastIndex(w, "\neval ")+1]
	out, err := exec.Command("bash", "-c", prelude+"\n"+w+"\n"+body).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	return string(out)
}

func TestWrapperAptAndIP(t *testing.T) {
	bin := t.TempDir()
	// fake apt-get/apt print their arguments; "update" fails like an unreachable repo
	fake := "#!/bin/sh\necho \"REAL $(basename $0) $*\"\ncase \" $* \" in *\" update \"*) exit 100;; esac\nexit 0\n"
	for _, n := range []string{"apt-get", "apt"} {
		if err := os.WriteFile(bin+"/"+n, []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a := &App{ID: "3x-ui", Command: "bash <(curl -Ls https://raw.githubusercontent.com/mhsanaei/3x-ui/master/install.sh)"}
	o := wrapperOpts{Hub: "http://10.0.0.5:8686", App: a, AptProxy: aptProxyURL("http://10.0.0.5:8686", ""), AptHTTPS: "keep"}
	out := runWrapped(t, o, "export PATH="+bin+":$PATH", `
apt-get install -y socat; echo "rc=$?"
apt-get update; echo "update-rc=$?"
bash -c 'apt -y install cron'
sh -c 'apt-get -o Debug::X=1 install curl'
type -t apt-get
__sm_rw https://api4.ipify.org && echo "IP1 $REPLY"
__sm_rw ifconfig.me && echo "IP2 $REPLY"
__sm_rw 'https://api.ipify.org?format=json' && echo "IP3 $REPLY"
__sm_rw https://ipinfo.io/json || echo "IP4 untouched"
sh -c 'curl --version >/dev/null && echo shim-ok'
`)
	for _, want := range []string{
		"REAL apt-get -o Acquire::http::Proxy=http://10.0.0.5:8686/ -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=15 -o Acquire::Retries=1 install -y socat",
		"rc=0", "update-rc=0", "REAL apt -o Acquire::http::Proxy=http://10.0.0.5:8686/", "install cron",
		"REAL apt-get -o Acquire::http::Proxy=http://10.0.0.5:8686/ -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=15 -o Acquire::Retries=1 -o Debug::X=1 install curl",
		"function", "IP1 http://10.0.0.5:8686/myip", "IP2 http://10.0.0.5:8686/myip", "IP3 http://10.0.0.5:8686/myip?format=json", "IP4 untouched",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// without the apt proxy apt-get is left alone
	o.AptProxy = ""
	out = runWrapped(t, o, "export PATH="+bin+":$PATH", "type -t apt-get; apt-get install x")
	if strings.Contains(out, "function") || !strings.Contains(out, "REAL apt-get install x") {
		t.Errorf("apt untouched expected:\n%s", out)
	}
}

func TestWrapperAptSkipsUnreachableHTTPS(t *testing.T) {
	etc := t.TempDir()
	os.MkdirAll(etc+"/sources.list.d", 0o755)
	os.WriteFile(etc+"/sources.list", []byte("deb http://de.archive.ubuntu.com/ubuntu noble main\n"), 0o644)
	os.WriteFile(etc+"/sources.list.d/cloudsmith.list", []byte("deb [signed-by=/k] https://dl.cloudsmith.invalid/public/x/deb/ubuntu noble main\ndeb http://ok.example/ubuntu noble main\n"), 0o644)
	os.WriteFile(etc+"/sources.list.d/docker.sources", []byte("Types: deb\nURIs: https://download.docker.invalid/linux/ubuntu\nSuites: noble\n\nTypes: deb\nURIs: http://keep.example/ubuntu\nSuites: noble\n"), 0o644)
	bin := t.TempDir()
	os.WriteFile(bin+"/apt-get", []byte("#!/bin/sh\necho \"REAL $*\"\n"), 0o755)
	a := &App{ID: "x", Command: "true"}
	o := wrapperOpts{Hub: "http://10.0.0.5:8686", App: a, AptProxy: "http://10.0.0.5:8686/"}
	out := runWrapped(t, o, "export PATH="+bin+":$PATH __SM_APT_ETC="+etc, `apt-get update; d="$__SM_BIN/.apt"; echo ---; cat "$d/sources.list" "$d/sources.list.d/"*`)
	for _, want := range []string{"unreachable https repos skipped", "Dir::Etc::SourceList=", "deb http://de.archive.ubuntu.com/ubuntu noble main",
		"deb http://ok.example/ubuntu noble main", "URIs: http://keep.example/ubuntu"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	after := out[strings.Index(out, "---"):]
	if strings.Contains(after, "cloudsmith.invalid") || strings.Contains(after, "docker.invalid") {
		t.Errorf("https repos not filtered:\n%s", after)
	}
}
