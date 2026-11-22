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

func runMenu() {
	for {
		c, err := loadConfigFile(cfgPath)
		fmt.Println("\n\033[1;36m══ sahand-mirror " + version + " ══\033[0m")
		if err == nil {
			fmt.Printf(" panel: \033[1m%s\033[0m   user: %s\n", panelAddress(c), c.AdminUser)
		} else {
			fmt.Println(" config missing — run install.sh")
		}
		fmt.Print(` 1) status            2) panel address / login
 3) reset password    4) change port
 5) sync now          6) export bundle
 7) import bundle     8) logs
 9) update            10) uninstall
 0) exit
`)
		var e error
		switch ask("choice", "0") {
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
		case "0", "q":
			return
		}
		if e != nil {
			fmt.Println("\033[31merror:\033[0m", e)
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
