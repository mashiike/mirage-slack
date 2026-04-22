# CLAUDE.md — maintenance hand-off

Hand-off notes for whoever (human or agent) touches this codebase next. Written for the Claude Code case but useful to any new contributor.

Start here: [`README.md`](README.md) for the user-facing story, [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the design rationale. This file is the operator's manual for making changes safely.

## The one rule that matters

**Do not add behaviour to the forward path.** mirage-slack's reason to exist is that a forwarded Slack payload behaves identically to a Slack-direct payload on the endpoint. Every tempting "small improvement" (verify signatures by default, reshape errors, inject bot tokens, rewrite URLs) erodes that property and turns the tool into a liability for the developer who is trying to debug a Slack App.

If you find yourself about to touch `proxy.go` or the forward branch of `slash.go`, ask: *does an endpoint wired directly to Slack see this change too?* If the answer is "no", push back on the request; the feature probably belongs elsewhere (e.g. a protected branch via `launch --protect`).

Additions are fine (extra headers, logging) — they don't remove information from the payload. Modifications and deletions are the ones to resist.

## How the code is organised

Flat package `mirageslack` at the repo root. There is no `internal/` subtree; everything is exported via short file names.

| File | Responsibility | Read order |
|---|---|---|
| `mirage_slack.go` | Package doc + `Version`. | 1 |
| `cli.go` | kong CLI, logging setup, `init-config` subcommand. | 2 |
| `config.go` + `native.go` | Jsonnet config loading with `env()` / `ssm()` natives. | 3 |
| `server.go` | `App` struct, ridge integration, HTTP mux. | 4 |
| `router.go` | `decideRoute` — sole source of routing truth. | 5 |
| `signing.go` | Slack v0 HMAC verification. | 6 |
| `slash.go` | Three Slack endpoints + kong subcommand dispatch + helpers. | 7 |
| `proxy.go` | `forwardRequest`, `httpClient` (compression disabled), timeouts. | 8 |
| `slack_list.go` | All direct calls to Slack Web API (`slackLists.*`, `files.list`, `files.info`, `auth.test`). | 9 |

If you need to read one file in isolation, start with `router.go` — it encodes the core invariants.

## Invariants worth remembering

- **Routing key is `channel_id`.** User ID, trigger ID, or anything else is never used to pick an endpoint.
- **One launched entry per channel.** `SlackListClient.Launch` enforces it; don't bypass by calling `updateEntry` directly.
- **DMs are not launch targets.** `ensureChannelLaunchable` rejects channel IDs starting with `D`. Add more prefixes (group DMs that start with `G` but are actually group DMs) only if you can distinguish them reliably.
- **Storage is a Slack List.** If you catch yourself designing a second store, stop. The design deliberately uses one source of truth.
- **`forwardRequest` must not retain the inbound request context.** Always use `context.WithoutCancel(r.Context())` for the outbound goroutine. The 2.5-second ACK timeout is the *only* bound that applies to the Slack response; the outbound has its own longer timeout.
- **Compression is disabled on the forward HTTP client.** If you re-introduce it, endpoints that return gzip will have `Content-Encoding: gzip` headers disagree with their body content on Slack's side.
- **The forward path does not verify Slack signatures by default.** Signature verification only runs for (a) mirage-slack's own `/mirage-slack` subcommands, (b) entries where `protected = true` (set via `launch --protect`), and (c) `default_endpoint` unless `default_endpoint_protect = false`. See §1 of the rule above for why.

## Slack API landmines

Paths the implementation had to discover experimentally. Reverting or "simplifying" any of these will break things:

- `slackLists.columns.list`, `slackLists.info`, `slackLists.delete` are documented but return `unknown_method`. Use `files.info?file=<list_id>` to read the schema. `files.list?types=lists` enumerates lists.
- `slackLists.items.update` places `row_id` on **each cell entry**, not at the top level. Sending `{list_id, id, cells}` yields `row_id_not_provided`.
- Text column values use the Block Kit `rich_text` shape: an array of `rich_text` blocks, each containing `rich_text_section` elements, each containing `text` elements. Use `slack-go/slack`'s `NewRichTextBlock` / `NewRichTextSection` / `NewRichTextSectionTextElement` helpers — do not hand-roll the map nesting.
- Link columns: `[{"original_url": "..."}]` (array, camelCase `original_url`).
- Channel columns: `[string]` (array of channel IDs).
- `slackLists.items.list` cell shapes differ from `slackLists.items.create`: reading is `text` / `link[{originalUrl}]` / `checkbox` / `channel[]`, writing is `rich_text[...]` / `link[{original_url}]` / `checkbox` / `channel[]`. Note `originalUrl` vs `original_url`.
- `slackLists.access.set` with `access_level: "read"` and `channel_ids: [...]` is the supported way to share a bot-owned list with a channel. It's used by the `list` subcommand before posting the permalink.

## Scopes the Slack App must hold

Minimum for mirage-slack itself to run:

- `commands`
- `lists:read`, `lists:write`

Plus whatever scopes the endpoint behind mirage-slack wants the parent App to have subscribed to (e.g. `app_mentions:read`, `channels:history`). The forward path transports events untouched; the App has to be subscribed for them to arrive in the first place.

## Local development

```sh
# generate a starter config
mirage-slack init-config --output config.local.jsonnet

# the server
export SLACK_SIGNING_SECRET=... SLACK_BOT_TOKEN=xoxb-...
go run ./cmd/mirage-slack run --config=config.local.jsonnet --addr=:18080 --log-format=text

# a throwaway forward target (logs every request body verbatim)
go run ./my-local/echo-endpoint --addr=:18081 --name=dev1
```

Exposing both through a tunnel (ngrok, VS Code devtunnels, …) gives a full Slack → mirage-slack → endpoint loop that mirrors production. `my-local/` is gitignored.

`config.local.jsonnet` and `.envrc` are gitignored. Secrets should be loaded via `env('...')` / `ssm('...')` in the Jsonnet config, never hard-coded.

## Testing — what to write first

The initial release ships without tests. If you are adding tests, in priority order:

1. `signing.go` — pure function, security-critical. Table-driven tests for valid/expired/missing-headers/bad-secret/body-tampered. A regression here is a vulnerability.
2. `router.go`'s `decideRoute` — the core invariant-bearing function. Make `SlackListClient.ListEntries` an interface so it can be faked, then cover all four decisions plus the "1 channel = 1 launched" rejection.
3. `proxy.go`'s `forwardRequest` — concurrency logic worth pinning. Use `httptest.Server` for the endpoint and `go.uber.org/goleak` to detect goroutine leaks. Cover the three outcomes (fast response, timeout + late drain, endpoint failure).

Lower priority but valuable: `config.go` `applyDefaults` / `Validate`, `slack_list.go` `validateSchema`.

## Changing behaviour with confidence

- Run `go build ./...` and `go vet ./...`. Both are expected to be clean.
- For any change touching `slash.go` or `proxy.go`, re-read the "one rule" at the top of this file and write down *why* the change does not violate it. If you can't articulate why, don't merge.
- For any change touching `slack_list.go`, run the full register → launch → terminate → unregister → list cycle against a real workspace at least once. Slack's docs and Slack's actual API disagree often enough that unit tests alone don't catch schema drift.

## Things that are deliberately not done yet

Recorded so nobody re-litigates them without information:

- No rate-limit cache for `ListEntries`. Small workspaces don't need it; larger ones will.
- No `--migrate` for schema drift. Bot-owned lists are authoritative; if the schema drifts, the fix is to delete the list and let mirage-slack recreate it.
- No automatic termination of stale `launched` entries. Manual terminate is explicit and audit-friendly.
- No App Home / rich UI. `/mirage-slack list` deliberately relies on Slack's built-in list unfurl.
- No "forward bot token to endpoint" mode. Endpoints use `response_url` / `trigger_id` — the Slack-direct pattern.

When one of these becomes actually necessary, the right move is to lift the decision from this file into a GitHub issue with the new rationale rather than silently reverse it in code.
