package main

import (
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
	for _, want := range []string{"export MIRROR_URL='http://10.0.0.5:8686'", "export -f __sm_enc __sm_rw __sm_real __sm_run curl wget",
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
