package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Upstream is a provider endpoint the proxy forwards to.
type Upstream struct {
	BaseURL string `json:"base_url"`
	Style   string `json:"style"` // "openai" or "anthropic"
}

// RouteRule assigns a purpose to calls whose upstream path starts with Prefix.
type RouteRule struct {
	Upstream string `json:"upstream,omitempty"` // empty = any
	Prefix   string `json:"prefix"`
	Purpose  string `json:"purpose"`
}

// Config is .ambermoth/config.json. It is operational configuration, not evidence.
type Config struct {
	Listen         string              `json:"listen"`
	Upstreams      map[string]Upstream `json:"upstreams"`
	DefaultPurpose string              `json:"default_purpose"`
	KeyDefaults    map[string]string   `json:"key_defaults"` // key fingerprint -> purpose
	Routes         []RouteRule         `json:"routes"`
	AutoEnroll     bool                `json:"auto_enroll"`
	AnchorInterval string              `json:"anchor_interval"` // e.g. "1h"; empty = off
	TSAs           []string            `json:"tsas"`
}

// DefaultConfig returns the configuration written by `ambermoth init`.
func DefaultConfig() Config {
	return Config{
		Listen: "127.0.0.1:8787",
		Upstreams: map[string]Upstream{
			"openai":    {BaseURL: "https://api.openai.com", Style: "openai"},
			"anthropic": {BaseURL: "https://api.anthropic.com", Style: "anthropic"},
			"gemini":    {BaseURL: "https://generativelanguage.googleapis.com", Style: "openai"},
		},
		DefaultPurpose: "untagged",
		KeyDefaults:    map[string]string{},
		Routes:         []RouteRule{},
		AutoEnroll:     true,
		AnchorInterval: "",
		TSAs:           []string{"https://freetsa.org/tsr", "http://timestamp.digicert.com"},
	}
}

// LoadConfig reads a config file, filling defaults for missing fields.
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	for name, u := range c.Upstreams {
		if u.Style != "openai" && u.Style != "anthropic" {
			return c, fmt.Errorf("upstream %q: style must be openai or anthropic", name)
		}
		u.BaseURL = strings.TrimRight(u.BaseURL, "/")
		c.Upstreams[name] = u
	}
	if c.AnchorInterval != "" {
		if _, err := time.ParseDuration(c.AnchorInterval); err != nil {
			return c, fmt.Errorf("anchor_interval: %w", err)
		}
	}
	return c, nil
}

// Taxonomy returns purpose taxonomy v1 (SPEC §5). It returns a new slice on
// each call, so callers cannot change the taxonomy.
func Taxonomy() []string {
	return []string{
		"production-inference", "eval-regression", "eval-benchmark", "rag-indexing",
		"rag-query", "batch-job", "dev-experiment", "other-declared",
	}
}

// ValidPurpose reports whether p is in the taxonomy or a custom x- tag.
func ValidPurpose(p string) bool {
	if p == "untagged" {
		return true
	}
	if strings.HasPrefix(p, "x-") && len(p) > 2 && len(p) <= 64 && utf8.ValidString(p) {
		return true
	}
	for _, t := range Taxonomy() {
		if p == t {
			return true
		}
	}
	return false
}
