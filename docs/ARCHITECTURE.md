# mirage-slack architecture

This document records the design rationale behind mirage-slack — the non-obvious decisions a reader cannot infer from the code alone. For user-facing docs see [`../README.md`](../README.md); for day-to-day maintenance hand-off see [`../CLAUDE.md`](../CLAUDE.md).

## Why mirage-slack exists

Building and iterating on a Slack App as a team has two friction points:

1. **The Slack App is a singleton.** Request URLs, slash command definitions, scopes — all of them live on one App per workspace. If Alice is running her development server at a given ngrok URL, Bob cannot point the same App somewhere else without stomping on her.
2. **Every developer spinning up their own App is paperwork-heavy.** Scopes, Event subscriptions, secrets, Install URLs — repeating the bootstrap per person scales poorly.

mirage-slack's position: **keep the parent App as a singleton, but route its traffic based on the channel the user is in**. One App, many targets. This is the "lamux for Slack" shape — lamux dispatches HTTP requests to different Lambda function aliases based on path; mirage-slack dispatches Slack payloads to different HTTP endpoints based on channel.

## Design axioms

### 1. Transparency over convenience

The primary value mirage-slack provides is that an endpoint that runs correctly via mirage-slack will also run correctly when wired directly to Slack. If we add convenience features on the forward path (body rewrites, header injection, silent signature checks), we risk the endpoint becoming accidentally coupled to mirage-slack's own behaviour, and the development loop loses its authority.

**Concrete consequence:** the default forward path does *not* verify Slack signatures. This looks counterintuitive, but verifying and rejecting forged requests before forwarding is precisely the class of "convenience feature" that would hide real bugs. A production endpoint that forgets to verify signatures should fail loudly when it's tested through mirage-slack, not be silently saved.

The escape hatch is explicit: `launch --protect` enables signature verification for that specific binding, and `routing.default_endpoint_protect` (default on) protects the fallback endpoint.

### 2. Storage is external and self-configuring

mirage-slack stores its state in a Slack-owned Slack List. The reasoning:

- The workflow is Slack-centric; using Slack's own storage avoids every user having to provision a database.
- The list is bot-owned, so the bot token already has the right to read/write it. No additional IAM / SQL credential is required.
- Operators can open the list in Slack's UI to audit the state.
- Startup can be fully automatic: `files.list?types=lists` enumerates bot-owned lists, and `slackLists.create` creates one on demand. No separate `init` step.

Unpleasant surprises the implementation had to work around:

- Several documented Lists API methods (`slackLists.columns.list`, `slackLists.info`, `slackLists.delete`) return `unknown_method` in practice. We recover the list schema from `files.info` instead, which does work and returns a `list_metadata.schema` block with column IDs and keys.
- `slackLists.items.update` expects `row_id` *on each cell entry*, not as a top-level field. This contradicts the obvious reading of the docs.
- Text columns require the full Block Kit rich_text structure (`rich_text` → `rich_text_section` → `text`). The `slack-go/slack` Block Kit types can be reused for this; do not hand-build the nested maps if you can avoid it.

### 3. 3-second ACK is architectural, not operational

Slack gives HTTP handlers 3 seconds to respond. mirage-slack cannot know in advance how long the downstream endpoint will take. The chosen pattern:

- Start the forward request on a context derived via `context.WithoutCancel(r.Context())` so it survives the handler returning.
- Race the forward response against a 2.5-second timer (the ACK budget, with a buffer).
- If the response wins, relay it untouched to Slack. If the timer wins, write a `200 OK` immediately and drain the forward response in a background goroutine (for logging).

Endpoints that genuinely take more than ~2 seconds are expected to use Slack's standard `response_url` to post their real response asynchronously. mirage-slack deliberately does *not* intervene in that flow.

**Lambda caveat:** when mirage-slack runs as a Lambda function, the runtime freezes the execution environment after the handler returns. The late-drain goroutine is on a best-effort basis and may never finish. This is acceptable because mirage-slack never needs to relay the late response itself — the endpoint talks to Slack directly via `response_url`. It does mean that the `forward late reply` log entries may be missing under Lambda.

### 4. "Nice-to-have" features are rejected by default

The development-aid framing means that every feature should be evaluated against "does adding this make the endpoint's behaviour diverge from the Slack-direct case?" Features that survived:

- **Channel-based routing.** Unavoidable; this is the core value proposition.
- **Opt-in signature verification** (`--protect`, `default_endpoint_protect`). Limited-scope convenience for fallback endpoints.
- **Auto-grant list access on `list` subcommand.** Needed so channel members can actually see the unfurled list preview. Doesn't affect forward behaviour.

Features deliberately *not* included:

- Bot Token forwarding to endpoints. Endpoints should use `response_url` / `trigger_id` (Slack's standard). Shipping the parent App's bot token to developer endpoints would make the development path diverge from the Slack-direct case.
- Wrapping entry lists or launch state in decorated messages. The `list` subcommand deliberately replies with just the permalink; Slack's unfurl renders the table. Adding "N environments launched" kind of summaries would require keeping two states in sync.
- Automatic TTL / cleanup of launched entries. Explicit terminate is clearer.

## Runtime shape

### Module layout

```
github.com/mashiike/mirage-slack/
├── cmd/mirage-slack/         entrypoint (main)
├── *.go                      library package `mirageslack`
│   ├── cli.go                kong CLI (run / init-config) + logging setup
│   ├── config.go             Config struct + jsonnet load + defaults + validate
│   ├── native.go             jsonnet native funcs (env, ssm)
│   ├── server.go             App wiring (ridge + http.ServeMux)
│   ├── router.go             decideRoute — per-request routing decision
│   ├── proxy.go              forward with 2.5s ACK + WithoutCancel
│   ├── signing.go            Slack v0 HMAC verify
│   ├── slash.go              slash / interactive / events handlers + subcommand dispatch
│   ├── slack_list.go         Slack Lists Web API wrapper
│   └── mirage_slack.go       package doc + Version
├── example/config.jsonnet
├── my-local/echo-endpoint/   throwaway forward target for local E2E
├── README.md / ARCHITECTURE.md / CLAUDE.md
└── LICENSE (MIT)
```

### Request lifecycle

All three Slack entrypoints (`/slack/commands`, `/slack/interactive`, `/slack/events`) funnel through a common shape:

1. Read body with a 5 MiB cap (`http.MaxBytesReader`).
2. Extract `channel_id` (from the slash payload, `payload.channel.id` in interactive, or `event.channel` in events).
3. `decideRoute(commandName, channelID)` returns one of four decisions:
   - `routeInternal` — the payload targets mirage-slack's own `/mirage-slack` command. Verify the signature and dispatch to the subcommand pipeline.
   - `routeForwardEntry` — a launched entry matches the channel. Forward (verifying signature first iff the entry is protected).
   - `routeForwardDefault` — no match, but `routing.default_endpoint` is configured. Forward there (verifying iff `default_endpoint_protect` is on).
   - `routeNotLaunched` — nothing to do. Respond with an ephemeral hint (for slash commands) or `200 OK` (for events/interactive).
4. Forward payloads hit the 3-second ACK logic described above. Internal subcommands are parsed with kong and dispatched to methods on `SlackListClient`.

### Slack List schema

Fixed, five columns plus Slack's automatic bookkeeping:

| key | type | role |
|---|---|---|
| `name` | text (primary) | Human identifier used by slash subcommands. |
| `endpoint` | link | Forward target URL. |
| `launched` | checkbox | Whether the entry currently accepts forwarded traffic. |
| `launched_channel` | channel | Channel the entry is bound to while launched. |
| `protected` | checkbox | Verify Slack signature before forwarding. |

Automatic columns (`created_by`, `created_time`, `last_edited_time`) record provenance.

## Known limitations & future work

- **Rate limiting.** `decideRoute` reads `slackLists.items.list` on every inbound request. For low-traffic dev workspaces this is fine; for high-traffic deployments a short TTL cache would be needed. Not done in the initial release.
- **Concurrent `register` race.** Two users running `register alice …` simultaneously can both observe "not found" and both create an entry. Slack Lists has no compare-and-swap. The window is small but real.
- **`interactive` payload channel extraction** only inspects `payload.channel.id`. Some interactive types (certain modal flows, global shortcuts) carry the channel elsewhere. These currently fall through to `routeNotLaunched` and are effectively unsupported.
- **1 channel = 1 launched** is enforced, but only against entries already marked `launched`. If the Slack List is hand-edited (it should not be — mirage-slack treats it as its own store), invariants can be violated.
- **Testing.** No tests ship with this initial release. Highest-value targets, in order: `signing.go`, `router.go` (mock `ListEntries`), `proxy.go` (httptest + goleak for the 2.5s ACK path).

These are all documented in the git history of this file and in `CLAUDE.md` so future maintainers have the context.
