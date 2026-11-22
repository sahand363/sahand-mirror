package main

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

// URLs in shell scripts may contain ${var}, $(cmd) and $var parts; keep them
// so the UI can show the whole URL and we know it is dynamic.
var urlRe = regexp.MustCompile(`https?://(?:[^\s"'<>|;\\(){}\[\]$` + "`" + `]|\$\{[^}\s]*\}|\$\([^)\n"']*\)|\$[A-Za-z_][A-Za-z0-9_]*)+`)

// NAME=value or NAME="${NAME:-value}" with a literal value.
var assignRe = regexp.MustCompile(`(?m)(?:^|[\s;])(?:export\s+|local\s+|readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)=["']?(?:\$\{[A-Za-z_][A-Za-z0-9_]*:-([^}"'\s$]+)\}|([^"'\s$;()]+))["']?`)
var varRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// GitHub path segments that are not owners.
var ghReserved = map[string]bool{"orgs": true, "settings": true, "login": true, "features": true, "about": true,
	"site": true, "sponsors": true, "topics": true, "marketplace": true, "search": true, "apps": true,
	"users": true, "notifications": true, "explore": true, "collections": true, "pricing": true, "enterprise": true}

type RawRef struct {
	Owner, Repo, Ref, Path string
}

func (r RawRef) URL() string {
	return "https://raw.githubusercontent.com/" + r.Owner + "/" + r.Repo + "/" + r.Ref + "/" + r.Path
}

type URLClass struct {
	Kind  string // "raw", "github", "ext", "skip"
	Owner string
	Repo  string
	Raw   RawRef
}

func literal(s string) bool { return s != "" && !strings.Contains(s, "$") }

// classifyURL decides what a URL found in a script refers to.
func classifyURL(u string) URLClass {
	pu, err := url.Parse(u)
	if err != nil || pu.Host == "" {
		return URLClass{Kind: "skip"}
	}
	host := strings.ToLower(pu.Hostname())
	segs := strings.Split(strings.Trim(pu.Path, "/"), "/")
	ownerRepo := func(o, r string) URLClass {
		r = strings.TrimSuffix(r, ".git")
		if !literal(o) || !literal(r) || !validName(o) || !validName(r) || ghReserved[strings.ToLower(o)] {
			return URLClass{Kind: "skip"}
		}
		return URLClass{Kind: "github", Owner: o, Repo: r}
	}
	switch host {
	case "github.com", "www.github.com", "codeload.github.com":
		if len(segs) >= 2 {
			return ownerRepo(segs[0], segs[1])
		}
		return URLClass{Kind: "skip"}
	case "api.github.com":
		if len(segs) >= 3 && segs[0] == "repos" {
			return ownerRepo(segs[1], segs[2])
		}
		return URLClass{Kind: "skip"}
	case "raw.githubusercontent.com":
		if len(segs) < 4 {
			return URLClass{Kind: "skip"}
		}
		c := ownerRepo(segs[0], segs[1])
		if c.Kind != "github" {
			return c
		}
		ref, rest := segs[2], segs[3:]
		if ref == "refs" && len(segs) >= 6 && (segs[3] == "heads" || segs[3] == "tags") {
			ref, rest = segs[4], segs[5:]
		}
		c.Kind = "raw"
		c.Raw = RawRef{c.Owner, c.Repo, ref, strings.Join(rest, "/")}
		return c
	case "objects.githubusercontent.com", "release-assets.githubusercontent.com", "github.githubassets.com", "avatars.githubusercontent.com":
		return URLClass{Kind: "skip"}
	}
	if strings.Contains(host, "$") || !strings.Contains(host, ".") || host == "localhost" ||
		strings.HasSuffix(host, ".local") || host == "example.com" || strings.HasSuffix(host, ".example.com") {
		return URLClass{Kind: "skip"}
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified()) {
		return URLClass{Kind: "skip"}
	}
	return URLClass{Kind: "ext"}
}

type ScanResult struct {
	Repos []string // "Owner/Repo" (original case, deduped case-insensitively)
	Raw   []RawRef
	Ext   []string
}

// scriptVars collects simple literal shell assignments (REPO="a/b", BRANCH="${BRANCH:-master}").
func scriptVars(script string) map[string]string {
	vars := map[string]string{}
	for _, m := range assignRe.FindAllStringSubmatch(script, -1) {
		v := m[3]
		if m[2] != "" {
			v = m[2]
		}
		if _, seen := vars[m[1]]; !seen && v != "" {
			vars[m[1]] = v
		}
	}
	return vars
}

func expandVars(u string, vars map[string]string) string {
	return varRefRe.ReplaceAllStringFunc(u, func(s string) string {
		m := varRefRe.FindStringSubmatch(s)
		name := m[1] + m[3]
		if v, ok := vars[name]; ok {
			return v
		}
		if m[2] != "" && !strings.Contains(m[2], "$") {
			return m[2]
		}
		return s
	})
}

// scanScript extracts GitHub repos, raw files and external URLs from a script.
func scanScript(script string) ScanResult {
	vars := scriptVars(script)
	var res ScanResult
	seenRepo, seenRaw, seenExt := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, u := range urlRe.FindAllString(script, -1) {
		u = strings.TrimRight(u, ".,:")
		u = expandVars(u, vars)
		c := classifyURL(u)
		switch c.Kind {
		case "github", "raw":
			if k := repoKey(c.Owner, c.Repo); !seenRepo[k] {
				seenRepo[k] = true
				res.Repos = append(res.Repos, c.Owner+"/"+strings.TrimSuffix(c.Repo, ".git"))
			}
			if c.Kind == "raw" && c.Raw.Path != "" {
				if k := strings.ToLower(c.Raw.URL()); !seenRaw[k] {
					seenRaw[k] = true
					res.Raw = append(res.Raw, c.Raw)
				}
			}
		case "ext":
			if !seenExt[u] {
				seenExt[u] = true
				res.Ext = append(res.Ext, u)
			}
		}
	}
	return res
}

// firstURL returns the first URL in an install command (the script it fetches).
func firstURL(cmd string) string {
	return strings.TrimRight(urlRe.FindString(cmd), ".,:")
}

var cmdLineRe = regexp.MustCompile(`(?m)^[\s$#>]*((?:sudo\s+)?(?:bash|sh)\s+<\(\s*(?:curl|wget)[^\n]*\)[^\n]*|(?:sudo\s+)?(?:curl|wget)\s[^\n|]*\|\s*(?:sudo\s+)?(?:bash|sh)\b[^\n]*)$`)

// findInstallCommand looks for a one-line install command for owner/repo in a README.
func findInstallCommand(readme, owner, repo string) string {
	want := strings.ToLower(owner + "/" + repo + "/")
	for _, m := range cmdLineRe.FindAllStringSubmatch(readme, -1) {
		line := strings.TrimSpace(strings.Trim(m[1], "`"))
		if strings.Contains(strings.ToLower(line), want) && !strings.Contains(line, "--version") {
			return line
		}
	}
	return ""
}

func looksLikeScript(b []byte) bool {
	if len(b) > 2<<20 {
		return false
	}
	s := string(b)
	return strings.HasPrefix(s, "#!") || strings.Contains(s, "curl ") || strings.Contains(s, "wget ")
}
