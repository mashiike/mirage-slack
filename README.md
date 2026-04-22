# mirage-slack

A thin reverse proxy / multiplexer for Slack Apps development — one shared parent Slack App, many developer endpoints.

```
  Slack workspace                                   developer endpoints
  ┌────────────────┐                                ┌────────────────────┐
  │ Slash commands │                                │  alice's ngrok     │
  │ Interactivity  │──▶ mirage-slack ──────────────▶│  bob's local server│
  │ Events API     │    (channel-based routing)     │  main / production │
  └────────────────┘                                └────────────────────┘
```

Instead of spinning up a separate Slack App per developer (dev Alice, dev Bob, staging, …), **create one parent Slack App once**, point it at mirage-slack, and let each channel dispatch to a different endpoint. Developers use slash commands to register their own endpoint, bind it to a channel, and stop when done.

- **Simple forwarder** — Slack payloads pass through untouched. The endpoint sees the same request shape it would see when wired directly to Slack, so end-to-end behaviour observed via mirage-slack matches production.
- **Self-discovering storage** — The runtime uses a bot-owned [Slack List](https://slack.com/features/lists) as its only backing store. No DynamoDB / Postgres to set up. mirage-slack finds or creates the list on startup.
- **Lambda / server / container ready** — Single binary runs on all three via [`fujiwara/ridge`](https://github.com/fujiwara/ridge).

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Slash subcommands](#slash-subcommands)
- [Configuration](#configuration)
- [Design & architecture](#design--architecture)
- [Contributing](#contributing)

## Install

```sh
go install github.com/mashiike/mirage-slack/cmd/mirage-slack@latest
```

Requires Go 1.26+.

## Quick start

### 1. Create the parent Slack App

Create an app at [api.slack.com/apps](https://api.slack.com/apps) and configure:

| Slack App feature | Value |
|---|---|
| Slash Command `/mirage-slack` | Request URL: `https://<host>/slack/commands` |
| Interactivity & Shortcuts | Request URL: `https://<host>/slack/interactive` |
| Event Subscriptions | Request URL: `https://<host>/slack/events` |

**Minimal Bot Token scopes**:

- `commands` (receive slash commands)
- `lists:write`, `lists:read` (manage the bot-owned Slack List)

Add any scope your forward targets need in production (`app_mentions:read`, `chat:write`, `channels:history`, …). These must be registered here because the parent App is the one actually subscribed to Slack events.

### 2. Generate a starter config

```sh
mirage-slack init-config > config.jsonnet
# or
mirage-slack init-config --output config.jsonnet
```

Edit the file if you want to override defaults. The minimum workable configuration uses only two environment variables:

```jsonnet
local env = std.native('env');

{
  slack: {
    signing_secret: env('SLACK_SIGNING_SECRET'),
    bot_token: env('SLACK_BOT_TOKEN'),
  },
  command: { name: '/mirage-slack' },
  routing: {},
}
```

### 3. Run the server

```sh
export SLACK_SIGNING_SECRET=...
export SLACK_BOT_TOKEN=xoxb-...
mirage-slack run --config=config.jsonnet --addr=:8080
```

On startup, mirage-slack:

1. Calls `auth.test` to resolve the bot's user ID and team URL.
2. Searches `files.list?types=lists` for a bot-owned list titled `mirage-slack` (override with `slack.list_name`).
3. If no list is found, creates one via `slackLists.create` with mirage-slack's fixed schema.
4. Caches the list metadata (column IDs, permalink components) and starts serving HTTP.

No separate `init` / migration step is required.

### 4. Register a development endpoint

From any channel:

```
/mirage-slack register alice https://alice.ngrok.app
/mirage-slack launch alice
```

Subsequent slash commands / interactivity / events in that channel are forwarded to `https://alice.ngrok.app`. When you are done:

```
/mirage-slack terminate alice       # stop forwarding, keep the registration
/mirage-slack unregister alice      # remove the registration entirely
```

## Slash subcommands

| Command | Effect |
|---|---|
| `/mirage-slack register <name> <url>` | Record a forward target (or update the URL of an existing entry). |
| `/mirage-slack unregister <name>` | Remove the entry. |
| `/mirage-slack launch <name> [--protect]` | Bind the entry to the current channel. With `--protect`, mirage-slack verifies the Slack signature before forwarding. |
| `/mirage-slack terminate <name>` | Unbind the entry. The registration is kept so you can `launch` again later. |
| `/mirage-slack list` | Grant the current channel view access to the list file and post its URL (Slack unfurls it). |

Rules:

- Registrations are workspace-wide; launch bindings are per-channel.
- One environment per channel: if another entry is already launched in the same channel, `launch` errors out — terminate the other one first.
- DMs are not supported as launch targets (public / private channels only).
- Success responses post **in the channel** (`in_channel`). Errors and usage go to the invoking user only (`ephemeral`).

## Configuration

Config is a [Jsonnet](https://jsonnet.org/) file. Two native functions are registered:

| Function | Use |
|---|---|
| `std.native('env')(name)` | Read an environment variable. |
| `std.native('ssm')(path)` | Fetch an AWS SSM Parameter Store value (decrypted). |

| Path | Type | Required | Default | Description |
|---|---|---|---|---|
| `slack.signing_secret` | string | ✓ | | Slack App Signing Secret. |
| `slack.bot_token` | string | ✓ | | Bot User OAuth Token (`xoxb-…`). |
| `slack.list_name` | string | | `mirage-slack` | Title of the bot-owned Slack List. |
| `command.name` | string | | `/mirage-slack` | Slash command name. |
| `routing.default_endpoint` | string | | — | Forward target for requests whose channel is not bound to any entry. |
| `routing.default_endpoint_protect` | bool | | `true` | Verify the Slack signature before forwarding to `default_endpoint`. |

CLI flags:

```
mirage-slack [--config=path] [--log-format=json|text] [--log-level=debug|info|warn|error] <command>

Commands:
  run          [--addr=:8080]                    start the HTTP server
  init-config  [--output path] [--force]         emit a starter config.jsonnet
```

## Design & architecture

- **Transparent forwarding is the non-negotiable property.** mirage-slack does not validate, enrich, or reshape payloads on the forward path by default. This is what makes it a _development_ aid: if the endpoint works under mirage-slack, it will work when wired to Slack directly.
- **Opt-in signature verification** (`launch --protect`, `routing.default_endpoint_protect`) exists for production-ish endpoints that should not be usable as an open relay.
- **3-second Slack SLA** is handled with `context.WithoutCancel`: the forward request is detached from the inbound context so the endpoint can keep processing past the 2.5-second ACK deadline. Late replies must use Slack's `response_url` (this is the standard Slack pattern anyway).

For the complete rationale, see [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Contributing

- See [`CLAUDE.md`](CLAUDE.md) for the developer hand-off notes (architecture invariants, non-obvious gotchas, how to reason about changes).
- `go build ./...` / `go vet ./...` should pass without errors.
- `my-local/echo-endpoint/` is a throwaway forward target useful for manual end-to-end verification with ngrok / devtunnels.

## License

MIT
