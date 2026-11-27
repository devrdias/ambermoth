Set up ambermoth on this machine, so LLM API calls made from it are recorded.

ambermoth is a local proxy that records every LLM API call as a hash-only entry in a
tamper-evident ledger, so the usage can be explained later. It passes traffic through
unchanged. The ledger directory is {{.Dir}}; if that path is relative, put it under my
home directory and use the absolute path everywhere below.

1. Run `ambermoth version`. If it isn't installed, get it from
   https://github.com/devrdias/ambermoth/releases/latest (check the SHA-256 checksum), or
   run `go install github.com/devrdias/ambermoth/cmd/ambermoth@latest`.

2. If {{.Dir}}/ledger.jsonl does not exist, run `ambermoth init --dir {{.Dir}}`. Never
   delete, edit or recreate files in {{.Dir}}: ledger.jsonl is append-only and secret.key
   is the private HMAC key. Tell me where secret.key is so I can back it up.

3. Run `ambermoth proxy --dir {{.Dir}}` as a service for my user, not as root: a launchd
   agent on macOS, a systemd user unit on Linux, a Task Scheduler task at logon on
   Windows. It should start at login and restart if it exits.

4. In my shell profile, set:

     AMBERMOTH_DIR=<the ledger directory>
     OPENAI_BASE_URL=http://127.0.0.1:8787/openai/v1
     ANTHROPIC_BASE_URL=http://127.0.0.1:8787/anthropic

   Gemini goes through http://127.0.0.1:8787/gemini/v1beta/openai, its
   OpenAI-compatible endpoint; there is no standard variable for it.

5. Set "anchor_interval": "1h" in {{.Dir}}/config.json unless I ask for something else.

6. Calls that don't send an X-Provenance-Purpose header get the "default_purpose" from
   config.json, which starts as untagged. Ask me whether to change it, and don't choose
   one yourself; the tag is part of the evidence. Valid tags:

     {{.Purposes}}

   or a custom tag starting with x-.

7. Any program that reads these variables now goes through the proxy, including coding
   agents. List the ones you find, and ask before changing any program's own settings.

8. Check it: open a new shell, make one real call, then run
   `ambermoth report --dir {{.Dir}}` and `ambermoth verify --dir {{.Dir}}`.

Show me each file you change outside {{.Dir}} before you change it.
