package mirageslack

import (
	"context"
	"strings"
)

type routeDecision int

const (
	// routeInternal targets mirage-slack's own subcommand processing.
	routeInternal routeDecision = iota
	// routeForwardEntry forwards to a launched entry's endpoint.
	routeForwardEntry
	// routeForwardDefault forwards to config.routing.default_endpoint.
	routeForwardDefault
	// routeNotLaunched has no target; caller responds with an ephemeral error.
	routeNotLaunched
)

type routeResult struct {
	Decision routeDecision
	Entry    *Entry
	Protect  bool
}

// decideRoute walks the design flow in docs/design.md §6.2:
//
//  1. /mirage-slack own command?           → internal
//  2. channel matches a launched entry?    → forward entry (+protect if flagged)
//  3. default_endpoint configured?         → forward default (+protect per config)
//  4. otherwise                            → not-launched
func (a *App) decideRoute(ctx context.Context, commandName, channelID string) (routeResult, error) {
	if commandName != "" && strings.EqualFold(commandName, a.cfg.Command.Name) {
		return routeResult{Decision: routeInternal}, nil
	}

	if channelID != "" {
		entries, err := a.list.ListEntries(ctx)
		if err != nil {
			return routeResult{}, err
		}
		for i := range entries {
			e := entries[i]
			if e.Launched && e.LaunchedChannel == channelID {
				return routeResult{
					Decision: routeForwardEntry,
					Entry:    &e,
					Protect:  e.Protected,
				}, nil
			}
		}
	}

	if a.cfg.Routing.DefaultEndpoint != "" {
		return routeResult{
			Decision: routeForwardDefault,
			Protect:  a.cfg.DefaultEndpointProtectEnabled(),
		}, nil
	}

	return routeResult{Decision: routeNotLaunched}, nil
}
