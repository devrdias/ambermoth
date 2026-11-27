package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
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
	if err := c.validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	for name, u := range c.Upstreams {
		u.BaseURL = strings.TrimRight(u.BaseURL, "/")
		c.Upstreams[name] = u
	}
	return c, nil
}

// AnchorEvery is the anchor_interval as a duration; 0 means anchoring is off.
// LoadConfig has already checked the value.
func (c Config) AnchorEvery() time.Duration {
	d, _ := time.ParseDuration(c.AnchorInterval)
	return d
}

const httpURLWant = "want an absolute http or https URL with a host, without a query or fragment"

// validate checks the values that would otherwise fail later or write nonsense
// to the ledger. Start-up is the place for that, never a request.
func (c Config) validate() error {
	checks := []func() error{c.checkListen, c.checkUpstreams, c.checkTSAs, c.checkAnchor, c.checkPurposes}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) checkListen() error {
	_, port, err := net.SplitHostPort(c.Listen)
	if n, perr := strconv.ParseUint(port, 10, 16); err != nil || perr != nil || n == 0 {
		return fmt.Errorf("listen %q: want host:port with a port from 1 to 65535", c.Listen)
	}
	return nil
}

func (c Config) checkUpstreams() error {
	names := make([]string, 0, len(c.Upstreams))
	for name := range c.Upstreams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		field := fmt.Sprintf("upstreams[%q]", name)
		u := c.Upstreams[name]
		switch {
		case name == "" || strings.Contains(name, "/") || name == "_ambermoth":
			return fmt.Errorf(`%s: name must not be empty, contain "/" or be "_ambermoth"`, field)
		case u.Style != "openai" && u.Style != "anthropic":
			return fmt.Errorf("upstream %q: style must be openai or anthropic", name)
		}
		if err := checkHTTPURL(field+".base_url", u.BaseURL); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) checkTSAs() error {
	for i, t := range c.TSAs {
		if err := checkHTTPURL(fmt.Sprintf("tsas[%d]", i), t); err != nil {
			return err
		}
	}
	return nil
}

func checkHTTPURL(field, raw string) error {
	u, err := url.Parse(raw)
	scheme := ""
	if err == nil {
		scheme = strings.ToLower(u.Scheme)
	}
	if err != nil || (scheme != "http" && scheme != "https") || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("%s %q: %s", field, raw, httpURLWant)
	}
	return nil
}

// checkAnchor accepts an empty interval, which turns anchoring off.
func (c Config) checkAnchor() error {
	if c.AnchorInterval == "" {
		return nil
	}
	if d, err := time.ParseDuration(c.AnchorInterval); err != nil || d <= 0 {
		return fmt.Errorf("anchor_interval %q: want a positive duration such as 1h, or empty to turn anchoring off", c.AnchorInterval)
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
