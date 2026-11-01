package redisstate

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"
)

func TestClientConfigurationAndCancellation(t *testing.T) {
	for _, cfg := range []Config{
		{}, {Network: "tcp", Address: "127.0.0.1:6379"},
		{Network: "tcp", Address: "redis.internal:6379", Namespace: "agent"},
		{Network: "tcp", Address: "redis.internal:6379", Namespace: "agent", TLS: &tls.Config{InsecureSkipVerify: true}}, // #nosec G402 -- Rejection test.
		{Network: "unix", Address: "relative.sock", Namespace: "agent"},
		{Network: "unix", Address: "/tmp/unused.sock", Namespace: "bad:namespace"},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid config accepted: %v", err)
		}
	}
	c, err := New(Config{Network: "unix", Address: "/tmp/incident-unused.sock", Namespace: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	if c.redis.Options().MaxRetries != 0 || !c.redis.Options().ContextTimeoutEnabled {
		t.Fatal("unsafe retry/timeout defaults")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Execute(ctx, Command{Action: "read", ID: "run1"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, cmd := range []Command{{Action: "FLUSHALL", ID: "run1"}, {Action: "read", ID: "../run1"}} {
		if _, err := c.Execute(t.Context(), cmd); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}
