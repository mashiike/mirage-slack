package mirageslack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

// Column keys for the mirage-slack Slack List schema.
const (
	ColName            = "name"
	ColEndpoint        = "endpoint"
	ColLaunched        = "launched"
	ColLaunchedChannel = "launched_channel"
	ColProtected       = "protected"
)

// Schema is the fixed Slack List schema that mirage-slack owns.
var Schema = []Column{
	{Key: ColName, Name: "Name", Type: "text", IsPrimaryColumn: true},
	{Key: ColEndpoint, Name: "Endpoint", Type: "link"},
	{Key: ColLaunched, Name: "Launched", Type: "checkbox"},
	{Key: ColLaunchedChannel, Name: "Launched Channel", Type: "channel"},
	{Key: ColProtected, Name: "Protected", Type: "checkbox"},
}

// Column describes one column in a Slack List schema.
type Column struct {
	Key             string `json:"key"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	IsPrimaryColumn bool   `json:"is_primary_column,omitempty"`
}

// schemaColumn is the server-side column representation (includes the
// assigned `id` / column_id, which is required to write cells).
type schemaColumn struct {
	ID              string `json:"id"`
	Key             string `json:"key"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	IsPrimaryColumn bool   `json:"is_primary_column"`
}

// Entry is one mirage-slack environment row.
type Entry struct {
	ItemID          string
	Name            string
	Endpoint        string
	Launched        bool
	LaunchedChannel string
	Protected       bool
}

// SlackListClient is a thin wrapper around the slackLists.* Web API methods
// plus a handful of files.* calls that we use to discover the bot-owned list.
//
// slack-go/slack does not expose the Lists API yet (2026-04), so we call the
// raw endpoints with a bot token.
type SlackListClient struct {
	token    string
	http     *http.Client
	listName string

	mu        sync.RWMutex
	listID    string
	botUserID string
	teamID    string
	teamURL   string            // e.g. "https://example.slack.com/"
	columnIDs map[string]string // key -> column_id
}

// NewSlackListClient constructs a client. Call Ensure before any read/write
// operation to populate list_id / column IDs. listName must be non-empty;
// Config.applyDefaults derives it from the configured slash command name.
func NewSlackListClient(token, listName string) *SlackListClient {
	return &SlackListClient{
		token:    token,
		listName: listName,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

// ListName returns the configured list name (primary identifier).
func (c *SlackListClient) ListName() string { return c.listName }

// ListID returns the resolved list_id (available after Ensure).
func (c *SlackListClient) ListID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.listID
}

// Ensure makes sure mirage-slack has a usable list:
//  1. auth.test → bot user_id + team_id + team url
//  2. files.list?types=lists → find bot-owned list whose title matches
//  3. if not found, slackLists.create with the mirage-slack schema
//  4. cache list_id + column_id map + permalink components on the client
//
// Emits INFO logs at every step so a slow startup (typically caused by
// files.list scanning many lists in a large workspace) is visible.
func (c *SlackListClient) Ensure(ctx context.Context) (string, error) {
	if c.ListID() != "" {
		return c.ListID(), nil
	}

	slog.Info("ensure: starting", "list_name", c.listName)

	slog.Info("ensure: calling auth.test")
	auth, err := c.authTest(ctx)
	if err != nil {
		return "", fmt.Errorf("auth.test: %w", err)
	}
	slog.Info("ensure: auth.test ok",
		"bot_user_id", auth.UserID,
		"team_id", auth.TeamID,
		"team_url", auth.URL,
	)

	slog.Info("ensure: searching bot-owned list (files.list types=lists)",
		"list_name", c.listName,
		"bot_user_id", auth.UserID,
	)
	existing, schema, err := c.findOwnedList(ctx, auth.UserID, c.listName)
	if err != nil {
		return "", fmt.Errorf("find owned list: %w", err)
	}

	if existing == "" {
		slog.Info("ensure: bot-owned list not found; creating new one", "list_name", c.listName)
		newID, newSchema, err := c.createList(ctx, c.listName, Schema)
		if err != nil {
			return "", fmt.Errorf("create list: %w", err)
		}
		slog.Info("ensure: created new list", "list_id", newID)
		existing, schema = newID, newSchema
	} else {
		slog.Info("ensure: found existing bot-owned list", "list_id", existing)
	}

	slog.Info("ensure: validating list schema", "list_id", existing)
	if err := validateSchema(schema); err != nil {
		return "", fmt.Errorf("list schema: %w", err)
	}

	c.mu.Lock()
	c.listID = existing
	c.botUserID = auth.UserID
	c.teamID = auth.TeamID
	c.teamURL = auth.URL
	c.columnIDs = columnIDsByKey(schema)
	c.mu.Unlock()
	slog.Info("ensure: done", "list_id", existing)
	return existing, nil
}

// authTestResult is the subset of auth.test fields mirage-slack consumes.
type authTestResult struct {
	UserID string `json:"user_id"`
	TeamID string `json:"team_id"`
	URL    string `json:"url"`
}

// authTest returns the bot's user_id plus the team URL needed to build list
// permalinks.
func (c *SlackListClient) authTest(ctx context.Context) (authTestResult, error) {
	var resp authTestResult
	if err := c.call(ctx, "auth.test", map[string]any{}, &resp); err != nil {
		return authTestResult{}, err
	}
	if resp.UserID == "" {
		return authTestResult{}, fmt.Errorf("auth.test: response missing user_id")
	}
	return resp, nil
}

// ListPermalink returns the Slack URL for the bot-owned list. Posting this URL
// in a message triggers Slack's automatic unfurl.
func (c *SlackListClient) ListPermalink() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.teamURL == "" || c.teamID == "" || c.listID == "" {
		return ""
	}
	return strings.TrimRight(c.teamURL, "/") + "/lists/" + c.teamID + "/" + c.listID
}

// findOwnedList searches files.list?types=lists for a list whose creator is
// botUserID and whose title matches exactly.
func (c *SlackListClient) findOwnedList(ctx context.Context, botUserID, title string) (string, []schemaColumn, error) {
	// files.list uses form-urlencoded via query params.
	q := url.Values{}
	q.Set("types", "lists")
	q.Set("count", "200")

	var resp struct {
		Files []struct {
			ID           string `json:"id"`
			User         string `json:"user"`
			Title        string `json:"title"`
			ListMetadata struct {
				Schema []schemaColumn `json:"schema"`
			} `json:"list_metadata"`
		} `json:"files"`
	}
	if err := c.callForm(ctx, "files.list", q, &resp); err != nil {
		return "", nil, err
	}
	for _, f := range resp.Files {
		if f.User == botUserID && f.Title == title {
			return f.ID, f.ListMetadata.Schema, nil
		}
	}
	return "", nil, nil
}

// createList calls slackLists.create and returns the new list_id + server schema.
func (c *SlackListClient) createList(ctx context.Context, name string, schema []Column) (string, []schemaColumn, error) {
	var resp struct {
		ListID       string `json:"list_id"`
		ListMetadata struct {
			Schema []schemaColumn `json:"schema"`
		} `json:"list_metadata"`
	}
	body := map[string]any{
		"name":   name,
		"schema": schema,
	}
	if err := c.call(ctx, "slackLists.create", body, &resp); err != nil {
		return "", nil, err
	}
	if resp.ListID == "" {
		return "", nil, fmt.Errorf("slackLists.create: response missing list_id")
	}
	return resp.ListID, resp.ListMetadata.Schema, nil
}

// validateSchema ensures every mirage-slack key is present in the live schema.
func validateSchema(live []schemaColumn) error {
	want := map[string]string{}
	for _, c := range Schema {
		want[c.Key] = c.Type
	}
	have := map[string]string{}
	for _, c := range live {
		have[c.Key] = c.Type
	}
	for k, ty := range want {
		got, ok := have[k]
		if !ok {
			return fmt.Errorf("missing column key=%q", k)
		}
		if got != ty {
			return fmt.Errorf("column key=%q: want type %q, got %q", k, ty, got)
		}
	}
	return nil
}

func columnIDsByKey(schema []schemaColumn) map[string]string {
	m := make(map[string]string, len(schema))
	for _, c := range schema {
		m[c.Key] = c.ID
	}
	return m
}

// columnID looks up the column_id for the given key. Ensure must have run.
func (c *SlackListClient) columnID(key string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.columnIDs[key]
	if !ok {
		return "", fmt.Errorf("column_id for key %q not available; was Ensure called?", key)
	}
	return id, nil
}

// itemField mirrors the per-cell payload of slackLists.items.list. Slack
// returns different subfields depending on column type:
//
//	text (rich_text): `text` holds the flattened plain text
//	link:             `link[]` with `originalUrl`
//	checkbox:         `checkbox` bool
//	channel:          `channel[]` of channel IDs
type itemField struct {
	Key      string `json:"key"`
	ColumnID string `json:"column_id"`
	Text     string `json:"text"`
	Link     []struct {
		OriginalURL  string `json:"originalUrl"`
		DisplayAsURL bool   `json:"displayAsUrl"`
		DisplayName  string `json:"display_name,omitempty"`
	} `json:"link"`
	Checkbox bool     `json:"checkbox"`
	Channel  []string `json:"channel"`
}

// ListEntries returns every mirage-slack environment row.
func (c *SlackListClient) ListEntries(ctx context.Context) ([]Entry, error) {
	var resp struct {
		Items []struct {
			ID     string      `json:"id"`
			Fields []itemField `json:"fields"`
		} `json:"items"`
	}
	body := map[string]any{"list_id": c.ListID()}
	if err := c.call(ctx, "slackLists.items.list", body, &resp); err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(resp.Items))
	for _, item := range resp.Items {
		e := Entry{ItemID: item.ID}
		for _, f := range item.Fields {
			switch f.Key {
			case ColName:
				e.Name = f.Text
			case ColEndpoint:
				if len(f.Link) > 0 {
					e.Endpoint = f.Link[0].OriginalURL
				}
			case ColLaunched:
				e.Launched = f.Checkbox
			case ColLaunchedChannel:
				if len(f.Channel) > 0 {
					e.LaunchedChannel = f.Channel[0]
				}
			case ColProtected:
				e.Protected = f.Checkbox
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Register creates a new entry with the given endpoint, or updates an existing
// entry's endpoint while preserving its launch state.
func (c *SlackListClient) Register(ctx context.Context, name, endpoint string) error {
	existing, err := c.findByName(ctx, name)
	if err != nil {
		return err
	}
	if existing == nil {
		return c.createEntry(ctx, Entry{Name: name, Endpoint: endpoint})
	}
	updated := *existing
	updated.Endpoint = endpoint
	return c.updateEntry(ctx, updated)
}

// Launch binds an already-registered entry to the given channel and flips
// its launched flag on. Rejects the operation if the channel is already
// bound to a different entry (v1 rule: one launched entry per channel).
func (c *SlackListClient) Launch(ctx context.Context, name, channel string, protect bool) error {
	entries, err := c.ListEntries(ctx)
	if err != nil {
		return err
	}
	var existing *Entry
	for i := range entries {
		e := entries[i]
		if e.Name == name {
			existing = &e
			continue
		}
		if e.Launched && e.LaunchedChannel == channel {
			return fmt.Errorf("channel <#%s> is already bound to %q; terminate it first", channel, e.Name)
		}
	}
	if existing == nil {
		return fmt.Errorf("not registered: %q (run `register` first)", name)
	}
	updated := *existing
	updated.Launched = true
	updated.LaunchedChannel = channel
	updated.Protected = protect
	return c.updateEntry(ctx, updated)
}

// Terminate clears the launch state of a registered entry.
func (c *SlackListClient) Terminate(ctx context.Context, name string) error {
	existing, err := c.findByName(ctx, name)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("not registered: %q", name)
	}
	updated := *existing
	updated.Launched = false
	updated.LaunchedChannel = ""
	return c.updateEntry(ctx, updated)
}

// GrantViewAccessToChannel shares the bot-owned list with the given channel
// as "Can view" (read-only). Idempotent: Slack treats the call as upsert.
// Needed so Slack's automatic unfurl of the permalink works for channel
// members who don't otherwise have access to the bot-owned list.
func (c *SlackListClient) GrantViewAccessToChannel(ctx context.Context, channelID string) error {
	body := map[string]any{
		"list_id":      c.ListID(),
		"access_level": "read",
		"channel_ids":  []string{channelID},
	}
	return c.call(ctx, "slackLists.access.set", body, nil)
}

// DeleteList removes a bot-owned Slack List via files.delete. Refuses to
// delete the currently active list (file_id equal to ListID()) so the caller
// cannot accidentally wipe the live entries. slackLists.delete returns
// unknown_method, but files.delete works against list-type files.
func (c *SlackListClient) DeleteList(ctx context.Context, fileID string) error {
	if fileID == "" {
		return fmt.Errorf("file_id is required")
	}
	if fileID == c.ListID() {
		return fmt.Errorf("refuse to delete the active list (file_id=%s); swap slack.list_name to a different title first", fileID)
	}
	body := map[string]any{"file": fileID}
	return c.call(ctx, "files.delete", body, nil)
}

// Unregister deletes the entry by name.
func (c *SlackListClient) Unregister(ctx context.Context, name string) error {
	existing, err := c.findByName(ctx, name)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("not registered: %q", name)
	}
	body := map[string]any{
		"list_id": c.ListID(),
		"id":      existing.ItemID,
	}
	return c.call(ctx, "slackLists.items.delete", body, nil)
}

func (c *SlackListClient) createEntry(ctx context.Context, e Entry) error {
	fields, err := c.fieldsForEntry(e)
	if err != nil {
		return err
	}
	body := map[string]any{
		"list_id":        c.ListID(),
		"initial_fields": fields,
	}
	return c.call(ctx, "slackLists.items.create", body, nil)
}

func (c *SlackListClient) updateEntry(ctx context.Context, e Entry) error {
	fields, err := c.fieldsForEntry(e)
	if err != nil {
		return err
	}
	// slackLists.items.update identifies rows via row_id on each cell.
	for i := range fields {
		fields[i]["row_id"] = e.ItemID
	}
	body := map[string]any{
		"list_id": c.ListID(),
		"cells":   fields,
	}
	return c.call(ctx, "slackLists.items.update", body, nil)
}

func (c *SlackListClient) findByName(ctx context.Context, name string) (*Entry, error) {
	entries, err := c.ListEntries(ctx)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i], nil
		}
	}
	return nil, nil
}

// fieldsForEntry builds the column_id-keyed cell payload for items.create /
// items.update, matching Slack Lists' per-type value schemas:
//
//	text:     rich_text (nested rich_text > rich_text_section > text)
//	link:     link [{original_url}]
//	checkbox: checkbox bool
//	channel:  channel []string
func (c *SlackListClient) fieldsForEntry(e Entry) ([]map[string]any, error) {
	nameID, err := c.columnID(ColName)
	if err != nil {
		return nil, err
	}
	endpointID, err := c.columnID(ColEndpoint)
	if err != nil {
		return nil, err
	}
	launchedID, err := c.columnID(ColLaunched)
	if err != nil {
		return nil, err
	}
	launchedChannelID, err := c.columnID(ColLaunchedChannel)
	if err != nil {
		return nil, err
	}
	protectedID, err := c.columnID(ColProtected)
	if err != nil {
		return nil, err
	}

	channelValue := []string{}
	if e.LaunchedChannel != "" {
		channelValue = []string{e.LaunchedChannel}
	}

	endpointLink := []map[string]any{}
	if e.Endpoint != "" {
		endpointLink = []map[string]any{{"original_url": e.Endpoint}}
	}

	return []map[string]any{
		{"column_id": nameID, "rich_text": richTextValue(e.Name)},
		{"column_id": endpointID, "link": endpointLink},
		{"column_id": launchedID, "checkbox": e.Launched},
		{"column_id": launchedChannelID, "channel": channelValue},
		{"column_id": protectedID, "checkbox": e.Protected},
	}, nil
}

// richTextValue constructs the Slack Block Kit rich_text array used for
// text-type columns. Uses slack-go's typed builders rather than raw maps.
func richTextValue(s string) []*slack.RichTextBlock {
	return []*slack.RichTextBlock{
		slack.NewRichTextBlock("",
			slack.NewRichTextSection(
				slack.NewRichTextSectionTextElement(s, nil),
			),
		),
	}
}

// slackResponseEnvelope captures the ok / error fields present on every Slack
// Web API response.
type slackResponseEnvelope struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error,omitempty"`
	ResponseMetadata struct {
		Messages []string `json:"messages,omitempty"`
	} `json:"response_metadata,omitempty"`
}

func (c *SlackListClient) call(ctx context.Context, method string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body for %s: %w", method, err)
	}

	url := "https://slack.com/api/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request for %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	return c.execute(req, method, out)
}

func (c *SlackListClient) callForm(ctx context.Context, method string, values url.Values, out any) error {
	u := "https://slack.com/api/" + method + "?" + values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.execute(req, method, out)
}

func (c *SlackListClient) execute(req *http.Request, method string, out any) error {
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Warn("close slack api response body", "method", method, "error", err)
		}
	}()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("%s: read response: %w", method, err)
	}

	var env slackResponseEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: decode envelope: %w: body=%s", method, err, string(raw))
	}
	if !env.OK {
		msg := env.Error
		if len(env.ResponseMetadata.Messages) > 0 {
			msg = fmt.Sprintf("%s (%s)", msg, strings.Join(env.ResponseMetadata.Messages, "; "))
		}
		return fmt.Errorf("%s: slack error: %s", method, msg)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s: decode response: %w: body=%s", method, err, string(raw))
		}
	}
	return nil
}
