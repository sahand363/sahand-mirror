package main

import (
	"reflect"
	"testing"
	"time"
)

const sample3xui = `#!/bin/bash
resolve_latest_tag() {
    url=$(curl -sSLI -o /dev/null -w '%{url_effective}' "https://github.com/MHSanaei/3x-ui/releases/latest" 2>/dev/null)
    curl -Ls "https://api.github.com/repos/MHSanaei/3x-ui/releases/latest" | grep '"tag_name":'
}
curl -fLR -o ${xui_folder}-linux-$(arch).tar.gz https://github.com/MHSanaei/3x-ui/releases/download/${tag_version}/x-ui-linux-$(arch).tar.gz
curl -fLRo "${xui_script_temp}" "https://raw.githubusercontent.com/MHSanaei/3x-ui/${script_ref}/x-ui.sh"
curl -fLRo x "https://raw.githubusercontent.com/MHSanaei/3x-ui/main/x-ui.service.debian"
curl -s https://get.acme.sh | sh
local response=$(curl -s --max-time 3 "https://api4.ipify.org")
echo "http://localhost:2222/${web_base_path}"
dnf install -y "https://download.postgresql.org/pub/repos/yum/reporpms/EL-${elver}-$(uname -m)/pgdg-redhat-repo-latest.noarch.rpm"
`

func TestScanScript3xui(t *testing.T) {
	r := scanScript(sample3xui)
	if !reflect.DeepEqual(r.Repos, []string{"MHSanaei/3x-ui"}) {
		t.Fatalf("repos = %v", r.Repos)
	}
	if len(r.Raw) != 2 || r.Raw[0].Ref != "${script_ref}" || r.Raw[1] != (RawRef{"MHSanaei", "3x-ui", "main", "x-ui.service.debian"}) {
		t.Fatalf("raw = %+v", r.Raw)
	}
	want := []string{"https://get.acme.sh", "https://api4.ipify.org",
		"https://download.postgresql.org/pub/repos/yum/reporpms/EL-${elver}-$(uname -m)/pgdg-redhat-repo-latest.noarch.rpm"}
	if !reflect.DeepEqual(r.Ext, want) {
		t.Fatalf("ext = %#v", r.Ext)
	}
}

func TestScanVariables(t *testing.T) {
	s := `REPO="sahand363/loadinme"
BRANCH="${BRANCH:-master}"
URL="https://github.com/${REPO}/releases/latest/download/loadinme-linux-${GO_ARCH}"
_url="https://raw.githubusercontent.com/acmesh-official/acme.sh/$BRANCH/acme.sh"`
	r := scanScript(s)
	if !reflect.DeepEqual(r.Repos, []string{"sahand363/loadinme", "acmesh-official/acme.sh"}) {
		t.Fatalf("repos = %v", r.Repos)
	}
	if len(r.Raw) != 1 || r.Raw[0].Ref != "master" || r.Raw[0].Path != "acme.sh" {
		t.Fatalf("raw = %+v", r.Raw)
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/releases/latest":                    "github",
		"https://api.github.com/repos/o/r/releases/latest":          "github",
		"https://raw.githubusercontent.com/o/r/main/install.sh":     "raw",
		"https://raw.githubusercontent.com/o/r/refs/heads/main/a/b": "raw",
		"https://github.com/${REPO}/releases":                       "skip",
		"https://github.com/settings/tokens":                        "skip",
		"https://objects.githubusercontent.com/x":                   "skip",
		"http://127.0.0.1:8080/health":                              "skip",
		"https://${domain}:443/":                                    "skip",
		"https://get.acme.sh":                                       "ext",
	}
	for u, want := range cases {
		if got := classifyURL(u).Kind; got != want {
			t.Errorf("%s: got %s want %s", u, got, want)
		}
	}
	if c := classifyURL("https://raw.githubusercontent.com/o/r/refs/heads/main/a/b"); c.Raw.Ref != "main" || c.Raw.Path != "a/b" {
		t.Errorf("refs/heads parse: %+v", c.Raw)
	}
}

func TestFindInstallCommand(t *testing.T) {
	readme := "# x\n```bash\nbash <(curl -Ls https://raw.githubusercontent.com/sahand363/zimzim/main/install.sh)\n```\n" +
		"curl -fsSL https://raw.githubusercontent.com/other/x/main/install.sh | bash\n"
	if got := findInstallCommand(readme, "sahand363", "zimzim"); got != "bash <(curl -Ls https://raw.githubusercontent.com/sahand363/zimzim/main/install.sh)" {
		t.Fatalf("got %q", got)
	}
	r2 := "curl -fsSL https://raw.githubusercontent.com/sahand363/loadinme/main/install.sh | bash\n"
	if got := findInstallCommand(r2, "sahand363", "loadinme"); got != "curl -fsSL https://raw.githubusercontent.com/sahand363/loadinme/main/install.sh | bash" {
		t.Fatalf("got %q", got)
	}
	if u := firstURL("bash <(curl -Ls https://raw.githubusercontent.com/mhsanaei/3x-ui/master/install.sh)"); u != "https://raw.githubusercontent.com/mhsanaei/3x-ui/master/install.sh" {
		t.Fatalf("firstURL %q", u)
	}
}

func TestAssetArch(t *testing.T) {
	cases := map[string]string{
		"x-ui-linux-amd64.tar.gz": "amd64", "x-ui-linux-arm64.tar.gz.sha256": "arm64",
		"x-ui-linux-armv7.tar.gz": "armv7", "x-ui-linux-armv6.tar.gz": "other", "x-ui-linux-386.tar.gz": "386",
		"x-ui-windows-amd64.zip": "otheros", "Xray-linux-64.zip": "", "app-arm64-v8a-release.apk": "android",
		"checksums.txt": "", "foo_Linux_x86_64.tar.gz": "amd64", "tool-linux-arm.tar.gz": "armv7",
	}
	for n, want := range cases {
		if got := assetArch(n); got != want {
			t.Errorf("%s: got %q want %q", n, got, want)
		}
	}
	p := repoPolicy{archs: map[string]bool{"amd64": true}}
	if !p.allows("x-ui-linux-amd64.tar.gz") || p.allows("x-ui-linux-arm64.tar.gz") || !p.allows("app.apk") || !p.allows("checksums.txt") {
		t.Error("policy filter wrong")
	}
}

func TestSelectReleases(t *testing.T) {
	now := time.Now()
	rel := func(tag string, ago int, pre bool) Release {
		return Release{Tag: tag, Pre: pre, Published: now.Add(-time.Duration(ago) * time.Hour)}
	}
	in := []Release{rel("dev-latest", 0, true), rel("v3", 1, false), rel("v2", 2, false), rel("v1", 3, false), rel("android-v5", 4, false), rel("v0", 5, false)}
	got := selectReleases(in, 2, false)
	var tags []string
	for _, r := range got {
		tags = append(tags, r.Tag)
	}
	if !reflect.DeepEqual(tags, []string{"v3", "v2", "android-v5"}) {
		t.Fatalf("tags = %v", tags)
	}
}
