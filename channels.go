package main

import (
	"fmt"
	"sync"
)

// Channel is a communication endpoint (CLI terminal, Telegram chat, etc.)
type Channel interface {
	ID() string
	Send(text string) error
	Ask(question string) (string, error)
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

func (r *ChannelRegistry) Send(channelID, text string) error {
	ch := r.Get(channelID)
	if ch == nil {
		return fmt.Errorf("channel %q not found", channelID)
	}
	return ch.Send(text)
}

func (r *ChannelRegistry) Ask(channelID, question string) (string, error) {
	ch := r.Get(channelID)
	if ch == nil {
		return "", fmt.Errorf("channel %q not found", channelID)
	}
	return ch.Ask(question)
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

// AvailableChannels returns channel IDs including gateway-level channels.
func (ic *InstanceChannels) AvailableChannels() []string {
	ids := []string{"cli"}
	if ic.telegram != nil {
		ids = append(ids, "telegram (bot @"+ic.telegram.BotName()+")")
	}
	// Also include any per-chat channels already in registry
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
