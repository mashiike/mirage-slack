package mirageslack

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	jsonnet "github.com/google/go-jsonnet"
)

// Config is the top-level configuration loaded from a jsonnet file.
type Config struct {
	Slack   SlackConfig   `json:"slack"`
	Command CommandConfig `json:"command"`
	Routing RoutingConfig `json:"routing"`
	Server  ServerConfig  `json:"server"`
}

type SlackConfig struct {
	SigningSecret string `json:"signing_secret"`
	BotToken      string `json:"bot_token"`
	// ListName identifies the bot-owned Slack List. At run startup mirage-slack
	// looks up a bot-owned list with this title; if none exists, it creates one.
	// Defaults to the slash command name without the leading slash (e.g. command
	// "/mirage-slack" yields "mirage-slack"), so running multiple instances with
	// distinct command names yields distinct list titles with zero extra config.
	ListName string `json:"list_name"`
}

type CommandConfig struct {
	Name string `json:"name"`
}

type RoutingConfig struct {
	DefaultEndpoint        string `json:"default_endpoint"`
	DefaultEndpointProtect *bool  `json:"default_endpoint_protect"`
}

// ServerConfig overrides the HTTP mount points exposed by mirage-slack.
type ServerConfig struct {
	Paths ServerPaths `json:"paths"`
}

// ServerPaths maps each Slack entrypoint to its URL path. Empty fields fall
// back to the defaults shown in DefaultServerPaths.
type ServerPaths struct {
	Commands    string `json:"commands"`
	Interactive string `json:"interactive"`
	Events      string `json:"events"`
}

// DefaultServerPaths returns the default URL paths used when the config
// leaves the corresponding fields empty.
func DefaultServerPaths() ServerPaths {
	return ServerPaths{
		Commands:    "/slack/commands",
		Interactive: "/slack/interactive",
		Events:      "/slack/events",
	}
}

// LoadConfig evaluates the jsonnet file at path and decodes it into a Config.
// Native functions (ssm, env) are registered against the provided context.
func LoadConfig(ctx context.Context, path string) (*Config, error) {
	vm := jsonnet.MakeVM()
	registerNativeFunctions(ctx, vm)

	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	jsonStr, err := vm.EvaluateAnonymousSnippet(path, string(bytes))
	if err != nil {
		return nil, fmt.Errorf("evaluate jsonnet: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal([]byte(jsonStr), &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Command.Name == "" {
		c.Command.Name = "/mirage-slack"
	}
	if !strings.HasPrefix(c.Command.Name, "/") {
		c.Command.Name = "/" + c.Command.Name
	}
	if c.Slack.ListName == "" {
		c.Slack.ListName = strings.TrimPrefix(c.Command.Name, "/")
	}
	if c.Routing.DefaultEndpointProtect == nil {
		t := true
		c.Routing.DefaultEndpointProtect = &t
	}
	defaults := DefaultServerPaths()
	if c.Server.Paths.Commands == "" {
		c.Server.Paths.Commands = defaults.Commands
	}
	if c.Server.Paths.Interactive == "" {
		c.Server.Paths.Interactive = defaults.Interactive
	}
	if c.Server.Paths.Events == "" {
		c.Server.Paths.Events = defaults.Events
	}
}

// Validate checks required fields.
func (c *Config) Validate() error {
	if c.Slack.SigningSecret == "" {
		return fmt.Errorf("slack.signing_secret is required")
	}
	if c.Slack.BotToken == "" {
		return fmt.Errorf("slack.bot_token is required")
	}
	return c.validateServerPaths()
}

var reservedServerPaths = map[string]string{
	"/healthz": "reserved for the health check endpoint",
}

// invalidServerPathChars are rejected in configured paths. '{' and '}' would
// be interpreted as net/http.ServeMux wildcards; whitespace would break the
// "METHOD PATH" pattern syntax. Either case causes mux.Handle to panic at
// startup, so we surface the problem as a Validate error instead.
const invalidServerPathChars = "{} \t"

func (c *Config) validateServerPaths() error {
	entries := []struct {
		key   string
		value string
	}{
		{"server.paths.commands", c.Server.Paths.Commands},
		{"server.paths.interactive", c.Server.Paths.Interactive},
		{"server.paths.events", c.Server.Paths.Events},
	}
	seen := map[string]string{}
	for _, e := range entries {
		if !strings.HasPrefix(e.value, "/") {
			return fmt.Errorf("%s must start with '/': %q", e.key, e.value)
		}
		if i := strings.IndexAny(e.value, invalidServerPathChars); i >= 0 {
			return fmt.Errorf("%s contains disallowed character %q (ServeMux wildcards and whitespace are not supported): %q",
				e.key, string(e.value[i]), e.value)
		}
		if reason, ok := reservedServerPaths[e.value]; ok {
			return fmt.Errorf("%s cannot be %q: %s", e.key, e.value, reason)
		}
		if other, ok := seen[e.value]; ok {
			return fmt.Errorf("%s conflicts with %s: both are %q", e.key, other, e.value)
		}
		seen[e.value] = e.key
	}
	return nil
}

// DefaultEndpointProtectEnabled returns the effective value with default applied.
func (c *Config) DefaultEndpointProtectEnabled() bool {
	if c.Routing.DefaultEndpointProtect == nil {
		return true
	}
	return *c.Routing.DefaultEndpointProtect
}
