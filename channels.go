package main

import (
	"fmt"
	"sync"
)

// Channel is a communication endpoint (CLI terminal, Telegram chat, etc.)
type Channel interface {
	ID() string
	Send(text string) error
	Status(text, level string) error
	Close()
}

// ChannelFactory creates a channel on demand for an unknown channel ID.
type ChannelFactory func(id string) Channel

// ChannelRegistry manages all active channels for an instance.
type ChannelRegistry struct {
	mu        sync.RWMutex
	channels  map[string]Channel
	factories []ChannelFactory
}

func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{
		channels: make(map[string]Channel),
	}
}

func (r *ChannelRegistry) AddFactory(f ChannelFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories = append(r.factories, f)
}

func (r *ChannelRegistry) Register(ch Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[ch.ID()] = ch
}

func (r *ChannelRegistry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.channels[id]; ok {
		ch.Close()
		delete(r.channels, id)
	}
}

func (r *ChannelRegistry) Get(id string) Channel {
	r.mu.RLock()
	ch, ok := r.channels[id]
	r.mu.RUnlock()
	if ok {
		return ch
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.channels[id]; ok {
		return ch
	}
	for _, f := range r.factories {
		if ch := f(id); ch != nil {
			r.channels[id] = ch
			return ch
		}
	}
	return nil
}

func (r *ChannelRegistry) List() []Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Channel, 0, len(r.channels))
	for _, ch := range r.channels {
		out = append(out, ch)
	}
	return out
}

// ChannelIDs returns registered channel ids (regardless of IsActive
// state). Useful for debug logs that want to distinguish "channel
// doesn't exist" from "channel exists but is silently inactive".
func (r *ChannelRegistry) ChannelIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.channels))
	for id := range r.channels {
		ids = append(ids, id)
	}
	return ids
}

func (r *ChannelRegistry) Send(channelID, text string) error {
	ch := r.Get(channelID)
	if ch == nil {
		return fmt.Errorf("channel %q not found", channelID)
	}
	return ch.Send(text)
}

func (r *ChannelRegistry) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.channels {
		ch.Close()
	}
	r.channels = make(map[string]Channel)
}

// InstanceChannels holds all channel infrastructure for a running instance.
type InstanceChannels struct {
	registry *ChannelRegistry
	mcp      *channelMCPServer
	telegram *TelegramGateway
	cli      *CLIBridge
}

// activeChannel is an optional interface channels may implement to
// gate their presence in the available-channels list. When missing,
// a channel is considered always active (today's default for slack,
// email, telegram proper). When present, the channel only appears in
// AvailableChannels() while IsActive() returns true — letting the
// chat channel say "I'm not advertised unless a dashboard is open".
// Keeps the agent from reflexively responding on channels where no
// human is listening (e.g. an inject/admin event shouldn't get a
// chat reply just because chat was registered with the instance).
type activeChannel interface {
	IsActive() bool
}

// AvailableChannels returns channel IDs for channels that are actually
// connected and can receive messages right now. The channels MCP uses
// this at tool-call time to gate respond/status with a clean rejection
// when nobody's listening.
func (ic *InstanceChannels) AvailableChannels() []string {
	var ids []string
	if ic.cli != nil && ic.cli.IsConnected() {
		ids = append(ids, "cli")
	}
	if ic.telegram != nil {
		ids = append(ids, "telegram (bot @"+ic.telegram.BotName()+")")
	}
	for _, ch := range ic.registry.List() {
		id := ch.ID()
		if id == "cli" {
			continue
		}
		// activeChannel gate — DB-backed channels like chat report
		// "no one's listening right now" so the agent doesn't see
		// them as valid respond targets when no UI is open.
		if ac, ok := ch.(activeChannel); ok && !ac.IsActive() {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// RegisteredChannels returns every channel id the instance knows
// about, regardless of whether it's live right now. Used by the
// channels MCP's tool description so the respond tool's schema stays
// stable across an instance's lifetime — MCP clients cache the
// tools/list result after initialize and don't re-fetch, so a
// description computed from AvailableChannels at boot would be
// permanently stale the moment a chat tab opens or closes. The agent
// still gets the accurate "is this channel live?" signal at call
// time, where AvailableChannels is consulted and a clear rejection
// comes back for dead channels.
func (ic *InstanceChannels) RegisteredChannels() []string {
	var ids []string
	if ic.cli != nil {
		ids = append(ids, "cli")
	}
	if ic.telegram != nil {
		ids = append(ids, "telegram (bot @"+ic.telegram.BotName()+")")
	}
	for _, ch := range ic.registry.List() {
		id := ch.ID()
		if id == "cli" {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// Stop shuts down all channels for an instance.
func (ic *InstanceChannels) Stop() {
	if ic.telegram != nil {
		ic.telegram.Stop()
	}
	if ic.mcp != nil {
		ic.mcp.close()
	}
	if ic.registry != nil {
		ic.registry.CloseAll()
	}
}
