package proxy_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/proxy"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigMissingFileGivesDefaults(t *testing.T) {
	got, err := proxy.LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, proxy.DefaultConfig()) {
		t.Errorf("got %+v, want the defaults", got)
	}
}

func TestLoadConfigUnreadablePath(t *testing.T) {
	if _, err := proxy.LoadConfig(t.TempDir()); err == nil {
		t.Fatal("a directory in place of the file should be an error")
	}
}

func TestLoadConfigDecodeErrorNamesTheFile(t *testing.T) {
	path := writeConfig(t, `{"listen":`)
	if _, err := proxy.LoadConfig(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want one naming %s", err, path)
	}
}

type configErrCase struct {
	name    string
	content string
	want    string
}

func TestLoadConfigErrors(t *testing.T) {
	cases := []configErrCase{
		{"empty file", ``, "unexpected end of JSON input"},
		{"malformed json", `{"listen":`, "unexpected end of JSON input"},
		{"not json", `listen: 1`, "invalid character"},
		{"trailing text", `{} x`, "invalid character"},
		{"wrong type for a field", `{"listen": 5}`, "cannot unmarshal number"},
		{"wrong type for a list", `{"tsas": "https://x"}`, "cannot unmarshal string"},
		{"unknown upstream style", `{"upstreams":{"x":{"base_url":"https://x.test","style":"gemini"}}}`, `upstream "x": style must be openai or anthropic`},
		{"empty upstream style", `{"upstreams":{"x":{"base_url":"https://x.test"}}}`, `upstream "x": style must be openai or anthropic`},
		{"style is case sensitive", `{"upstreams":{"x":{"base_url":"https://x.test","style":"OpenAI"}}}`, `upstream "x"`},
		{"anchor interval not a duration", `{"anchor_interval":"soon"}`, "anchor_interval"},
		{"anchor interval without a unit", `{"anchor_interval":"30"}`, "anchor_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.content)
			_, err := proxy.LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

type configLoadCase struct {
	name    string
	content string
	check   func(t *testing.T, c proxy.Config)
}

// LoadConfig checks only the upstream styles and anchor_interval. Everything
// else is accepted as written, including values that cannot work.
func TestLoadConfigAccepts(t *testing.T) {
	cases := []configLoadCase{
		{"empty object keeps the defaults", `{}`, func(t *testing.T, c proxy.Config) {
			if !reflect.DeepEqual(c, proxy.DefaultConfig()) {
				t.Errorf("got %+v", c)
			}
		}},
		{"unknown keys are ignored", `{"removed_key":{"a":1},"listen":"127.0.0.1:9000","retention_days":7}`, func(t *testing.T, c proxy.Config) {
			if c.Listen != "127.0.0.1:9000" {
				t.Errorf("listen = %q", c.Listen)
			}
		}},
		{"base url trailing slashes are trimmed", `{"upstreams":{"x":{"base_url":"https://x.test//","style":"openai"}}}`, func(t *testing.T, c proxy.Config) {
			if got := c.Upstreams["x"].BaseURL; got != "https://x.test" {
				t.Errorf("base_url = %q", got)
			}
		}},
		{"base url is not validated", `{"upstreams":{"x":{"base_url":"://bad url","style":"openai"},"y":{"base_url":"","style":"anthropic"}}}`, func(t *testing.T, c proxy.Config) {
			if c.Upstreams["x"].BaseURL != "://bad url" || c.Upstreams["y"].BaseURL != "" {
				t.Errorf("upstreams = %+v", c.Upstreams)
			}
		}},
		{"listen address is not validated", `{"listen":"not an address"}`, func(t *testing.T, c proxy.Config) {
			if c.Listen != "not an address" {
				t.Errorf("listen = %q", c.Listen)
			}
		}},
		{"anchor interval accepts any duration", `{"anchor_interval":"-1h"}`, func(t *testing.T, c proxy.Config) {
			if c.AnchorInterval != "-1h" {
				t.Errorf("anchor_interval = %q", c.AnchorInterval)
			}
		}},
		{"upstreams in the file are added to the default ones", `{"upstreams":{"x":{"base_url":"https://x.test","style":"openai"}}}`, func(t *testing.T, c proxy.Config) {
			if len(c.Upstreams) != 4 || c.Upstreams["x"].BaseURL != "https://x.test" || c.Upstreams["openai"].Style != "openai" {
				t.Errorf("upstreams = %+v", c.Upstreams)
			}
		}},
		{"an upstream in the file replaces the default of that name", `{"upstreams":{"openai":{"base_url":"https://o.test/","style":"anthropic"}}}`, func(t *testing.T, c proxy.Config) {
			if got := c.Upstreams["openai"]; got.BaseURL != "https://o.test" || got.Style != "anthropic" {
				t.Errorf("openai = %+v", got)
			}
		}},
		{"lists in the file replace the default lists", `{"tsas":["https://t.test"],"routes":[]}`, func(t *testing.T, c proxy.Config) {
			if !reflect.DeepEqual(c.TSAs, []string{"https://t.test"}) || len(c.Routes) != 0 {
				t.Errorf("tsas = %v routes = %v", c.TSAs, c.Routes)
			}
		}},
		{"auto_enroll can be turned off", `{"auto_enroll":false}`, func(t *testing.T, c proxy.Config) {
			if c.AutoEnroll {
				t.Error("auto_enroll should be false")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := proxy.LoadConfig(writeConfig(t, tc.content))
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, c)
		})
	}
}

func TestLoadConfigFullFile(t *testing.T) {
	const file = `{
  "listen": "0.0.0.0:9090",
  "upstreams": {
    "openai": {"base_url": "https://api.openai.com/", "style": "openai"},
    "anthropic": {"base_url": "https://api.anthropic.com", "style": "anthropic"},
    "gemini": {"base_url": "https://generativelanguage.googleapis.com", "style": "openai"},
    "local": {"base_url": "http://127.0.0.1:11434/v1/", "style": "openai"}
  },
  "default_purpose": "dev-experiment",
  "key_defaults": {"sha256:abc": "production-inference"},
  "routes": [
    {"prefix": "/v1/embeddings", "purpose": "rag-indexing"},
    {"upstream": "anthropic", "prefix": "/v1/messages/batches", "purpose": "batch-job"}
  ],
  "auto_enroll": false,
  "anchor_interval": "1h30m",
  "tsas": ["https://tsa.example/tsr"]
}`
	want := proxy.Config{
		Listen: "0.0.0.0:9090",
		Upstreams: map[string]proxy.Upstream{
			"openai":    {BaseURL: "https://api.openai.com", Style: "openai"},
			"anthropic": {BaseURL: "https://api.anthropic.com", Style: "anthropic"},
			"gemini":    {BaseURL: "https://generativelanguage.googleapis.com", Style: "openai"},
			"local":     {BaseURL: "http://127.0.0.1:11434/v1", Style: "openai"},
		},
		DefaultPurpose: "dev-experiment",
		KeyDefaults:    map[string]string{"sha256:abc": "production-inference"},
		Routes: []proxy.RouteRule{
			{Prefix: "/v1/embeddings", Purpose: "rag-indexing"},
			{Upstream: "anthropic", Prefix: "/v1/messages/batches", Purpose: "batch-job"},
		},
		AutoEnroll:     false,
		AnchorInterval: "1h30m",
		TSAs:           []string{"https://tsa.example/tsr"},
	}
	got, err := proxy.LoadConfig(writeConfig(t, file))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// The base URL is only parsed when the proxy is built.
func TestNewRejectsAnUnparsableBaseURL(t *testing.T) {
	c, err := proxy.LoadConfig(writeConfig(t, `{"upstreams":{"x":{"base_url":"://bad","style":"openai"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, jsonUpstream(), nil)
	if _, err := proxy.New(c, r.store, r.w, nil, discard); err == nil {
		t.Fatal("New accepted an unparsable base URL")
	}
}
