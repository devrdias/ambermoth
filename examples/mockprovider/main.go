// Command mockprovider is a fake OpenAI/Anthropic API for demos and local
// testing: it returns canned responses with realistic usage fields, streaming
// or not. It never calls a real provider.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:9999", "listen address")
	flag.Parse()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			log.Printf("request body is not JSON: %v", err)
		}
		in := 20 + len(body)/4
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages") && req.Stream:
			w.Header().Set("Content-Type", "text/event-stream")
			events := []string{
				fmt.Sprintf(`{"type":"message_start","message":{"model":%q,"usage":{"input_tokens":%d,"output_tokens":1}}}`, req.Model, in),
				`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`,
				`{"type":"message_delta","usage":{"output_tokens":12}}`,
				`{"type":"message_stop"}`,
			}
			for _, ev := range events {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", ev)
				w.(http.Flusher).Flush()
				time.Sleep(5 * time.Millisecond)
			}
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"model":%q,"content":[{"type":"text","text":"Hello"}],"usage":{"input_tokens":%d,"output_tokens":12}}`, req.Model, in)
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"model":%q,"data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":%d,"total_tokens":%d}}`, req.Model, in, in)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"role":"assistant","content":"Hello"}}],"usage":{"prompt_tokens":%d,"completion_tokens":12}}`, req.Model, in)
		}
	})
	log.Printf("mock provider on http://%s (fake responses only)", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
