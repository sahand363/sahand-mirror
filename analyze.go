package main

// Readiness analyzer: a deterministic (no AI) scan of an app's mirrored
// install scripts for everything that will still need the outside world on
// shutdown day — OS packages, IP-detection services, Let's Encrypt, docker
// images, language package managers and unmirrored URLs.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Finding is one raw observation in a script. Its status (handled / needs
// action / can't handle offline) is computed later against the hub's state.
type Finding struct {
	Kind    string `json:"kind"`         // pkg, ipdetect, ssl, docker, lang, aptrepo
	PM      string `json:"pm,omitempty"` // apt, dnf, yum, apk, pacman, zypper, pip, npm, go, …
	Name    string `json:"name"`
	Detail  string `json:"detail,omitempty"`
	Where   string `json:"where,omitempty"` // script:line
	Dynamic bool   `json:"dynamic,omitempty"`
}

// AIItem is one suggestion from the optional AI second opinion.
type AIItem struct {
	Category   string `json:"category"`
	Name       string `json:"name"`
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion"`
	Status     string `json:"status"` // action | offline | info
}

type Readiness struct {
	Analyzed time.Time `json:"analyzed"`
	Scripts  []string  `json:"scripts"`
	Findings []Finding `json:"findings"`
	AI       []AIItem  `json:"ai,omitempty"`
	AIAt     time.Time `json:"ai_at,omitempty"`
	AIModel  string    `json:"ai_model,omitempty"`
}

type ScriptDoc struct {
	URL  string `json:"url"`
	Body string `json:"-"`
	File string `json:"file"`
}

// ---------- IP detection services ----------

// ipDetectEntries: "host" means the bare host (root path), "host/path" an
// exact path. Multi-purpose hosts (ipinfo.io serves JSON at /) only match
// the plain-IP path.
var ipDetectEntries = []string{
	"api.ipify.org", "api4.ipify.org", "api6.ipify.org", "api64.ipify.org",
	"icanhazip.com", "ipv4.icanhazip.com", "ipv6.icanhazip.com",
	"ident.me", "4.ident.me", "6.ident.me", "v4.ident.me", "v6.ident.me",
	"checkip.amazonaws.com", "ifconfig.me", "ifconfig.me/ip", "ifconfig.co", "ifconfig.co/ip",
	"ip.sb", "ipv4.ip.sb", "ipv6.ip.sb", "api.ip.sb/ip", "api-ipv4.ip.sb/ip",
	"ipinfo.io/ip", "v4.api.ipinfo.io/ip", "v6.api.ipinfo.io/ip",
	"ipv4.myexternalip.com/raw", "myexternalip.com/raw", "check-host.net/ip",
	"ipecho.net/plain", "wtfismyip.com/text", "ip.me", "ipv4.wtfismyip.com/text",
}

var ipDetectHosts = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range ipDetectEntries {
		h, _, _ := strings.Cut(e, "/")
		m[h] = true
	}
	return m
}()

// ipDetectMatch reports whether a URL (with or without scheme) is an IP
// detection endpoint the wrapper rewrites to /myip.
func ipDetectMatch(u string) bool {
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	pu, err := url.Parse(u)
	if err != nil {
		return false
	}
	p := strings.TrimSuffix(pu.Path, "/")
	return contains(ipDetectEntries, strings.ToLower(pu.Hostname())+p)
}

// ---------- package managers ----------

var pmCmdRe = regexp.MustCompile(`(?:^|[\s;&|({` + "`" + `]|\$\()(?:sudo\s+|command\s+|env\s+|[A-Z_]+=\S*\s+)*(apt-get|apt|aptitude|dnf|yum|microdnf|apk|pacman|zypper|pip3?|python3?\s+-m\s+pip|npm|yarn|pnpm|go|cargo|gem)\s([^\n;&|)` + "`" + `]*)`)

// options that take a separate argument, per tool
var pmArgOpts = map[string]map[string]bool{
	"apt":    {"-o": true, "-t": true, "-c": true, "--target-release": true, "--option": true},
	"dnf":    {"--repo": true, "--enablerepo": true, "--disablerepo": true, "--setopt": true, "--releasever": true, "-c": true, "--installroot": true, "--exclude": true, "-x": true},
	"apk":    {"--repository": true, "-X": true, "--root": true, "-p": true, "--arch": true, "--virtual": true, "-t": true},
	"pacman": {"--root": true, "-r": true, "--config": true, "--dbpath": true, "-b": true},
	"zypper": {"--repo": true, "-r": true, "--from": true, "--type": true, "-t": true},
	"pip":    {"-r": true, "--requirement": true, "-i": true, "--index-url": true, "--extra-index-url": true, "-c": true, "--target": true, "-t": true, "--prefix": true, "-e": true},
	"npm":    {"--prefix": true, "--registry": true},
	"go":     {},
	"cargo":  {"--root": true, "--version": true, "--git": true, "--branch": true, "--tag": true},
	"gem":    {"-v": true, "--version": true, "--source": true, "-i": true, "--install-dir": true},
}

var arrayRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\[[@*]\]\}`)

var pkgNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._@/:~-]*$`)

func normPM(tool string) string {
	switch {
	case tool == "apt-get" || tool == "apt" || tool == "aptitude":
		return "apt"
	case tool == "microdnf":
		return "dnf"
	case strings.HasPrefix(tool, "pip") || strings.Contains(tool, "pip"):
		return "pip"
	case tool == "yarn" || tool == "pnpm":
		return "npm"
	}
	return tool
}

// installArgs returns the package arguments after the install verb, or
// ok=false when the command is not an install.
func installArgs(pm string, toks []string) (pkgs []string, ok bool) {
	opts := pmArgOpts[pm]
	if pm == "yum" {
		opts = pmArgOpts["dnf"]
	}
	verbAt := -1
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if strings.HasPrefix(t, "-") {
			if pm == "pacman" && (t == "-S" || t == "--sync" || regexp.MustCompile(`^-S[yuw]*$`).MatchString(t)) {
				verbAt = i
				break
			}
			if opts[t] {
				i++
			}
			continue
		}
		switch pm {
		case "apt", "dnf", "yum", "zypper", "pip", "npm", "go", "cargo", "gem":
			if t == "install" || (pm == "zypper" && t == "in") || (pm == "npm" && (t == "i" || t == "add")) || (pm == "dnf" && t == "reinstall") {
				verbAt = i
			} else if pm == "npm" && t == "global" && i+1 < len(toks) && toks[i+1] == "add" {
				verbAt = i + 1
			}
		case "apk":
			if t == "add" {
				verbAt = i
			}
		}
		if verbAt < 0 {
			return nil, false // first non-option word is some other subcommand
		}
		break
	}
	if verbAt < 0 {
		return nil, false
	}
	for i := verbAt + 1; i < len(toks); i++ {
		t := toks[i]
		if strings.HasPrefix(t, ">") || strings.HasPrefix(t, "<") || strings.HasPrefix(t, "2>") || strings.HasPrefix(t, "1>") || strings.HasPrefix(t, "&>") || t == "#" {
			break
		}
		if strings.HasPrefix(t, "-") {
			if opts[t] {
				i++
			}
			continue
		}
		pkgs = append(pkgs, t)
	}
	return pkgs, true
}

// pkgVars: literal assignments usable to expand package lists, including
// quoted lists with spaces (PKGS="curl jq") and arrays (PKGS=(curl jq)).
var listAssignRe = regexp.MustCompile(`(?m)(?:^|[\s;])(?:export\s+|local\s+|readonly\s+|declare\s+(?:-a\s+)?)?([A-Za-z_][A-Za-z0-9_]*)=(?:"([^"$\x60\n]*)"|'([^'\n]*)'|\(([^)$\x60\n]*)\))`)

func pkgVars(script string) map[string]string {
	vars := scriptVars(script)
	for _, m := range listAssignRe.FindAllStringSubmatch(script, -1) {
		v := m[2] + m[3] + strings.ReplaceAll(strings.ReplaceAll(m[4], `"`, ""), "'", "")
		if strings.TrimSpace(v) != "" {
			if _, ok := vars[m[1]]; !ok || strings.Contains(v, " ") {
				vars[m[1]] = strings.TrimSpace(v)
			}
		}
	}
	return vars
}

// logicalLines joins backslash continuations and drops comment lines,
// returning each line with its 1-based starting line number.
func logicalLines(body string) ([]string, []int) {
	raw := strings.Split(body, "\n")
	var out []string
	var nums []int
	for i := 0; i < len(raw); i++ {
		start := i
		l := raw[i]
		for strings.HasSuffix(l, "\\") && i+1 < len(raw) {
			i++
			l = strings.TrimSuffix(l, "\\") + " " + raw[i]
		}
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, l)
		nums = append(nums, start+1)
	}
	return out, nums
}

func shortName(u string) string {
	if u == "recipe" {
		return "recipe"
	}
	if i := strings.LastIndex(u, "/"); i >= 0 && i < len(u)-1 {
		return u[i+1:]
	}
	return u
}

var (
	sslRe        = regexp.MustCompile(`(?i)(acme\.sh|get\.acme\.sh|certbot|letsencrypt|let's encrypt|zerossl)`)
	dockerPullRe = regexp.MustCompile(`\bdocker\s+(pull|run|create)\s+([^\n;&|)]*)`)
	composeRe    = regexp.MustCompile(`\b(docker[\s-]compose|docker\s+compose)\b`)
	imageLineRe  = regexp.MustCompile(`(?m)^\s*image:\s*["']?([A-Za-z0-9][A-Za-z0-9._/:@${}-]+)`)
	getDockerRe  = regexp.MustCompile(`get\.docker\.com|download\.docker\.com`)
	aptRepoRe    = regexp.MustCompile(`(?i)(\bdeb\s+(?:\[[^\]]*\]\s+)?(https?://[^\s"']+)|add-apt-repository\s+([^\s;&|]+)|^\s*URIs:\s*(\S+)|apt\.postgresql\.org\.sh|packagecloud\.io|cloudsmith\.io|dl\.cloudsmith|download\.docker\.com/linux)`)
	dockerArgOpt = map[string]bool{"-p": true, "-v": true, "-e": true, "--name": true, "--network": true, "--net": true, "-w": true,
		"--restart": true, "--env-file": true, "-u": true, "--user": true, "--entrypoint": true, "--mount": true, "-l": true,
		"--label": true, "--log-opt": true, "--log-driver": true, "--add-host": true, "--device": true, "--cap-add": true,
		"--cap-drop": true, "-h": true, "--hostname": true, "-m": true, "--memory": true, "--cpus": true, "--dns": true,
		"--ulimit": true, "--platform": true, "--publish": true, "--volume": true, "--env": true, "--workdir": true, "--tmpfs": true,
		"--security-opt": true, "--sysctl": true, "--shm-size": true, "--pid": true, "--ipc": true, "--expose": true}
)

func unquote(t string) string {
	return strings.Trim(t, `"'`)
}

// analyzeScripts runs every detector over the scripts and returns deduped findings.
func analyzeScripts(docs []ScriptDoc) []Finding {
	var out []Finding
	seen := map[string]bool{}
	add := func(f Finding) {
		k := f.Kind + "|" + f.PM + "|" + f.Name
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, f)
	}
	for _, d := range docs {
		vars := pkgVars(d.Body)
		lines, nums := logicalLines(d.Body)
		src := shortName(d.URL)
		for li, line := range lines {
			where := fmt.Sprintf("%s:%d", src, nums[li])
			// package managers
			for _, m := range pmCmdRe.FindAllStringSubmatch(line, -1) {
				pm := normPM(strings.Join(strings.Fields(m[1]), " "))
				toks := strings.Fields(m[2])
				pkgs, ok := installArgs(pm, toks)
				if !ok {
					continue
				}
				kind := "pkg"
				if pm == "pip" || pm == "npm" || pm == "go" || pm == "cargo" || pm == "gem" {
					kind = "lang"
				}
				for _, p := range pkgs {
					p = unquote(p)
					if p == "" || p == "\\" || strings.Contains(p, "://") {
						continue
					}
					if strings.Contains(p, "$") {
						exp := expandVars(arrayRefRe.ReplaceAllString(p, "$${$1}"), vars)
						if strings.Contains(exp, "$") {
							add(Finding{Kind: kind, PM: pm, Name: p, Where: where, Dynamic: true,
								Detail: "package name built from a variable"})
							continue
						}
						for _, x := range strings.Fields(exp) {
							addPkg(add, kind, pm, x, where)
						}
						continue
					}
					addPkg(add, kind, pm, p, where)
				}
			}
			// IP detection services: URLs and bare hosts
			for _, u := range urlRe.FindAllString(line, -1) {
				u = strings.TrimRight(u, ".,:")
				if ipDetectMatch(u) {
					pu, _ := url.Parse(u)
					add(Finding{Kind: "ipdetect", Name: pu.Host + strings.TrimSuffix(pu.Path, "/"), Where: where})
				}
			}
			for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return strings.ContainsRune(" \t\"'()=;|&`", r) }) {
				if !strings.Contains(tok, "://") && ipDetectHosts[strings.ToLower(strings.SplitN(tok, "/", 2)[0])] && ipDetectMatch(tok) {
					add(Finding{Kind: "ipdetect", Name: strings.TrimSuffix(tok, "/"), Where: where})
				}
			}
			// Let's Encrypt / acme.sh / certbot
			if m := sslRe.FindString(line); m != "" {
				name := strings.ToLower(m)
				switch {
				case strings.Contains(name, "acme"):
					name = "acme.sh"
				case name == "letsencrypt" || strings.HasPrefix(name, "let"):
					name = "Let's Encrypt"
				}
				add(Finding{Kind: "ssl", Name: name, Where: where})
			}
			// docker
			for _, m := range dockerPullRe.FindAllStringSubmatch(line, -1) {
				toks := strings.Fields(m[2])
				for i := 0; i < len(toks); i++ {
					t := unquote(toks[i])
					if strings.HasPrefix(t, "-") {
						if dockerArgOpt[t] {
							i++
						}
						continue
					}
					if strings.HasPrefix(t, ">") || t == "\\" {
						break
					}
					add(Finding{Kind: "docker", PM: "image", Name: t, Where: where, Dynamic: strings.Contains(t, "$")})
					break
				}
			}
			if composeRe.MatchString(line) {
				add(Finding{Kind: "docker", PM: "compose", Name: "docker compose", Where: where})
			}
			if getDockerRe.MatchString(line) {
				add(Finding{Kind: "docker", PM: "engine", Name: "Docker Engine (get.docker.com / download.docker.com)", Where: where})
			}
			// third-party apt repositories
			if m := aptRepoRe.FindStringSubmatch(line); m != nil {
				name := m[2] + m[3] + m[4]
				if name == "" {
					name = m[1]
				}
				add(Finding{Kind: "aptrepo", Name: unquote(name), Where: where})
			}
		}
		for _, m := range imageLineRe.FindAllStringSubmatch(d.Body, -1) {
			add(Finding{Kind: "docker", PM: "image", Name: m[1], Where: src, Dynamic: strings.Contains(m[1], "$")})
		}
	}
	return out
}

func addPkg(add func(Finding), kind, pm, p, where string) {
	if strings.Contains(p, "://") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "./") ||
		strings.HasSuffix(p, ".deb") || strings.HasSuffix(p, ".rpm") || strings.ContainsAny(p, "*?[") {
		return // local file or URL: the URL itself shows up under external URLs
	}
	if pm == "pip" {
		if i := strings.IndexAny(p, "=<>!~[;"); i > 0 {
			p = p[:i]
		}
	}
	if kind == "pkg" {
		if i := strings.IndexAny(p, "=/"); i > 0 && pm == "apt" {
			p = p[:i] // pkg=version, pkg/suite
		}
		if pm == "apt" {
			p = strings.TrimSuffix(strings.TrimSuffix(p, ":any"), ":native")
			if i := strings.Index(p, ":"); i > 0 {
				p = p[:i]
			}
		}
	}
	if !pkgNameRe.MatchString(p) {
		return
	}
	add(Finding{Kind: kind, PM: pm, Name: p, Where: where})
}

// ---------- script storage ----------

func scriptsDir(data, app string) string { return filepath.Join(data, "scripts", slugify(app)) }

// saveScripts stores the install scripts discovered during a sync so the
// analyzer and the AI can run later without internet.
func saveScripts(data, app string, docs []ScriptDoc) error {
	dir := scriptsDir(data, app)
	tmp := dir + ".tmp-" + randHex(4)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	var idx []ScriptDoc
	for i, d := range docs {
		f := fmt.Sprintf("%02d-%s", i, slugify(shortName(d.URL)))
		if err := os.WriteFile(filepath.Join(tmp, f), []byte(d.Body), 0o644); err != nil {
			os.RemoveAll(tmp)
			return err
		}
		idx = append(idx, ScriptDoc{URL: d.URL, File: f})
	}
	b, _ := json.MarshalIndent(idx, "", " ")
	if err := os.WriteFile(filepath.Join(tmp, "index.json"), b, 0o644); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	os.RemoveAll(dir)
	return os.Rename(tmp, dir)
}

func loadSavedScripts(data, app string) []ScriptDoc {
	dir := scriptsDir(data, app)
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil
	}
	var idx []ScriptDoc
	if json.Unmarshal(b, &idx) != nil {
		return nil
	}
	var out []ScriptDoc
	for _, d := range idx {
		if !validName(d.File) {
			continue
		}
		if body, err := os.ReadFile(filepath.Join(dir, d.File)); err == nil {
			d.Body = string(body)
			out = append(out, d)
		}
	}
	return out
}

// ---------- evaluation against hub state ----------

// ReadyItem is a finding with its current status, as the UI shows it.
type ReadyItem struct {
	Category   string `json:"category"` // pkg, ipdetect, ssl, docker, lang, aptrepo, ext, ai
	PM         string `json:"pm,omitempty"`
	Name       string `json:"name"`
	Detail     string `json:"detail,omitempty"`
	Where      string `json:"where,omitempty"`
	Status     string `json:"status"` // handled | action | offline | info
	Fix        string `json:"fix,omitempty"`
	FixArg     string `json:"fix_arg,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
	Source     string `json:"source"` // auto | ai
}

type ReadySummary struct {
	Handled int `json:"handled"`
	Action  int `json:"action"`
	Offline int `json:"offline"`
}

// aptPackagesOf lists literal apt package names the analyzer found.
func aptPackagesOf(a *App) []string {
	var out []string
	if a.Readiness != nil {
		for _, f := range a.Readiness.Findings {
			if f.Kind == "pkg" && f.PM == "apt" && !f.Dynamic && !contains(out, f.Name) {
				out = append(out, f.Name)
			}
		}
	}
	for _, p := range a.AptExtra {
		if !contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func evaluateReadiness(a *App, st *State, cfg Config) ([]ReadyItem, ReadySummary) {
	var items []ReadyItem
	targets := aptTargetsFor(cfg, a.Archs)
	if a.Readiness != nil {
		other := map[string][]string{}
		otherWhere := map[string]string{}
		for _, f := range a.Readiness.Findings {
			it := ReadyItem{Category: f.Kind, PM: f.PM, Name: f.Name, Detail: f.Detail, Where: f.Where, Source: "auto"}
			switch f.Kind {
			case "pkg":
				if f.PM != "apt" {
					other[f.PM] = append(other[f.PM], f.Name)
					if otherWhere[f.PM] == "" {
						otherWhere[f.PM] = f.Where
					}
					continue
				}
				if f.Dynamic {
					it.Status, it.Fix = "action", "prefetch"
					it.Suggestion = "نام بسته با متغیر ساخته می‌شود؛ نام واقعی را در «پیش‌دریافت بسته‌ها» اضافه کنید."
					break
				}
				missing := aptMissing(st, targets, f.Name)
				switch {
				case len(targets) == 0:
					it.Status, it.Suggestion = "action", "در تنظیمات هیچ توزیعی برای پیش‌دریافت apt انتخاب نشده."
				case len(missing) == 0:
					it.Status, it.Detail = "handled", "در کش apt هاب برای همهٔ هدف‌ها"
				default:
					it.Status, it.Fix = "action", "prefetch"
					it.Detail = "در کش نیست: " + strings.Join(missing, "، ")
					it.Suggestion = "«پیش‌دریافت بسته‌ها» را بزنید تا این بسته و وابستگی‌هایش برای اوبونتو/دبیان کش شود."
				}
			case "ipdetect":
				it.Status, it.Detail = "handled", "دستور نصب این آدرس را به ‎/myip‎ هاب هدایت می‌کند"
			case "ssl":
				it.Status, it.Fix = "action", "cert"
				it.Suggestion = "Let's Encrypt بدون اینترنت بین‌الملل کار نمی‌کند. در پرسش‌های نصب گزینهٔ self-signed یا «رد کردن SSL» را انتخاب کنید و بعد با دکمهٔ «صدور گواهی داخلی» برای IP/دامنهٔ سرور گواهی بگیرید."
				if f.Name == "acme.sh" {
					it.Detail = "نصب acme.sh از هاب انجام می‌شود (get.acme.sh و مخزن acme.sh آینه می‌شوند)، ولی صدور گواهی به سرورهای Let's Encrypt نیاز دارد"
				}
			case "docker":
				it.Status = "offline"
				switch f.PM {
				case "engine":
					it.Suggestion = "مخزن apt داکر (download.docker.com) HTTPS است و کش نمی‌شود؛ داکر را از قبل روی سرورها نصب کنید."
				case "compose":
					it.Suggestion = "ایمیج‌های compose را از قبل pull کنید یا با docker save/load منتقل کنید."
				default:
					it.Suggestion = "ایمیج داکر آینه نمی‌شود؛ از قبل pull کنید یا با «docker save -o img.tar» و «docker load -i img.tar» منتقل کنید (یا رجیستری داخلی)."
				}
			case "lang":
				it.Status = "offline"
				it.Suggestion = map[string]string{
					"pip":   "PyPI آینه نمی‌شود؛ بسته را از قبل نصب کنید یا با «pip download» چرخ‌ها را جمع کنید (یا میرور داخلی PyPI).",
					"npm":   "npm آینه نمی‌شود؛ از قبل نصب کنید یا میرور داخلی npm (مثلاً رجیستری داخلی) تنظیم کنید.",
					"go":    "go install به proxy.golang.org نیاز دارد؛ باینری را از ریلیز گیت‌هاب استفاده کنید یا GOPROXY داخلی.",
					"cargo": "crates.io آینه نمی‌شود؛ باینری آماده استفاده کنید.",
					"gem":   "rubygems آینه نمی‌شود؛ از قبل نصب کنید.",
				}[f.PM]
			case "aptrepo":
				it.Status = "offline"
				it.Suggestion = "مخزن apt شخص ثالث؛ اگر HTTPS باشد از هاب کش نمی‌شود و روز قطعی شکست می‌خورد (دستور نصب هاب مخزن‌های HTTPS در دسترس‌نبوده را موقتاً کنار می‌گذارد)."
				if strings.HasPrefix(f.Name, "http://") {
					it.Status, it.Suggestion = "handled", "مخزن HTTP است؛ از طریق پراکسی apt هاب کش می‌شود (به شرط یک‌بار نصب در زمان وصل بودن)."
				}
			default:
				it.Status = "info"
			}
			items = append(items, it)
		}
		pms := make([]string, 0, len(other))
		for pm := range other {
			pms = append(pms, pm)
		}
		sort.Strings(pms)
		for _, pm := range pms {
			items = append(items, ReadyItem{Category: "pkg", PM: pm, Name: strings.Join(dedupe(other[pm]), " "), Where: otherWhere[pm],
				Status: "offline", Source: "auto",
				Suggestion: "هاب فقط apt (اوبونتو/دبیان) را کش می‌کند؛ برای " + pm + " میرور داخلی سیستم‌عامل لازم است یا این بسته‌ها را از قبل نصب کنید."})
		}
	}
	// external URLs
	for _, u := range a.Externals {
		if ipDetectMatch(u) {
			continue
		}
		e := st.Ext[u]
		it := ReadyItem{Category: "ext", Name: u, Source: "auto", FixArg: u}
		switch {
		case e == nil:
			continue
		case e.Dynamic:
			it.Status, it.Suggestion = "offline", "آدرس با متغیر ساخته می‌شود و قابل آینه نیست؛ اگر ضروری است فایل را دستی در رسپی جایگزین کنید."
		case e.Mirror && !e.Fetched.IsZero() && e.Error == "":
			it.Status, it.Detail = "handled", "آینه شده ("+humanBytes(e.Size)+")"
		case e.Mirror && e.Error != "":
			it.Status, it.Detail, it.Fix = "action", e.Error, "sync"
		case e.Mirror:
			it.Status, it.Detail, it.Fix = "action", "روشن است ولی هنوز دریافت نشده", "sync"
		default:
			it.Status, it.Fix = "action", "mirror-ext"
			it.Suggestion = "اگر اسکریپت این فایل را دانلود می‌کند، «آینه شود» را بزنید و همگام‌سازی کنید."
		}
		items = append(items, it)
	}
	if a.Readiness != nil {
		for _, x := range a.Readiness.AI {
			st := x.Status
			if st != "action" && st != "offline" && st != "handled" {
				st = "info"
			}
			items = append(items, ReadyItem{Category: "ai", PM: x.Category, Name: x.Name, Detail: x.Detail, Suggestion: x.Suggestion, Status: st, Source: "ai"})
		}
	}
	var sum ReadySummary
	for _, it := range items {
		switch it.Status {
		case "handled":
			sum.Handled++
		case "action":
			sum.Action++
		case "offline":
			sum.Offline++
		}
	}
	return items, sum
}

func dedupe(l []string) []string {
	var out []string
	for _, x := range l {
		if !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// truncateScripts builds the AI prompt body: every script, each capped, total capped.
func truncateScripts(docs []ScriptDoc, per, total int) string {
	var b strings.Builder
	for _, d := range docs {
		body := d.Body
		if len(body) > per {
			head := body[:per*2/3]
			tail := body[len(body)-per/3:]
			body = head + "\n# … [" + strconv.Itoa(len(d.Body)-per) + " bytes truncated] …\n" + tail
		}
		chunk := "### " + d.URL + "\n```sh\n" + body + "\n```\n"
		if b.Len()+len(chunk) > total {
			b.WriteString("### (more scripts omitted: " + d.URL + ")\n")
			continue
		}
		b.WriteString(chunk)
	}
	return b.String()
}
