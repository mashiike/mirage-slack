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
}

type SlackConfig struct {
	SigningSecret string `json:"signing_secret"`
	BotToken      string `json:"bot_token"`
	// ListName identifies the bot-owned Slack List. At run startup mirage-slack
	// looks up a bot-owned list with this title; if none exists, it creates one.
	ListName string `json:"list_name"`
}

type CommandConfig struct {
	Name string `json:"name"`
}

type RoutingConfig struct {
	DefaultEndpoint        string `json:"default_endpoint"`
	DefaultEndpointProtect *bool  `json:"default_endpoint_protect"`
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
		c.Slack.ListName = "mirage-slack"
	}
	if c.Routing.DefaultEndpointProtect == nil {
		t := true
		c.Routing.DefaultEndpointProtect = &t
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
	return nil
}

// DefaultEndpointProtectEnabled returns the effective value with default applied.
func (c *Config) DefaultEndpointProtectEnabled() bool {
	if c.Routing.DefaultEndpointProtect == nil {
		return true
	}
	return *c.Routing.DefaultEndpointProtect
}
