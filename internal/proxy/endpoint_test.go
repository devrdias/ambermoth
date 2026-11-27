package proxy_test

import (
	"testing"
)

type endpointCase struct {
	name string
	path string
	want string
}

// The endpoint class is a substring match on the lowercased path, first hit
// in a fixed order. These cases pin that order and its looseness.
func TestEndpointClass(t *testing.T) {
	cases := []endpointCase{
		{"chat completions", "/v1/chat/completions", "chat.completions"},
		{"chat completions trailing slash", "/v1/chat/completions/", "chat.completions"},
		{"chat completions item", "/v1/chat/completions/chatcmpl-1/messages", "chat.completions"},
		{"chat completions upper case", "/V1/Chat/Completions", "chat.completions"},
		{"chat completions without version", "/chat/completions", "chat.completions"},
		{"chat completions other version", "/v2/chat/completions", "chat.completions"},
		{"chat completions azure style", "/openai/deployments/d1/chat/completions", "chat.completions"},
		{"chat completions gemini compat", "/v1beta/openai/chat/completions", "chat.completions"},
		{"legacy completions", "/v1/completions", "completions"},
		{"legacy completions engines", "/v1/engines/davinci/completions", "completions"},
		{"responses", "/v1/responses", "responses"},
		{"responses item", "/v1/responses/resp_1", "responses"},
		{"responses input items", "/v1/responses/resp_1/input_items", "responses"},
		{"embeddings", "/v1/embeddings", "embeddings"},
		{"embeddings upper case", "/v1/EMBEDDINGS", "embeddings"},
		{"embeddings suffix text", "/v1/embeddings-v2", "embeddings"},
		{"messages", "/v1/messages", "messages"},
		{"messages trailing slash", "/v1/messages/", "messages"},
		{"messages of a thread", "/v1/threads/t1/messages", "messages"},
		{"messages count tokens", "/v1/messages/count_tokens", "messages.count_tokens"},
		{"messages count tokens upper case", "/v1/Messages/Count_Tokens", "messages.count_tokens"},
		{"batches", "/v1/batches", "batches"},
		{"batch item", "/v1/batches/batch_1/cancel", "batches"},
		{"message batches beat messages", "/v1/messages/batches", "batches"},
		{"message batch results", "/v1/messages/batches/msgbatch_1/results", "batches"},
		{"files", "/v1/files", "files"},
		{"file content", "/v1/files/file-1/content", "files"},
		{"models", "/v1/models", "models"},
		{"model item", "/v1/models/gpt-4o", "models"},
		{"models suffix text", "/v1/modelsx", "models"},
		{"gemini generateContent counts as models", "/v1beta/models/gemini-pro:generateContent", "models"},
		{"realtime", "/v1/realtime", "realtime"},
		{"realtime sessions", "/v1/realtime/sessions", "realtime"},
		{"realtime beats chat completions", "/v1/realtime/chat/completions", "realtime"},
		{"audio is other", "/v1/audio/transcriptions", "other"},
		{"images is other", "/v1/images/generations", "other"},
		{"fine tuning is other", "/v1/fine_tuning/jobs", "other"},
		{"root is other", "/", "other"},
		{"bare version is other", "/v1", "other"},
		{"chat without completions is other", "/v1/chat", "other"},
		{"singular completion is other", "/v1/completion", "other"},
		{"segment needs a leading slash", "/v1/xmessages", "other"},
	}
	styles := []string{"openai", "anthropic"}
	for _, style := range styles {
		r := newRig(t, jsonUpstream(), nil)
		for _, tc := range cases {
			if status, _, err := r.send("GET", "/"+style+tc.path, "", nil); status != 200 || err != nil {
				t.Fatalf("%s %s: status %d err %v", style, tc.name, status, err)
			}
		}
		es := r.entries()
		if len(es) != len(cases) {
			t.Fatalf("%s: want %d entries, got %d", style, len(cases), len(es))
		}
		for i, tc := range cases {
			t.Run(style+"/"+tc.name, func(t *testing.T) {
				if got := es[i].Str("endpoint"); got != tc.want {
					t.Errorf("endpoint of %s = %q, want %q", tc.path, got, tc.want)
				}
				if got := es[i].Str("api_style"); got != style {
					t.Errorf("api_style = %q, want %q", got, style)
				}
			})
		}
	}
}

// The query string is not part of the path used for classification or stored.
func TestEndpointClassIgnoresQuery(t *testing.T) {
	r := newRig(t, jsonUpstream(), nil)
	if _, _, err := r.send("GET", "/openai/v1/models?filter=/chat/completions", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.send("POST", "/openai/v1/embeddings?x=1", `{}`, nil); err != nil {
		t.Fatal(err)
	}
	es := r.entries()
	if len(es) != 2 {
		t.Fatalf("want 2 entries, got %d", len(es))
	}
	if es[0].Str("endpoint") != "models" || es[0].Str("path") != "/v1/models" {
		t.Errorf("query leaked into the first entry: %v", es[0])
	}
	if es[1].Str("endpoint") != "embeddings" || es[1].Str("path") != "/v1/embeddings" || es[1].Str("method") != "POST" {
		t.Errorf("second entry wrong: %v", es[1])
	}
}
