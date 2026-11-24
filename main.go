package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "v0.3.2"

//go:embed install.sh
var installScript []byte

var cfgPath = "/etc/sahand-mirror/config.json"

const usage = `sahand-mirror %s — internal GitHub mirror hub

usage: sahand-mirror [--config FILE] <command>
  (no command, as root)       interactive menu
  serve                       run the hub (systemd runs this)
  init [--port N] [--public-url URL]
                              create config with a random admin password
  reset-password              set a new random admin password and print it
  status                      show hub status
  sync [--wait]               sync all apps from GitHub now
  export --out FILE           write the whole mirror as a bundle
  import FILE                 import a bundle (verifies sha256 manifest)
  collect --out FILE [--data DIR] [--apps apps.json] [--keep N] [--token T]
                              on a machine WITH internet: download everything
                              into a bundle for an offline hub
  version
`

func main() {
	log.SetFlags(log.LstdFlags)
	args := os.Args[1:]
	if v := os.Getenv("SAHAND_MIRROR_CONFIG"); v != "" {
		cfgPath = v
	}
	if len(args) >= 2 && (args[0] == "--config" || args[0] == "-c") {
		cfgPath, args = args[1], args[2:]
	}
	if len(args) == 0 {
		if os.Geteuid() == 0 && isTTY() {
			runMenu()
			return
		}
		fmt.Printf(usage, version)
		return
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe()
	case "init":
		err = cmdInit(rest)
	case "reset-password":
		var pw string
		pw, err = resetPassword()
		if err == nil {
			fmt.Println("new admin password:", pw)
		}
	case "status":
		err = cmdStatus()
	case "sync":
		err = cmdSync(len(rest) > 0 && rest[0] == "--wait")
	case "export":
		fs := flag.NewFlagSet("export", flag.ExitOnError)
		out := fs.String("out", "", "bundle file")
		fs.Parse(rest)
		if *out == "" {
			err = errors.New("--out is required")
			break
		}
		err = cmdExport(*out)
	case "import":
		if len(rest) != 1 {
			err = errors.New("usage: sahand-mirror import FILE")
			break
		}
		err = cmdImport(rest[0])
	case "collect":
		err = cmdCollect(rest)
	case "menu":
		runMenu()
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Printf(usage, version)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func cmdServe() error {
	cs, err := NewConfigStore(cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w (run `sahand-mirror init`)", err)
	}
	s, err := NewServer(cs)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Run(ctx)
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	port := fs.Int("port", 0, "listen port")
	pub := fs.String("public-url", "", "address target servers use, e.g. http://10.0.0.5:8686")
	data := fs.String("data", "", "data dir")
	fs.Parse(args)
	c, err := loadConfigFile(cfgPath)
	fresh := errors.Is(err, os.ErrNotExist)
	if err != nil && !fresh {
		return err
	}
	if *port > 0 {
		c.Listen = fmt.Sprintf(":%d", *port)
		if c.PublicURL != "" {
			if u, err := url.Parse(c.PublicURL); err == nil {
				u.Host = net.JoinHostPort(u.Hostname(), fmt.Sprint(*port))
				c.PublicURL = u.String()
			}
		}
	}
	if *pub != "" {
		c.PublicURL = strings.TrimRight(*pub, "/")
	}
	if *data != "" {
		c.DataDir = *data
	}
	pw := ""
	if fresh || c.PasswordHash == "" {
		pw = randPassword()
		c.PasswordHash = hashPassword(pw)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	if err := saveConfigFile(cfgPath, c); err != nil {
		return err
	}
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return err
	}
	if _, err := LoadOrCreateCA(c.DataDir); err != nil {
		return err
	}
	fmt.Println("config:", cfgPath)
	fmt.Println("listen:", c.Listen)
	if pw != "" {
		fmt.Println("admin user:", c.AdminUser)
		fmt.Println("admin password:", pw)
	}
	return nil
}

func resetPassword() (string, error) {
	c, err := loadConfigFile(cfgPath)
	if err != nil {
		return "", err
	}
	pw := randPassword()
	c.PasswordHash = hashPassword(pw)
	return pw, saveConfigFile(cfgPath, c)
}

// ctl calls the running service over its unix socket; ok=false if it is not running.
func ctl(path string, q url.Values, out any) (bool, error) {
	c, err := loadConfigFile(cfgPath)
	if err != nil {
		return false, err
	}
	sock := controlSock(c.DataDir)
	cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := cl.Get("http://ctl" + path + "?" + q.Encode())
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var e struct{ Error string }
	json.Unmarshal(b, &e)
	if e.Error != "" {
		return true, errors.New(e.Error)
	}
	if out != nil {
		json.Unmarshal(b, out)
	}
	return true, nil
}

// local opens the store directly when the service is not running.
func local() (*Store, Config, error) {
	c, err := loadConfigFile(cfgPath)
	if err != nil {
		return nil, c, err
	}
	st, err := OpenStore(c.DataDir, true)
	return st, c, err
}

func cmdStatus() error {
	var s map[string]any
	ok, err := ctl("/status", nil, &s)
	if err != nil {
		return err
	}
	if !ok {
		store, c, err := local()
		if err != nil {
			return err
		}
		st := store.Snapshot()
		fmt.Printf("service: not running\npublic url: %s\napps: %d  repos: %d\nlast sync ok: %s\nlast import: %s\n",
			c.PublicURL, len(st.Apps), len(st.Repos), fmtTime(st.Sync.LastOK), fmtTime(st.Sync.LastImport))
		return nil
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(b))
	return nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func cmdSync(wait bool) error {
	q := url.Values{}
	if wait {
		q.Set("wait", "1")
	}
	ok, err := ctl("/sync", q, nil)
	if ok || err != nil {
		if err == nil {
			fmt.Println("sync started in the running service (see panel / logs)")
		}
		return err
	}
	store, c, err := local()
	if err != nil {
		return err
	}
	return NewSyncer(store, func() Config { return c }, c.DataDir).Run(context.Background(), nil)
}

func cmdExport(out string) error {
	out, _ = filepath.Abs(out)
	ok, err := ctl("/export", url.Values{"path": {out}}, nil)
	if !ok && err == nil {
		var store *Store
		var c Config
		if store, c, err = local(); err == nil {
			_, err = ExportBundleFile(out, c.DataDir, store)
		}
	}
	if err == nil {
		fmt.Println("bundle written:", out)
	}
	return err
}

func cmdImport(in string) error {
	in, _ = filepath.Abs(in)
	ok, err := ctl("/import", url.Values{"path": {in}}, nil)
	if !ok && err == nil {
		var store *Store
		var c Config
		if store, c, err = local(); err == nil {
			_, err = ImportBundle(in, c.DataDir, store)
		}
	}
	if err == nil {
		fmt.Println("imported:", in)
	}
	return err
}

// cmdCollect runs on an internet-connected machine and produces a bundle.
func cmdCollect(args []string) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	out := fs.String("out", "sahand-mirror-bundle.tar.gz", "bundle file")
	data := fs.String("data", "./sahand-mirror-collect", "working data dir (kept for incremental runs)")
	apps := fs.String("apps", "", "apps.json exported from a hub (default: hub config's apps or the built-in list)")
	keep := fs.Int("keep", 0, "releases per repo (default from config or 3)")
	token := fs.String("token", os.Getenv("GITHUB_TOKEN"), "GitHub token (optional)")
	fs.Parse(args)
	c, err := loadConfigFile(cfgPath)
	if err != nil {
		c = defaultConfig()
	}
	c.DataDir, _ = filepath.Abs(*data)
	if *keep > 0 {
		c.Keep = *keep
	}
	if *token != "" {
		c.GitHubToken = *token
	}
	store, err := OpenStore(c.DataDir, *apps == "")
	if err != nil {
		return err
	}
	if *apps != "" {
		b, err := os.ReadFile(*apps)
		if err != nil {
			return err
		}
		var list []*App
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("%s: %w", *apps, err)
		}
		store.Update(func(st *State) {
			for _, a := range list {
				if a.ID == "" || st.App(a.ID) != nil {
					continue
				}
				st.Apps = append(st.Apps, a)
			}
		})
	}
	sy := NewSyncer(store, func() Config { return c }, c.DataDir)
	start := time.Now()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		for range time.Tick(5 * time.Second) {
			store.View(func(st *State) {
				if st.Sync.Progress != "" {
					fmt.Fprintf(os.Stderr, "  %s\n", st.Sync.Progress)
				}
			})
		}
	}()
	if err := sy.Run(ctx, nil); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	m, err := ExportBundleFile(*out, c.DataDir, store)
	if err != nil {
		return err
	}
	fmt.Printf("bundle %s: %d files, %d apps, %s\n", *out, len(m.Files), len(m.Apps), time.Since(start).Round(time.Second))
	return nil
}
