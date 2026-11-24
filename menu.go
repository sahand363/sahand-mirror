package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	binPath  = "/usr/local/bin/sahand-mirror"
	unitPath = "/etc/systemd/system/sahand-mirror.service"
	selfRepo = "sahand363/sahand-mirror"
)

var in = bufio.NewReader(os.Stdin)

func ask(prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	s, _ := in.ReadString('\n')
	if s = strings.TrimSpace(s); s == "" {
		return def
	}
	return s
}

func run(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func panelAddress(c Config) string {
	if c.PublicURL != "" {
		return c.PublicURL
	}
	_, port, _ := net.SplitHostPort(c.Listen)
	return "http://<server-ip>:" + port
}

var menuItems = []struct{ key, label string }{
	{"1", "Status"},
	{"2", "Panel address / login"},
	{"3", "Reset password"},
	{"4", "Change port"},
	{"5", "Sync now"},
	{"6", "Export bundle"},
	{"7", "Import bundle"},
	{"8", "Logs"},
	{"9", "Update"},
	{"10", "Uninstall"},
	{"11", "apt proxy command for other servers"},
	{"0", "Exit"},
}

func clearScreen() { fmt.Print("\033[H\033[2J\033[3J") }

func pause() {
	fmt.Print("\n\033[2mPress Enter to return to the menu...\033[0m")
	in.ReadString('\n')
}

func drawMenu(c Config, cfgErr error) {
	clearScreen()
	line := strings.Repeat("─", 46)
	fmt.Printf("\033[1;36m%s\033[0m\n", line)
	fmt.Printf("  \033[1;36msahand-mirror\033[0m  \033[2m%s\033[0m\n", version)
	if cfgErr == nil {
		fmt.Printf("  panel: \033[1m%s\033[0m\n  user:  %s\n", panelAddress(c), c.AdminUser)
	} else {
		fmt.Println("  \033[33mconfig missing — run install.sh\033[0m")
	}
	fmt.Printf("\033[1;36m%s\033[0m\n\n", line)
	for _, it := range menuItems {
		fmt.Printf("  \033[1;33m%3s)\033[0m  %s\n", it.key, it.label)
	}
	fmt.Println()
}

func runMenu() {
	for {
		c, err := loadConfigFile(cfgPath)
		drawMenu(c, err)
		choice := ask("Choice", "0")
		if choice != "0" && choice != "q" {
			clearScreen()
		}
		var e error
		switch choice {
		case "1":
			run("systemctl", "--no-pager", "status", "sahand-mirror")
			e = cmdStatus()
		case "2":
			fmt.Printf("panel: %s\nuser:  %s\npassword is stored hashed; use 3 to set a new one.\n", panelAddress(c), c.AdminUser)
			fmt.Printf("install one-liner example: bash <(curl -Ls %s/i/<app>)\n", panelAddress(c))
		case "3":
			var pw string
			if pw, e = resetPassword(); e == nil {
				fmt.Printf("new password: \033[1;32m%s\033[0m\n", pw)
			}
		case "4":
			p := ask("new port", strings.TrimPrefix(c.Listen, ":"))
			if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
				e = errors.New("invalid port")
			} else if e = cmdInit([]string{"--port", p}); e == nil {
				e = run("systemctl", "restart", "sahand-mirror")
				c, _ = loadConfigFile(cfgPath)
				fmt.Println("panel:", panelAddress(c))
			}
		case "5":
			e = cmdSync(false)
		case "6":
			e = cmdExport(ask("output file", "/root/sahand-mirror-bundle-"+time.Now().Format("20060102")+".tar.gz"))
		case "7":
			e = cmdImport(ask("bundle file", ""))
		case "8":
			run("journalctl", "-u", "sahand-mirror", "-n", "200", "--no-pager")
		case "9":
			e = menuUpdate(c)
		case "10":
			if menuUninstall() {
				return
			}
		case "11":
			hub := panelAddress(c)
			p := aptProxyURL(hub, c.AccessKey)
			if p == "" {
				p = hub + "/  (hub is https: point sources.list at " + hub + "/apt/<host>/<path> instead)"
			}
			fmt.Printf("run on the other server (as root):\n\n  echo 'Acquire::http::Proxy \"%s\";' > /etc/apt/apt.conf.d/99sahand-mirror && apt-get update\n\nundo (back to normal):\n  rm -f /etc/apt/apt.conf.d/99sahand-mirror && apt-get update\n", p)
			fmt.Println("only http:// repos are cached; https repos (docker, cloudsmith...) still go direct and fail offline.")
		case "0", "q":
			return
		}
		if e != nil {
			fmt.Println("\033[31merror:\033[0m", e)
		}
		if choice != "0" && choice != "q" {
			pause()
		}
	}
}

func menuUpdate(c Config) error {
	fmt.Println(" 1) from GitHub (needs international internet)\n 2) from a sahand-mirror hub (e.g. another internal hub)")
	base := "https://github.com/" + selfRepo
	if ask("source", "1") == "2" {
		hub := strings.TrimRight(ask("hub address (http://IP:PORT)", c.PublicURL), "/")
		base = hub + "/gh/web/" + selfRepo
	}
	name := "sahand-mirror-linux-" + selfArch() + ".tar.gz"
	u := base + "/releases/latest/download/" + name
	fmt.Println("downloading", u)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	get := func(u string) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		if c.AccessKey != "" {
			req.Header.Set("X-Mirror-Key", c.AccessKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	}
	tgz, err := get(u)
	if err != nil {
		return err
	}
	if sum, err := get(u + ".sha256"); err == nil {
		h := sha256.Sum256(tgz)
		if f := strings.Fields(string(sum)); len(f) == 0 || f[0] != hex.EncodeToString(h[:]) {
			return errors.New("sha256 mismatch")
		}
		fmt.Println("sha256 verified")
	}
	bin, err := binaryFromTarGz(tgz)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(binPath, bin, 0o755); err != nil {
		return err
	}
	fmt.Println("binary replaced, restarting service")
	return run("systemctl", "restart", "sahand-mirror")
}

func binaryFromTarGz(b []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New("sahand-mirror binary not found in archive")
		}
		if filepath.Base(h.Name) == "sahand-mirror" && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
}

func menuUninstall() bool {
	if strings.ToLower(ask("uninstall sahand-mirror? (yes/no)", "no")) != "yes" {
		return false
	}
	c, _ := loadConfigFile(cfgPath)
	run("systemctl", "disable", "--now", "sahand-mirror")
	os.Remove(unitPath)
	run("systemctl", "daemon-reload")
	if strings.ToLower(ask("also delete mirrored data "+c.DataDir+" and config? (yes/no)", "no")) == "yes" {
		os.RemoveAll(c.DataDir)
		os.RemoveAll(filepath.Dir(cfgPath))
	}
	os.Remove(binPath)
	fmt.Println("uninstalled.")
	return true
}
