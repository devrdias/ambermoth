package proxy_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

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

// What LoadConfig accepts, and how it fills and normalises the config.
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

// LoadConfig rejects a bad base URL, and New still refuses one it is handed
// directly.
func TestNewRejectsAnUnparsableBaseURL(t *testing.T) {
	c := proxy.DefaultConfig()
	c.Upstreams = map[string]proxy.Upstream{"x": {BaseURL: "://bad", Style: "openai"}}
	r := newRig(t, jsonUpstream(), nil)
	if _, err := proxy.New(c, r.store, r.w, nil, discard); err == nil {
		t.Fatal("New accepted an unparsable base URL")
	}
}

type valueCase struct {
	name    string
	content string
	wantErr string // substring; empty = must load
}

func runValueCases(t *testing.T, cases []valueCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := proxy.LoadConfig(writeConfig(t, tc.content))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("a valid config was rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing\n%s", err, tc.wantErr)
			}
		})
	}
}

func TestLoadConfigChecksListen(t *testing.T) {
	const want = "want host:port with a port from 1 to 65535"
	listen := func(v string) string { return `{"listen":"` + v + `"}` }
	bad := func(name, v string) valueCase {
		return valueCase{name, listen(v), `listen "` + v + `": ` + want}
	}
	runValueCases(t, []valueCase{
		{"loopback", listen("127.0.0.1:8787"), ""},
		{"all interfaces", listen("0.0.0.0:9090"), ""},
		{"empty host", listen(":8787"), ""},
		{"host name", listen("localhost:8787"), ""},
		{"ipv6", listen("[::1]:8787"), ""},
		{"lowest port", listen("127.0.0.1:1"), ""},
		{"highest port", listen("127.0.0.1:65535"), ""},
		bad("empty", ""),
		bad("not an address", "not an address"),
		bad("no port", "127.0.0.1"),
		bad("empty port", "127.0.0.1:"),
		bad("port zero", "127.0.0.1:0"),
		bad("port too high", "127.0.0.1:65536"),
		bad("negative port", "127.0.0.1:-1"),
		bad("service name", "127.0.0.1:http"),
		bad("port with text", "127.0.0.1:80x"),
		bad("signed port", "127.0.0.1:+80"),
		bad("ipv6 without brackets", "::1:8787"),
		bad("ipv6 without port", "[::1]"),
		{"listen of the wrong type", `{"listen":5}`, "cannot unmarshal number"},
	})
}

func TestLoadConfigChecksBaseURLs(t *testing.T) {
	const want = "want an absolute http or https URL with a host, without a query or fragment"
	up := func(v string) string {
		return `{"upstreams":{"x":{"base_url":"` + v + `","style":"openai"}}}`
	}
	bad := func(name, v string) valueCase {
		return valueCase{name, up(v), `upstreams["x"].base_url "` + v + `": ` + want}
	}
	runValueCases(t, []valueCase{
		{"https", up("https://api.example.com"), ""},
		{"trailing slash", up("https://api.example.com/"), ""},
		{"http with port and path", up("http://127.0.0.1:11434/v1/"), ""},
		{"upper case scheme", up("HTTPS://api.example.com"), ""},
		bad("empty", ""),
		bad("unparsable", "://bad url"),
		bad("no scheme", "api.example.com"),
		bad("scheme relative", "//api.example.com"),
		bad("path only", "/v1"),
		bad("ftp", "ftp://api.example.com"),
		bad("file", "file:///etc/passwd"),
		bad("no host", "https://"),
		bad("no host with path", "https:///v1"),
		bad("query", "https://api.example.com?a=1"),
		bad("fragment", "https://api.example.com/#top"),
		{"every upstream is checked", `{"upstreams":{"a":{"base_url":"https://a.test","style":"openai"},"b":{"base_url":"nope","style":"openai"}}}`,
			`upstreams["b"].base_url "nope"`},
		{"a default upstream replaced with a bad URL", `{"upstreams":{"openai":{"base_url":"api.openai.com","style":"openai"}}}`,
			`upstreams["openai"].base_url "api.openai.com"`},
	})
}

func TestLoadConfigChecksUpstreamNames(t *testing.T) {
	up := func(name string) string {
		return `{"upstreams":{"` + name + `":{"base_url":"https://x.test","style":"openai"}}}`
	}
	const why = `name must not be empty, contain "/" or be "_ambermoth"`
	runValueCases(t, []valueCase{
		{"plain name", up("local-llm"), ""},
		{"empty name", up(""), `upstreams[""]: ` + why},
		{"name with a slash", up("a/b"), `upstreams["a/b"]: ` + why},
		{"name of the health endpoint", up("_ambermoth"), `upstreams["_ambermoth"]: ` + why},
	})
}

func TestLoadConfigChecksTSAs(t *testing.T) {
	const want = "want an absolute http or https URL with a host, without a query or fragment"
	runValueCases(t, []valueCase{
		{"none", `{"tsas":[]}`, ""},
		{"null", `{"tsas":null}`, ""},
		{"two", `{"tsas":["https://a.test/tsr","http://b.test/tsr"]}`, ""},
		{"no scheme", `{"tsas":["freetsa.org/tsr"]}`, `tsas[0] "freetsa.org/tsr": ` + want},
		{"empty entry", `{"tsas":["https://a.test","" ]}`, `tsas[1] "": ` + want},
		{"ftp", `{"tsas":["ftp://a.test"]}`, `tsas[0] "ftp://a.test"`},
	})
}

func TestLoadConfigChecksAnchorInterval(t *testing.T) {
	const want = "want a positive duration such as 1h, or empty to turn anchoring off"
	iv := func(v string) string { return `{"anchor_interval":"` + v + `"}` }
	bad := func(name, v string) valueCase {
		return valueCase{name, iv(v), `anchor_interval "` + v + `": ` + want}
	}
	runValueCases(t, []valueCase{
		{"empty means off", iv(""), ""},
		{"milliseconds", iv("20ms"), ""},
		{"one second", iv("1s"), ""},
		{"hours", iv("1h"), ""},
		{"combined", iv("1h30m"), ""},
		{"seconds past a minute", iv("90s"), ""},
		bad("zero", "0"),
		bad("zero seconds", "0s"),
		bad("negative", "-1h"),
		bad("negative milliseconds", "-20ms"),
		bad("not a duration", "soon"),
		bad("no unit", "30"),
		bad("overflow", "99999999h"),
		{"wrong type", `{"anchor_interval":3600}`, "cannot unmarshal number"},
	})
}

func TestAnchorEvery(t *testing.T) {
	cases := map[string]time.Duration{"": 0, "1s": time.Second, "1h30m": 90 * time.Minute}
	for in, want := range cases {
		c := proxy.Config{AnchorInterval: in}
		if got := c.AnchorEvery(); got != want {
			t.Errorf("AnchorEvery(%q) = %v, want %v", in, got, want)
		}
	}
}

// The file that `ambermoth init` writes must load and give back the defaults.
func TestDefaultConfigFileLoads(t *testing.T) {
	got, err := proxy.LoadConfig(writeConfig(t, string(proxy.MarshalConfig(proxy.DefaultConfig()))))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, proxy.DefaultConfig()) {
		t.Errorf("got %+v, want the defaults", got)
	}
}

var jsonBlockRE = regexp.MustCompile("(?s)```json\n(.*?)```")

// Every JSON block in the README and the spec that looks like a config must
// load, so a documented example never rots.
func TestDocumentedConfigExamplesLoad(t *testing.T) {
	keys := []string{"listen", "upstreams", "default_purpose", "key_defaults", "routes", "auto_enroll", "anchor_interval", "tsas"}
	files := []string{"../../README.md", "../../docs/SPEC.md"}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range jsonBlockRE.FindAllStringSubmatch(string(b), -1) {
			var obj map[string]json.RawMessage
			if json.Unmarshal([]byte(m[1]), &obj) != nil || !hasAny(obj, keys) {
				continue
			}
			t.Run(fmt.Sprintf("%s block %d", filepath.Base(file), i), func(t *testing.T) {
				if _, err := proxy.LoadConfig(writeConfig(t, m[1])); err != nil {
					t.Errorf("documented example does not load: %v", err)
				}
			})
		}
	}
}

func hasAny(obj map[string]json.RawMessage, keys []string) bool {
	for _, k := range keys {
		if _, ok := obj[k]; ok {
			return true
		}
	}
	return false
}
