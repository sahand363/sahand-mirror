package main

// Optional AI second opinion on an app's offline readiness. Never required;
// works only while the hub can reach the AI provider.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const aiSystemPrompt = `You review Linux install scripts for an offline mirror hub used during an international internet shutdown in Iran.
The hub already mirrors GitHub (github.com, api.github.com, raw.githubusercontent.com, release assets), static files it was told to mirror,
caches http:// apt repositories (Ubuntu/Debian), answers IP-detection services with the client's IP, and can issue internal-CA TLS certificates.
Given the scripts and the deterministic findings already made, list ONLY additional things that will still need the outside internet or will break offline:
other package managers or repos, binaries fetched from non-GitHub hosts, DNS/NTP/time sync, telemetry or license checks, Let's Encrypt, docker images,
language package managers, URLs built from variables, interactive prompts that should be answered differently offline, and concrete suggestions.
Reply with JSON only: {"items":[{"category":"pkg|download|ssl|docker|lang|network|prompt|other","name":"short title","detail":"what and where (function or line)","suggestion":"what to do","status":"action|offline|info"}]}.
Use at most 15 items. Do not repeat the existing findings.`

type aiClient struct {
	provider, base, model, key string
	http                       *http.Client
}

func newAIClient(c Config) (*aiClient, error) {
	if c.AIProvider == "" || c.AIKey == "" {
		return nil, errors.New("هوش مصنوعی در تنظیمات فعال نشده (اختیاری است)")
	}
	a := &aiClient{provider: c.AIProvider, base: strings.TrimRight(c.AIBaseURL, "/"), model: c.AIModel, key: c.AIKey,
		http: &http.Client{Timeout: 120 * time.Second}}
	switch a.provider {
	case "gemini":
		if a.model == "" {
			a.model = "gemini-2.5-flash"
		}
		if a.base == "" {
			a.base = "https://generativelanguage.googleapis.com/v1beta"
		}
	case "openai":
		if a.base == "" {
			a.base = "https://api.openai.com/v1"
		}
		if a.model == "" {
			a.model = "gpt-4o-mini"
		}
	default:
		return nil, fmt.Errorf("unknown AI provider %q", a.provider)
	}
	return a, nil
}

func (a *aiClient) ask(ctx context.Context, system, user string) (string, error) {
	var req *http.Request
	var err error
	switch a.provider {
	case "gemini":
		body := map[string]any{
			"systemInstruction": map[string]any{"parts": []any{map[string]string{"text": system}}},
			"contents":          []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": user}}}},
			"generationConfig":  map[string]any{"responseMimeType": "application/json", "temperature": 0.2},
		}
		b, _ := json.Marshal(body)
		u := a.base + "/models/" + url.PathEscape(a.model) + ":generateContent"
		req, err = http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("x-goog-api-key", a.key)
		}
	default:
		body := map[string]any{"model": a.model, "temperature": 0.2,
			"response_format": map[string]string{"type": "json_object"},
			"messages":        []any{map[string]string{"role": "system", "content": system}, map[string]string{"role": "user", "content": user}}}
		b, _ := json.Marshal(body)
		req, err = http.NewRequestWithContext(ctx, "POST", a.base+"/chat/completions", bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+a.key)
		}
	}
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("سرویس هوش مصنوعی در دسترس نیست (فقط با اینترنت کار می‌کند): %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		msg := string(rb)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("AI HTTP %d: %s", resp.StatusCode, msg)
	}
	var text string
	if a.provider == "gemini" {
		var g struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(rb, &g); err != nil {
			return "", err
		}
		for _, c := range g.Candidates {
			for _, p := range c.Content.Parts {
				text += p.Text
			}
		}
	} else {
		var o struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(rb, &o); err != nil {
			return "", err
		}
		if len(o.Choices) > 0 {
			text = o.Choices[0].Message.Content
		}
	}
	if text == "" {
		return "", errors.New("AI returned an empty answer")
	}
	return text, nil
}

var jsonObjRe = regexp.MustCompile(`(?s)\{.*\}`)

// parseAIItems accepts {"items":[...]} or a bare array, tolerating code fences.
func parseAIItems(text string) ([]AIItem, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```")
	text = strings.TrimSuffix(strings.TrimSpace(text), "```")
	var arr []AIItem
	if strings.HasPrefix(strings.TrimSpace(text), "[") && json.Unmarshal([]byte(text), &arr) == nil {
		return cleanAI(arr), nil
	}
	var obj struct {
		Items []AIItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		if m := jsonObjRe.FindString(text); m != "" && json.Unmarshal([]byte(m), &obj) == nil {
			return cleanAI(obj.Items), nil
		}
		return nil, fmt.Errorf("AI answer is not the expected JSON: %w", err)
	}
	return cleanAI(obj.Items), nil
}

func cleanAI(in []AIItem) []AIItem {
	var out []AIItem
	cut := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if r := []rune(s); len(r) > n {
			return string(r[:n]) + "…"
		}
		return s
	}
	for _, x := range in {
		if strings.TrimSpace(x.Name) == "" {
			continue
		}
		x.Name, x.Detail, x.Suggestion, x.Category = cut(x.Name, 120), cut(x.Detail, 600), cut(x.Suggestion, 600), cut(x.Category, 30)
		if len(out) < 20 {
			out = append(out, x)
		}
	}
	return out
}

// aiReview builds the prompt from scripts + findings and asks the provider.
func aiReview(ctx context.Context, c Config, a *App, docs []ScriptDoc, items []ReadyItem) ([]AIItem, string, error) {
	cl, err := newAIClient(c)
	if err != nil {
		return nil, "", err
	}
	var f strings.Builder
	for _, it := range items {
		if it.Source == "ai" {
			continue
		}
		fmt.Fprintf(&f, "- [%s/%s] %s %s (%s)\n", it.Category, it.Status, it.PM, it.Name, it.Where)
	}
	user := fmt.Sprintf("App: %s\nInstall command: %s\nTarget archs: %v\n\nExisting deterministic findings:\n%s\nScripts:\n%s",
		a.ID, a.Command, a.Archs, f.String(), truncateScripts(docs, 60000, 150000))
	text, err := cl.ask(ctx, aiSystemPrompt, user)
	if err != nil {
		return nil, cl.model, err
	}
	out, err := parseAIItems(text)
	return out, cl.provider + "/" + cl.model, err
}
