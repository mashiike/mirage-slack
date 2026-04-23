package mirageslack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/alecthomas/kong"
)

// maxSlackBodyBytes bounds the body size mirage-slack will read from a single
// HTTP request. Slack payloads are at most a few MB in practice.
const maxSlackBodyBytes = 5 << 20 // 5 MiB

// SlashCommand is the subset of Slack slash command payload fields that
// mirage-slack inspects.
type SlashCommand struct {
	Command     string
	Text        string
	ChannelID   string
	UserID      string
	ResponseURL string
	TriggerID   string
}

func parseSlashCommand(body []byte) SlashCommand {
	v, err := url.ParseQuery(string(body))
	if err != nil {
		slog.Warn("parse slash command body (partial result used)", "error", err)
	}
	return SlashCommand{
		Command:     v.Get("command"),
		Text:        v.Get("text"),
		ChannelID:   v.Get("channel_id"),
		UserID:      v.Get("user_id"),
		ResponseURL: v.Get("response_url"),
		TriggerID:   v.Get("trigger_id"),
	}
}

// HandleSlashCommand dispatches slash command requests to either the
// internal subcommand pipeline or the forward pipeline.
func (a *App) HandleSlashCommand(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSlackBodyBytes)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	cmd := parseSlashCommand(bodyBytes)

	slog.Info("slack.commands received",
		"command", cmd.Command,
		"channel_id", cmd.ChannelID,
		"user_id", cmd.UserID,
		"text", cmd.Text,
	)

	decision, err := a.decideRoute(r.Context(), cmd.Command, cmd.ChannelID)
	if err != nil {
		slog.Error("slack.commands route error", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logRouteDecision("slack.commands routed", decision)

	switch decision.Decision {
	case routeInternal:
		if !verifySlackSignature(r.Header, bodyBytes, a.cfg.Slack.SigningSecret) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		a.handleInternalSlashCommand(r.Context(), w, cmd)

	case routeForwardEntry:
		if decision.Protect && !verifySlackSignature(r.Header, bodyBytes, a.cfg.Slack.SigningSecret) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		a.forwardRequest(w, r, bodyBytes, decision.Entry.Endpoint, decision.Entry.Name)

	case routeForwardDefault:
		if decision.Protect && !verifySlackSignature(r.Header, bodyBytes, a.cfg.Slack.SigningSecret) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		a.forwardRequest(w, r, bodyBytes, a.cfg.Routing.DefaultEndpoint, "")

	case routeNotLaunched:
		respondEphemeral(w, fmt.Sprintf("no environment is launched in this channel (channel_id=%s). Use `%s register` / `%s launch` first.",
			cmd.ChannelID, a.cfg.Command.Name, a.cfg.Command.Name))
	}
}

// HandleInteractive is the forward-only entrypoint for block actions, modal
// submissions, and shortcuts.
func (a *App) HandleInteractive(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSlackBodyBytes)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	channelID := extractChannelIDFromInteractive(bodyBytes)
	slog.Info("slack.interactive received", "channel_id", channelID, "bytes", len(bodyBytes))
	a.forwardOrRespond(w, r, bodyBytes, channelID, "slack.interactive")
}

// HandleEvent is the Events API entrypoint. url_verification challenges are
// answered here; everything else is forwarded using the event's channel_id.
func (a *App) HandleEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSlackBodyBytes)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if challenge := extractURLVerificationChallenge(bodyBytes); challenge != "" {
		slog.Info("slack.events url_verification challenge answered")
		w.Header().Set("Content-Type", "text/plain")
		if _, err := w.Write([]byte(challenge)); err != nil {
			slog.Warn("write challenge response", "error", err)
		}
		return
	}

	eventType := extractEventType(bodyBytes)
	channelID := extractChannelIDFromEvent(bodyBytes)
	slog.Info("slack.events received",
		"event_type", eventType,
		"channel_id", channelID,
		"bytes", len(bodyBytes),
	)

	if channelID == "" {
		slog.Info("slack.events skipped (no channel_id)", "event_type", eventType)
		w.WriteHeader(http.StatusOK)
		return
	}
	a.forwardOrRespond(w, r, bodyBytes, channelID, "slack.events")
}

// forwardOrRespond is the shared path for interactive / event requests.
func (a *App) forwardOrRespond(w http.ResponseWriter, r *http.Request, bodyBytes []byte, channelID, source string) {
	decision, err := a.decideRoute(r.Context(), "", channelID)
	if err != nil {
		slog.Error(source+" route error", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logRouteDecision(source+" routed", decision)
	switch decision.Decision {
	case routeForwardEntry:
		if decision.Protect && !verifySlackSignature(r.Header, bodyBytes, a.cfg.Slack.SigningSecret) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		a.forwardRequest(w, r, bodyBytes, decision.Entry.Endpoint, decision.Entry.Name)
	case routeForwardDefault:
		if decision.Protect && !verifySlackSignature(r.Header, bodyBytes, a.cfg.Slack.SigningSecret) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		a.forwardRequest(w, r, bodyBytes, a.cfg.Routing.DefaultEndpoint, "")
	case routeNotLaunched, routeInternal:
		w.WriteHeader(http.StatusOK)
	}
}

// slashCLI is the kong grammar for the in-Slack subcommands.
type slashCLI struct {
	Register   registerCmd   `cmd:"" help:"register an environment endpoint"`
	Unregister unregisterCmd `cmd:"" help:"unregister an environment"`
	Launch     launchCmd     `cmd:"" help:"launch an environment in the current channel"`
	Terminate  terminateCmd  `cmd:"" help:"terminate a launched environment"`
	List       listCmd       `cmd:"" help:"list all environments"`
	PruneList  pruneListCmd  `cmd:"prune-list" help:"delete a bot-owned Slack List by file_id (use for orphans left behind after list_name changes)"`
}

type registerCmd struct {
	Name     string `arg:"" help:"environment name"`
	Endpoint string `arg:"" help:"forward target URL"`
}

type unregisterCmd struct {
	Name string `arg:"" help:"environment name"`
}

type launchCmd struct {
	Name    string `arg:"" help:"environment name"`
	Protect bool   `name:"protect" help:"enable SigningSecret verification on forward"`
}

type terminateCmd struct {
	Name string `arg:"" help:"environment name"`
}

type listCmd struct{}

type pruneListCmd struct {
	FileID string `arg:"" name:"file_id" help:"Slack file_id of the bot-owned list to delete (F…)"`
}

// handleInternalSlashCommand parses cmd.Text with kong and dispatches.
func (a *App) handleInternalSlashCommand(ctx context.Context, w http.ResponseWriter, cmd SlashCommand) {
	args := splitText(cmd.Text)
	if len(args) == 0 {
		respondEphemeral(w, usageHelp(cmd.Command))
		return
	}

	var cli slashCLI
	parser, err := kong.New(&cli,
		kong.Name(cmd.Command),
		kong.Exit(func(int) {}),
		kong.Writers(io.Discard, io.Discard),
	)
	if err != nil {
		respondEphemeral(w, "internal error: "+err.Error())
		return
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		respondEphemeral(w, fmt.Sprintf("parse error: %s\n\n%s", err.Error(), usageHelp(cmd.Command)))
		return
	}

	msg, err := a.runSlashSubcommand(ctx, kctx, cmd, &cli)
	if err != nil {
		respondEphemeral(w, "error: "+err.Error())
		return
	}
	respondInChannel(w, msg)
}

func (a *App) runSlashSubcommand(ctx context.Context, kctx *kong.Context, cmd SlashCommand, cli *slashCLI) (string, error) {
	switch kctx.Command() {
	case "register <name> <endpoint>":
		if err := a.list.Register(ctx, cli.Register.Name, cli.Register.Endpoint); err != nil {
			return "", err
		}
		return fmt.Sprintf("registered `%s` → %s", cli.Register.Name, cli.Register.Endpoint), nil

	case "unregister <name>":
		if err := a.list.Unregister(ctx, cli.Unregister.Name); err != nil {
			return "", err
		}
		return fmt.Sprintf("unregistered `%s`", cli.Unregister.Name), nil

	case "launch <name>":
		if err := ensureChannelLaunchable(cmd.ChannelID); err != nil {
			return "", err
		}
		if err := a.list.Launch(ctx, cli.Launch.Name, cmd.ChannelID, cli.Launch.Protect); err != nil {
			return "", err
		}
		protect := ""
		if cli.Launch.Protect {
			protect = " (protect=on)"
		}
		return fmt.Sprintf("launched `%s` in <#%s>%s", cli.Launch.Name, cmd.ChannelID, protect), nil

	case "terminate <name>":
		if err := a.list.Terminate(ctx, cli.Terminate.Name); err != nil {
			return "", err
		}
		return fmt.Sprintf("terminated `%s`", cli.Terminate.Name), nil

	case "list":
		if cmd.ChannelID != "" {
			if err := a.list.GrantViewAccessToChannel(ctx, cmd.ChannelID); err != nil {
				return "", fmt.Errorf("grant view access to <#%s>: %w", cmd.ChannelID, err)
			}
		}
		permalink := a.list.ListPermalink()
		if permalink == "" {
			return "", fmt.Errorf("list permalink unavailable")
		}
		return permalink, nil

	case "prune-list <file_id>":
		if err := a.list.DeleteList(ctx, cli.PruneList.FileID); err != nil {
			return "", err
		}
		return fmt.Sprintf("pruned bot-owned list `%s`", cli.PruneList.FileID), nil
	}
	return "", fmt.Errorf("unknown subcommand: %s", kctx.Command())
}

// ensureChannelLaunchable rejects DM / group DM channels (docs §5.4: launch
// must be performed in a public or private channel).
//
// Slack channel ID prefixes:
//
//	C… public channel
//	G… private channel (legacy; now also used for some group DMs)
//	D… direct message
//
// We reject D prefix. G is ambiguous (private channel vs group DM), but the
// common "dev チャンネルで launch する" flow uses C/G, so we allow G and accept
// the rare group-DM edge case for simplicity.
func ensureChannelLaunchable(channelID string) error {
	if channelID == "" {
		return fmt.Errorf("launch requires a channel context")
	}
	if strings.HasPrefix(channelID, "D") {
		return fmt.Errorf("launch in a DM is not supported (use a public or private channel)")
	}
	return nil
}

// splitText performs a simple whitespace split. Quoted strings are not
// supported in v1 (Slack slash command `text` rarely needs them).
func splitText(s string) []string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// respondEphemeral writes a Slack slash command ephemeral response (only the
// invoking user sees it). Used for errors / usage messages.
func respondEphemeral(w http.ResponseWriter, text string) {
	writeSlashResponse(w, "ephemeral", text)
}

// respondInChannel writes a Slack slash command in_channel response (every
// member of the channel sees the message). Used for successful operations so
// that the team can observe each other's register/launch/terminate activity.
func respondInChannel(w http.ResponseWriter, text string) {
	writeSlashResponse(w, "in_channel", text)
}

func writeSlashResponse(w http.ResponseWriter, responseType, text string) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"response_type": responseType,
		"text":          text,
	}); err != nil {
		slog.Warn("write slash response", "error", err)
	}
}

// extractChannelIDFromInteractive parses the form-encoded `payload` field
// and pulls out payload.channel.id.
func extractChannelIDFromInteractive(body []byte) string {
	v, err := url.ParseQuery(string(body))
	if err != nil {
		return ""
	}
	payload := v.Get("payload")
	if payload == "" {
		return ""
	}
	var envelope struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return ""
	}
	return envelope.Channel.ID
}

// extractURLVerificationChallenge returns the challenge value when the body
// is a url_verification event; empty string otherwise.
func extractURLVerificationChallenge(body []byte) string {
	var envelope struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	if envelope.Type != "url_verification" {
		return ""
	}
	return envelope.Challenge
}

// extractChannelIDFromEvent pulls event.channel from the Events API wrapper.
func extractChannelIDFromEvent(body []byte) string {
	var envelope struct {
		Event struct {
			Channel   string `json:"channel"`
			ChannelID string `json:"channel_id"`
			Item      struct {
				Channel string `json:"channel"`
			} `json:"item"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	if envelope.Event.Channel != "" {
		return envelope.Event.Channel
	}
	if envelope.Event.ChannelID != "" {
		return envelope.Event.ChannelID
	}
	return envelope.Event.Item.Channel
}

// extractEventType returns the event.type string of an Events API envelope,
// or empty string if not present.
func extractEventType(body []byte) string {
	var envelope struct {
		Event struct {
			Type string `json:"type"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Event.Type
}

func logRouteDecision(msg string, d routeResult) {
	attrs := []any{"decision", decisionLabel(d.Decision), "protect", d.Protect}
	if d.Entry != nil {
		attrs = append(attrs, "env", d.Entry.Name, "endpoint", d.Entry.Endpoint)
	}
	slog.Info(msg, attrs...)
}

// usageHelp returns the help text shown to the user when the slash command is
// invoked with no arguments or with a parse error. The command name is
// parameterized so it tracks config.command.name overrides.
func usageHelp(command string) string {
	return fmt.Sprintf(`*Usage:* `+"`%s <subcommand> [args]`"+`

*Subcommands:*
• `+"`%s register <name> <url>`"+`     — Register a forward target endpoint
• `+"`%s unregister <name>`"+`         — Remove a registration
• `+"`%s launch <name> [--protect]`"+` — Bind this channel to the entry and start forwarding (`+"`--protect`"+` enables SigningSecret verification)
• `+"`%s terminate <name>`"+`          — Stop forwarding (registration is kept)
• `+"`%s list`"+`                      — Share the list file with this channel (view-only) and post its link
• `+"`%s prune-list <file_id>`"+`      — Delete a bot-owned Slack List by file_id (for orphans left over after list_name changes)

*Example:*
`+"```"+`
%s register dev1 https://dev1.example.com
%s launch dev1
%s list
`+"```",
		command, command, command, command, command, command, command,
		command, command, command)
}

func decisionLabel(d routeDecision) string {
	switch d {
	case routeInternal:
		return "internal"
	case routeForwardEntry:
		return "forward_entry"
	case routeForwardDefault:
		return "forward_default"
	case routeNotLaunched:
		return "not_launched"
	}
	return "unknown"
}
