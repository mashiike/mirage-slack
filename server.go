package mirageslack

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/fujiwara/ridge"
)

// App bundles the runtime state needed to serve mirage-slack.
type App struct {
	cfg  *Config
	list *SlackListClient
}

// NewApp builds an App and ensures the Slack List is ready (discovered or
// created) before returning.
func NewApp(ctx context.Context, cfg *Config) (*App, error) {
	slog.Info("initializing mirage-slack app", "list_name", cfg.Slack.ListName)
	list := NewSlackListClient(cfg.Slack.BotToken, cfg.Slack.ListName)
	listID, err := list.Ensure(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensure slack list: %w", err)
	}
	slog.Info("slack list ready", "list_id", listID, "list_name", cfg.Slack.ListName)
	return &App{cfg: cfg, list: list}, nil
}

// Serve starts the HTTP server. On AWS Lambda, ridge runs the Lambda runtime
// transparently; otherwise it listens on addr.
func (a *App) Serve(ctx context.Context, addr string) error {
	ridge.RunWithContext(ctx, addr, "/", a.Handler())
	return nil
}

// Handler builds the HTTP handler tree.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /slack/commands", a.HandleSlashCommand)
	mux.HandleFunc("POST /slack/interactive", a.HandleInteractive)
	mux.HandleFunc("POST /slack/events", a.HandleEvent)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}
