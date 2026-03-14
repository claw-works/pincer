package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
)

// PubSub is the interface for cross-instance message fan-out.
// Nil implementation is used when REDIS_URL is not set (single-instance mode).
type PubSub interface {
	// Publish sends a message to the channel for the target agent.
	Publish(ctx context.Context, agentID string, msg Message) error
	// Subscribe registers a local deliver callback for the given agent.
	Subscribe(ctx context.Context, deliver func(Message), agentIDs ...string) error
	// Unsubscribe removes subscriptions for the given agent IDs.
	Unsubscribe(ctx context.Context, agentIDs ...string) error
	// Close cleans up resources.
	Close() error
}

// nopPubSub does nothing (single-instance fallback).
type nopPubSub struct{}

func (nopPubSub) Publish(_ context.Context, _ string, _ Message) error      { return nil }
func (nopPubSub) Subscribe(_ context.Context, _ func(Message), _ ...string) error { return nil }
func (nopPubSub) Unsubscribe(_ context.Context, _ ...string) error           { return nil }
func (nopPubSub) Close() error                                               { return nil }

// channelName returns the Redis pub/sub channel name for an agent.
func channelName(agentID string) string {
	return fmt.Sprintf("pincer:msg:%s", agentID)
}

// redisPubSub implements PubSub using Redis.
// It maintains a single *redis.PubSub subscription that accumulates channels,
// and dispatches received messages to per-agent deliver callbacks.
type redisPubSub struct {
	client    *redis.Client
	mu        sync.RWMutex
	sub       *redis.PubSub
	callbacks map[string]func(Message) // agentID → deliver func
}

// NewRedisPubSub creates a Redis-backed PubSub from a Redis URL.
// Returns nopPubSub if url is empty.
func NewRedisPubSub(redisURL string) PubSub {
	if redisURL == "" {
		return nopPubSub{}
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Printf("hub/pubsub: invalid REDIS_URL %q: %v — falling back to nop", redisURL, err)
		return nopPubSub{}
	}
	c := redis.NewClient(opt)
	// Open a persistent subscription (no channels yet).
	sub := c.Subscribe(context.Background())
	r := &redisPubSub{
		client:    c,
		sub:       sub,
		callbacks: make(map[string]func(Message)),
	}
	go r.readLoop()
	log.Printf("hub/pubsub: Redis pub/sub enabled (%s)", opt.Addr)
	return r
}

func (r *redisPubSub) readLoop() {
	ch := r.sub.Channel()
	for payload := range ch {
		var msg Message
		if err := json.Unmarshal([]byte(payload.Payload), &msg); err != nil {
			log.Printf("hub/pubsub: unmarshal error: %v", err)
			continue
		}
		r.mu.RLock()
		deliver, ok := r.callbacks[msg.To]
		r.mu.RUnlock()
		if ok {
			deliver(msg)
		}
	}
}

func (r *redisPubSub) Publish(ctx context.Context, agentID string, msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("pubsub publish marshal: %w", err)
	}
	return r.client.Publish(ctx, channelName(agentID), data).Err()
}

func (r *redisPubSub) Subscribe(ctx context.Context, deliver func(Message), agentIDs ...string) error {
	if len(agentIDs) == 0 {
		return nil
	}
	channels := make([]string, len(agentIDs))
	for i, id := range agentIDs {
		channels[i] = channelName(id)
	}
	r.mu.Lock()
	for _, id := range agentIDs {
		r.callbacks[id] = deliver
	}
	r.mu.Unlock()
	return r.sub.Subscribe(ctx, channels...)
}

func (r *redisPubSub) Unsubscribe(ctx context.Context, agentIDs ...string) error {
	channels := make([]string, len(agentIDs))
	r.mu.Lock()
	for i, id := range agentIDs {
		channels[i] = channelName(id)
		delete(r.callbacks, id)
	}
	r.mu.Unlock()
	return r.sub.Unsubscribe(ctx, channels...)
}

func (r *redisPubSub) Close() error {
	r.sub.Close()
	return r.client.Close()
}
