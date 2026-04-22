package mirageslack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/alecthomas/kong"
)

// CLI is the top-level kong grammar for the `mirage-slack` binary.
type CLI struct {
	Config    string           `help:"path to config file (jsonnet)" short:"c" default:"config.jsonnet"`
	LogFormat string           `help:"log output format (json or text)" enum:"json,text" default:"json"`
	LogLevel  string           `help:"log level (debug, info, warn, error)" enum:"debug,info,warn,error" default:"info"`
	Version   kong.VersionFlag `help:"show version and exit" short:"V"`

	Run        runCmd        `cmd:"" default:"withargs" help:"run the mirage-slack HTTP server"`
	InitConfig initConfigCmd `cmd:"init-config" help:"emit a starter config.jsonnet"`
}

type runCmd struct {
	Addr string `help:"listen address (ignored on AWS Lambda)" default:":8080"`
}

type initConfigCmd struct {
	Output string `short:"o" help:"write config to this path (default: stdout)"`
	Force  bool   `help:"overwrite the output file if it already exists"`
}

// Run is the entry point used by cmd/mirage-slack/main.go.
func Run(ctx context.Context, args []string) error {
	var cli CLI
	parser, err := kong.New(&cli,
		kong.Name("mirage-slack"),
		kong.Description("Slack Apps dispatcher / multiplexer for developer environments"),
		kong.UsageOnError(),
		kong.Vars{"version": Version},
	)
	if err != nil {
		return err
	}
	kctx, err := parser.Parse(args)
	if err != nil {
		return err
	}

	if err := setupLogging(cli.LogFormat, cli.LogLevel); err != nil {
		return err
	}

	switch kctx.Command() {
	case "run":
		return runCommand(ctx, &cli)
	case "init-config":
		return initConfigCommand(&cli.InitConfig)
	default:
		return fmt.Errorf("unknown command: %s", kctx.Command())
	}
}

const starterConfigJsonnet = `local ssm = std.native('ssm');
local env = std.native('env');

{
  slack: {
    // Slack App's Signing Secret.
    signing_secret: env('SLACK_SIGNING_SECRET'),
    // Bot User OAuth Token (xoxb-...).
    bot_token: env('SLACK_BOT_TOKEN'),
    // Title of the bot-owned Slack List that stores the environment entries.
    // Optional; defaults to 'mirage-slack'. mirage-slack will discover a list
    // with this title on startup and create one if it does not exist.
    // list_name: 'mirage-slack',
  },
  command: {
    // Slash command name. Defaults to '/mirage-slack'.
    name: '/mirage-slack',
  },
  routing: {
    // Optional fallback: requests whose channel is not bound to any entry
    // are forwarded to default_endpoint. If omitted, such requests receive
    // an ephemeral "not launched" response.
    // default_endpoint: 'https://main.example.com/slack',
    // When default_endpoint_protect is true (default), mirage-slack verifies
    // the SigningSecret before forwarding to the default endpoint.
    // default_endpoint_protect: true,
  },
}
`

func initConfigCommand(cmd *initConfigCmd) error {
	if cmd.Output == "" || cmd.Output == "-" {
		if _, err := io.WriteString(os.Stdout, starterConfigJsonnet); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		return nil
	}
	if _, err := os.Stat(cmd.Output); err == nil && !cmd.Force {
		return fmt.Errorf("refuse to overwrite existing file %q (use --force to overwrite)", cmd.Output)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %q: %w", cmd.Output, err)
	}
	if err := os.WriteFile(cmd.Output, []byte(starterConfigJsonnet), 0o600); err != nil {
		return fmt.Errorf("write %q: %w", cmd.Output, err)
	}
	slog.Info("wrote starter config", "path", cmd.Output)
	return nil
}

func setupLogging(format, level string) error {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("invalid log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	return nil
}

func runCommand(ctx context.Context, cli *CLI) error {
	cfg, err := LoadConfig(ctx, cli.Config)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	app, err := NewApp(ctx, cfg)
	if err != nil {
		return err
	}
	return app.Serve(ctx, cli.Run.Addr)
}
