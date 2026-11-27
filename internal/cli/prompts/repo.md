Set up ambermoth in this repository.

ambermoth is a local proxy that records every LLM API call as a hash-only entry in a
tamper-evident ledger, so the usage can be explained later. It passes traffic through
unchanged. The ledger directory is {{.Dir}}.

1. If {{.Dir}}/ledger.jsonl does not exist, run `ambermoth init --dir {{.Dir}}`. Never
   delete, edit or recreate files in {{.Dir}}: ledger.jsonl is append-only and secret.key
   is the private HMAC key. Add {{.Dir}}/ to .gitignore.

2. Find every place the code calls an LLM API: OpenAI, Anthropic or Gemini SDKs, or raw
   HTTP to their endpoints. Make the base URL configurable, using the SDK's own
   environment variable where it has one, and don't hardcode the proxy address:

     OpenAI     OPENAI_BASE_URL=http://127.0.0.1:8787/openai/v1
     Anthropic  ANTHROPIC_BASE_URL=http://127.0.0.1:8787/anthropic
     Gemini     http://127.0.0.1:8787/gemini/v1beta/openai (its OpenAI-compatible endpoint)

   Document the variables where this repo documents its other settings.

3. List each call site with a line on what it does, and ask me what purpose it has. Don't
   choose purposes yourself; the tag is part of the evidence. Valid tags:

     {{.Purposes}}

   or a custom tag starting with x-. other-declared also needs an X-Provenance-Note
   header saying what the traffic is.

4. When I've answered, send the tag on each call in the X-Provenance-Purpose header. If
   one provider path always has one purpose, a route rule in {{.Dir}}/config.json does
   the same without code changes:

     "routes": [{"upstream": "anthropic", "prefix": "/v1/messages", "purpose": "<tag>"}]

   The proxy strips X-Provenance-* headers before forwarding.

5. Set "anchor_interval": "1h" in {{.Dir}}/config.json unless I ask for something else.

6. Check it: start `ambermoth proxy --dir {{.Dir}}`, make one real call through it, then
   run `ambermoth report --dir {{.Dir}}` and `ambermoth verify --dir {{.Dir}}`.

7. For each purpose this repo's code produces, run
   `ambermoth link --purpose <tag> --repo . --dir {{.Dir}}`. Tell me to run it again
   whenever that code changes.

Show me the diff before committing, and never commit anything from {{.Dir}}.
