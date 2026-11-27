package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const otherDeclared = "other-declared"

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
		u.BaseURL = strings.TrimRight(u.BaseURL, "/")
		c.Upstreams[name] = u
	}
	if err := c.validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// validate checks the values that would otherwise fail later or write nonsense
// to the ledger. Start-up is the place for that, never a request.
func (c Config) validate() error {
	if err := c.checkUpstreams(); err != nil {
		return err
	}
	if c.AnchorInterval != "" {
		if _, err := time.ParseDuration(c.AnchorInterval); err != nil {
			return fmt.Errorf("anchor_interval: %w", err)
		}
	}
	return c.checkPurposes()
}

func (c Config) checkUpstreams() error {
	for name, u := range c.Upstreams {
		if u.Style != "openai" && u.Style != "anthropic" {
			return fmt.Errorf("upstream %q: style must be openai or anthropic", name)
		}
	}
	return nil
}

// checkPurposes applies the header's purpose check to every purpose the config
// sets. An empty default_purpose is allowed and means untagged.
func (c Config) checkPurposes() error {
	if c.DefaultPurpose != "" {
		if err := checkConfigPurpose("default_purpose", c.DefaultPurpose); err != nil {
			return err
		}
	}
	for i, r := range c.Routes {
		if err := checkConfigPurpose(fmt.Sprintf("routes[%d].purpose", i), r.Purpose); err != nil {
			return err
		}
	}
	fps := make([]string, 0, len(c.KeyDefaults))
	for fp := range c.KeyDefaults {
		fps = append(fps, fp)
	}
	sort.Strings(fps)
	for _, fp := range fps {
		if err := checkConfigPurpose(fmt.Sprintf("key_defaults[%q]", fp), c.KeyDefaults[fp]); err != nil {
			return err
		}
	}
	return nil
}

// configTags are the taxonomy tags a config may set.
func configTags() []string {
	var tags []string
	for _, t := range Taxonomy() {
		if t != otherDeclared {
			tags = append(tags, t)
		}
	}
	return tags
}

// checkConfigPurpose rejects a config purpose that the header check would reject.
// other-declared is refused too: SPEC §5 requires a note and a config has none.
func checkConfigPurpose(field, p string) error {
	if !ValidPurpose(p) {
		return fmt.Errorf("%s %q: not a purpose tag; use one of %s, untagged or an x- tag (docs/SPEC.md §5)",
			field, p, strings.Join(configTags(), ", "))
	}
	if p == otherDeclared {
		return fmt.Errorf("%s %q: other-declared needs a note and a config has none; send it in the X-Provenance-Purpose and X-Provenance-Note headers (docs/SPEC.md §5)", field, p)
	}
	return nil
}

// Taxonomy returns purpose taxonomy v1 (SPEC §5). It returns a new slice on
// each call, so callers cannot change the taxonomy.
func Taxonomy() []string {
	return []string{
		"production-inference", "eval-regression", "eval-benchmark", "rag-indexing",
		"rag-query", "batch-job", "dev-experiment", otherDeclared,
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
