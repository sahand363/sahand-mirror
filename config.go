package main

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const pbkdf2Iter = 210000

type Config struct {
	Listen        string `json:"listen"`          // e.g. ":8686"
	PublicURL     string `json:"public_url"`      // e.g. http://10.0.0.5:8686 (what target servers use)
	DataDir       string `json:"data_dir"`        // /var/lib/sahand-mirror
	DirectSync    bool   `json:"direct_sync"`     // hub syncs from GitHub itself
	SyncHour      int    `json:"sync_hour"`       // 0-23 local time, -1 = off
	Keep          int    `json:"keep"`            // newest N releases per repo
	GitHubToken   string `json:"github_token"`    // optional, write-only in UI
	AdminUser     string `json:"admin_user"`      //
	PasswordHash  string `json:"password_hash"`   // pbkdf2-sha256$iter$salt$hash
	AccessKey     string `json:"access_key"`      // optional key for public /i /gh /ext
	TLS           bool   `json:"tls"`             // serve HTTPS with an internal-CA cert
	AptMirrorHint string `json:"apt_mirror_hint"` // shown in install wrapper output
	MaxUploadMB   int64  `json:"max_upload_mb"`   // bundle upload cap

	// v0.2
	AptProxyOff bool              `json:"apt_proxy_off,omitempty"` // disable the apt caching proxy
	AptTargets  []string          `json:"apt_targets,omitempty"`   // "ubuntu/noble"...; nil = defaults
	AptUpstream map[string]string `json:"apt_upstream,omitempty"`  // family -> upstream base URL
	AptInternal map[string]string `json:"apt_internal,omitempty"`  // family -> internal mirror used when upstream fails
	TrustXFF    bool              `json:"trust_xff,omitempty"`     // /myip honours X-Forwarded-For
	AIProvider  string            `json:"ai_provider,omitempty"`   // "" | gemini | openai
	AIBaseURL   string            `json:"ai_base_url,omitempty"`   // OpenAI-compatible base, e.g. https://api.openai.com/v1
	AIModel     string            `json:"ai_model,omitempty"`
	AIKey       string            `json:"ai_key,omitempty"` // write-only in UI
}

func defaultConfig() Config {
	return Config{Listen: ":8686", DataDir: "/var/lib/sahand-mirror", DirectSync: true,
		SyncHour: 4, Keep: 3, AdminUser: "admin", MaxUploadMB: 20480}
}

func hashPassword(pw string) string {
	salt := []byte(randHex(16))
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

func checkPassword(hash, pw string) bool {
	p := strings.Split(hash, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	want, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// ConfigStore reloads the file when it changes on disk, so CLI edits
// (reset password from the menu) apply to the running service.
type ConfigStore struct {
	path  string
	mu    sync.RWMutex
	cfg   Config
	mtime time.Time
}

func loadConfigFile(path string) (Config, error) {
	c := defaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Keep < 1 {
		c.Keep = 3
	}
	if c.MaxUploadMB <= 0 {
		c.MaxUploadMB = 20480
	}
	return c, nil
}

func saveConfigFile(path string, c Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomic(path, append(b, '\n'), 0o600)
}

func NewConfigStore(path string) (*ConfigStore, error) {
	c, err := loadConfigFile(path)
	if err != nil {
		return nil, err
	}
	cs := &ConfigStore{path: path, cfg: c}
	if fi, err := os.Stat(path); err == nil {
		cs.mtime = fi.ModTime()
	}
	return cs, nil
}

func (cs *ConfigStore) Get() Config {
	if fi, err := os.Stat(cs.path); err == nil {
		cs.mu.RLock()
		changed := !fi.ModTime().Equal(cs.mtime)
		cs.mu.RUnlock()
		if changed {
			if c, err := loadConfigFile(cs.path); err == nil {
				cs.mu.Lock()
				cs.cfg, cs.mtime = c, fi.ModTime()
				cs.mu.Unlock()
			}
		}
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cfg
}

func (cs *ConfigStore) Update(fn func(*Config)) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c := cs.cfg
	fn(&c)
	if err := saveConfigFile(cs.path, c); err != nil {
		return err
	}
	cs.cfg = c
	if fi, err := os.Stat(cs.path); err == nil {
		cs.mtime = fi.ModTime()
	}
	return nil
}
