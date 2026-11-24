package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// GH talks to GitHub. With a token it uses the REST API; without one (or when
// the API is rate limited) it falls back to the public web pages, which have
// no 60 req/h limit.
type GH struct {
	client *http.Client
	token  string
	base   string // https://github.com (overridable in tests)
	api    string
	raw    string
}

func newGH(token string) *GH {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.ResponseHeaderTimeout = 60 * time.Second
	return &GH{client: &http.Client{Transport: tr}, token: token,
		base: "https://github.com", api: "https://api.github.com", raw: "https://raw.githubusercontent.com"}
}

type httpError struct {
	URL  string
	Code int
}

func (e *httpError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.URL, e.Code) }

func (g *GH) do(ctx context.Context, method, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "sahand-mirror/"+version)
	if strings.HasPrefix(u, g.api) {
		req.Header.Set("Accept", "application/vnd.github+json")
		if g.token != "" {
			req.Header.Set("Authorization", "Bearer "+g.token)
		}
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, &httpError{u, resp.StatusCode}
	}
	return resp, nil
}

func (g *GH) getBytes(ctx context.Context, u string, max int64) ([]byte, error) {
	resp, err := g.do(ctx, "GET", u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

func (g *GH) getJSON(ctx context.Context, u string, v any) error {
	b, err := g.getBytes(ctx, u, 32<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// download streams u into dst atomically.
func (g *GH) download(ctx context.Context, u, dst string) (int64, string, error) {
	resp, err := g.do(ctx, "GET", u)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, done := live.track(filepath.Base(dst), resp.ContentLength, resp.Body)
	defer done()
	return copyAtomic(dst, body)
}

type repoInfo struct {
	Name          string // canonical Owner/Repo
	DefaultBranch string
}

var symrefRe = regexp.MustCompile(`symref=HEAD:refs/heads/([^\s\x00]+)`)

func (g *GH) repoInfo(ctx context.Context, owner, repo string) (repoInfo, error) {
	if g.token != "" {
		var r struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		}
		if err := g.getJSON(ctx, g.api+"/repos/"+owner+"/"+repo, &r); err == nil && r.DefaultBranch != "" {
			return repoInfo{r.FullName, r.DefaultBranch}, nil
		}
	}
	// git smart-HTTP advertisement names the default branch, no API quota used.
	b, err := g.getBytes(ctx, g.base+"/"+owner+"/"+repo+".git/info/refs?service=git-upload-pack", 64<<10)
	if err != nil {
		return repoInfo{}, err
	}
	m := symrefRe.FindSubmatch(b)
	if m == nil {
		return repoInfo{}, fmt.Errorf("%s/%s: default branch not found", owner, repo)
	}
	name := owner + "/" + repo
	// canonical case from the releases feed link, best effort
	if resp, err := g.do(ctx, "HEAD", g.base+"/"+owner+"/"+repo); err == nil {
		resp.Body.Close()
		if p := strings.Trim(resp.Request.URL.Path, "/"); strings.Count(p, "/") == 1 {
			name = p
		}
	}
	return repoInfo{name, string(m[1])}, nil
}

// headSHA returns the commit the default branch points to, read from the git
// smart-HTTP advertisement (small, no API quota).
func (g *GH) headSHA(ctx context.Context, owner, repo string) (string, error) {
	b, err := g.getBytes(ctx, g.base+"/"+owner+"/"+repo+".git/info/refs?service=git-upload-pack", 64<<10)
	if err != nil {
		return "", err
	}
	m := headSHARe.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("%s/%s: HEAD not found", owner, repo)
	}
	return string(m[1]), nil
}

var headSHARe = regexp.MustCompile(`([0-9a-f]{40}) HEAD`)

type ghAPIRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// releases returns up to ~30 (API) or ~10 (web) newest releases, and the
// tag GitHub marks as "latest".
func (g *GH) releases(ctx context.Context, owner, repo string) ([]Release, string, error) {
	if g.token != "" {
		var list []ghAPIRelease
		err := g.getJSON(ctx, g.api+"/repos/"+owner+"/"+repo+"/releases?per_page=30", &list)
		if err == nil {
			var out []Release
			for _, r := range list {
				if r.Draft {
					continue
				}
				rel := Release{Tag: r.TagName, Name: r.Name, Pre: r.Prerelease, Published: r.PublishedAt}
				for _, a := range r.Assets {
					rel.Assets = append(rel.Assets, Asset{Name: a.Name, Size: a.Size})
				}
				out = append(out, rel)
			}
			var latest ghAPIRelease
			g.getJSON(ctx, g.api+"/repos/"+owner+"/"+repo+"/releases/latest", &latest)
			return out, latest.TagName, nil
		}
	}
	return g.webReleases(ctx, owner, repo)
}

type atomFeed struct {
	Entries []struct {
		Updated string `xml:"updated"`
		Title   string `xml:"title"`
		Link    struct {
			Href string `xml:"href,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

var preTagRe = regexp.MustCompile(`(?i)(alpha|beta|rc\d*|dev|pre|nightly|canary|snapshot)`)

func (g *GH) webReleases(ctx context.Context, owner, repo string) ([]Release, string, error) {
	b, err := g.getBytes(ctx, g.base+"/"+owner+"/"+repo+"/releases.atom", 8<<20)
	if err != nil {
		return nil, "", err
	}
	var feed atomFeed
	if err := xml.NewDecoder(bytes.NewReader(b)).Decode(&feed); err != nil {
		return nil, "", fmt.Errorf("releases.atom: %w", err)
	}
	latest := ""
	if tag, err := g.latestTagWeb(ctx, owner, repo); err == nil {
		latest = tag
	}
	var out []Release
	seenLatest := latest == ""
	now := time.Now()
	for _, e := range feed.Entries {
		i := strings.LastIndex(e.Link.Href, "/releases/tag/")
		if i < 0 {
			continue
		}
		tag, err := url.PathUnescape(e.Link.Href[i+len("/releases/tag/"):])
		if err != nil || !validTag(tag) {
			continue
		}
		pub, err := time.Parse(time.RFC3339, strings.TrimSpace(e.Updated))
		if err != nil {
			pub = now.Add(-time.Duration(len(out)) * time.Minute)
		}
		// GitHub's "latest" is the newest stable release, so anything listed
		// before it is a pre-release.
		pre := !seenLatest || preTagRe.MatchString(tag)
		if tag == latest {
			seenLatest, pre = true, false
		}
		out = append(out, Release{Tag: tag, Name: e.Title, Pre: pre, Published: pub})
	}
	return out, latest, nil
}

func (g *GH) latestTagWeb(ctx context.Context, owner, repo string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "HEAD", g.base+"/"+owner+"/"+repo+"/releases/latest", nil)
	req.Header.Set("User-Agent", "sahand-mirror/"+version)
	c := *g.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/releases/tag/")
	if i < 0 {
		return "", fmt.Errorf("no latest release")
	}
	return url.PathUnescape(loc[i+len("/releases/tag/"):])
}

// webAssets lists asset names of a release from the expanded_assets fragment.
func (g *GH) webAssets(ctx context.Context, owner, repo, tag string) ([]Asset, error) {
	b, err := g.getBytes(ctx, g.base+"/"+owner+"/"+repo+"/releases/expanded_assets/"+url.PathEscape(tag), 8<<20)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`(?i)href="/` + regexp.QuoteMeta(owner) + `/` + regexp.QuoteMeta(repo) + `/releases/download/([^/"]+)/([^"]+)"`)
	var out []Asset
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		name, err := url.PathUnescape(m[2])
		if err != nil || !validTag(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, Asset{Name: name})
	}
	return out, nil
}

func (g *GH) assetURL(owner, repo, tag, name string) string {
	return g.base + "/" + owner + "/" + repo + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
}

func (g *GH) tarballURL(owner, repo, kind, ref string) string {
	return g.base + "/" + owner + "/" + repo + "/archive/refs/" + kind + "/" + ref + ".tar.gz"
}
