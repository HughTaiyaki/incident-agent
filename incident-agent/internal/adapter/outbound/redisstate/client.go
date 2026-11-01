package redisstate

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"net"
	"path/filepath"
	"regexp"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

var (
	// ErrInvalid indicates invalid local configuration or command input.
	ErrInvalid = errors.New("redis state: invalid input")
	// ErrUnavailable means the response was not confirmed; writes may have committed.
	ErrUnavailable = errors.New("redis state: response unconfirmed; read state before continuing")
	// ErrProtocol indicates an unexpected or corrupt persistence response.
	ErrProtocol       = errors.New("redis state: invalid response")
	identifierPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
)

// Config requires an explicit dedicated Redis address and namespace; no defaults
// or environment discovery may accidentally select the mall cache.
type Config struct {
	Network   string
	Address   string
	Namespace string
	Username  string
	Password  string
	TLS       *tls.Config
}

// Client executes the fixed state protocol without automatic command retries.
// It owns its connection pool and must be closed by the application.
type Client struct {
	redis     *redis.Client
	namespace string
}

// Command aliases the application persistence command for existing callers.
type Command = runstate.Command

// State aliases the application persistence snapshot.
type State = runstate.State

// Operation aliases a persisted call record.
type Operation = runstate.Operation

// Reply aliases an atomic persistence response.
type Reply = runstate.Reply

// New constructs a lazy client. No network connection is made until Execute.
// Remote TCP requires verified TLS; plaintext TCP is limited to loopback IPs.
func New(cfg Config) (*Client, error) {
	if !identifierPattern.MatchString(cfg.Namespace) || cfg.Address == "" {
		return nil, ErrInvalid
	}
	switch cfg.Network {
	case "unix":
		if !filepath.IsAbs(cfg.Address) || cfg.TLS != nil {
			return nil, ErrInvalid
		}
	case "tcp":
		host, port, err := net.SplitHostPort(cfg.Address)
		if err != nil || port == "" {
			return nil, ErrInvalid
		}
		ip := net.ParseIP(host)
		if (ip == nil || !ip.IsLoopback()) && cfg.TLS == nil {
			return nil, ErrInvalid
		}
	default:
		return nil, ErrInvalid
	}
	var tlsConfig *tls.Config
	if cfg.TLS != nil {
		if cfg.TLS.InsecureSkipVerify {
			return nil, ErrInvalid
		}
		tlsConfig = cfg.TLS.Clone()
		if tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	r := redis.NewClient(&redis.Options{
		Network: cfg.Network, Addr: cfg.Address, Username: cfg.Username, Password: cfg.Password, TLSConfig: tlsConfig,
		Protocol: 2, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		ContextTimeoutEnabled: true, PoolSize: 2, MaxActiveConns: 2, PoolTimeout: time.Second,
		DisableIdentity: true, MaintNotificationsConfig: &maintnotifications.Config{Mode: "disabled"},
	})
	return &Client{redis: r, namespace: cfg.Namespace}, nil
}

// Close releases the owned pool. It does not delete persisted data.
func (c *Client) Close() error { return c.redis.Close() }

// Execute performs one EVAL with fixed keys and a bounded deadline. Transport
// errors never include server messages, connection addresses or credentials.
func (c *Client) Execute(ctx context.Context, cmd Command) (Reply, error) {
	if err := ctx.Err(); err != nil {
		return Reply{}, err
	}
	if !identifierPattern.MatchString(cmd.ID) {
		return Reply{}, ErrInvalid
	}
	switch cmd.Action {
	case "create", "read", "claim", "renew", "begin", "commit", "complete", "cancel", "checkpoint":
	default:
		return Reply{}, ErrInvalid
	}
	if len(cmd.Request) > 16384 || len(cmd.Input) > 65536 || len(cmd.Result) > 2097152 || len(cmd.Report) > 65536 || len(cmd.Checkpoint) > 3*1024*1024 {
		return Reply{}, ErrInvalid
	}
	data, err := json.Marshal(cmd)
	if err != nil || len(data) > 8*1024*1024 {
		return Reply{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	keys := []string{c.namespace + ":run:" + cmd.ID, c.namespace + ":ready", c.namespace + ":leases"}
	raw, err := c.redis.Eval(ctx, Script, keys, string(data)).Text()
	if err != nil {
		if ctx.Err() != nil {
			return Reply{}, errors.Join(ErrUnavailable, ctx.Err())
		}
		return Reply{}, ErrUnavailable
	}
	if len(raw) > 8*1024*1024 {
		return Reply{}, ErrProtocol
	}
	var reply Reply
	if json.Unmarshal([]byte(raw), &reply) != nil {
		return Reply{}, ErrProtocol
	}
	switch reply.Code {
	case "OK", "EXISTS", "ALREADY", "TERMINAL":
		if reply.State == nil || reply.State.Schema != 1 || reply.State.ID != cmd.ID {
			return Reply{}, ErrProtocol
		}
	case "INVALID", "CAPACITY", "OVER_CAPACITY", "OVER_MEMORY", "WRONGTYPE", "CORRUPT", "UNKNOWN_SCHEMA", "NOT_FOUND", "CONFLICT", "BUSY", "FENCED", "CANCELLED", "DEADLINE", "BUDGET":
	default:
		return Reply{}, ErrProtocol
	}
	return reply, nil
}

// Candidates discovers ready and expired leased runs using Redis time. Claim
// remains authoritative; duplicate candidates and races never confer ownership.
func (c *Client) Candidates(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	values, err := c.redis.Eval(ctx, candidateScript, []string{c.namespace + ":ready", c.namespace + ":leases"}, limit).StringSlice()
	if err != nil {
		return nil, ErrUnavailable
	}
	for _, id := range values {
		if !identifierPattern.MatchString(id) {
			return nil, ErrProtocol
		}
	}
	return values, nil
}

const candidateScript = `local t=redis.call('TIME'); local now=t[1]*1000+math.floor(t[2]/1000)
local a=redis.call('ZRANGEBYSCORE',KEYS[1],'-inf',now,'LIMIT',0,ARGV[1])
local b=redis.call('ZRANGEBYSCORE',KEYS[2],'-inf',now,'LIMIT',0,ARGV[1])
for _,v in ipairs(b) do table.insert(a,v) end; return a`

// Heartbeat records a live worker. It expires on its own, so a crashed worker
// stops counting without any cleanup.
func (c *Client) Heartbeat(ctx context.Context, owner string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.redis.Set(ctx, c.namespace+":worker:heartbeat", owner, 15*time.Second).Err(); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Readiness returns fixed reason codes for what would stop a new run progressing;
// none means ready. Redis detail is never included.
func (c *Client) Readiness(ctx context.Context) []string {
	if err := c.CheckPersistence(ctx); err != nil {
		if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrProtocol) {
			return []string{"redis_unavailable"}
		}
		return []string{"redis_misconfigured"}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	alive, err := c.redis.Exists(ctx, c.namespace+":worker:heartbeat").Result()
	if err != nil {
		return []string{"redis_unavailable"}
	}
	if alive == 0 {
		return []string{"worker_absent"}
	}
	return nil
}

// CheckPersistence rejects configurations that cannot meet the worker's declared
// durability policy. CONFIG GET permission is required; errors fail closed.
func (c *Client) CheckPersistence(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	values, err := c.redis.Do(ctx, "CONFIG", "GET", "appendonly", "appendfsync", "maxmemory-policy", "maxmemory").StringSlice()
	if err != nil {
		return ErrUnavailable
	}
	if len(values) != 8 {
		return ErrProtocol
	}
	settings := map[string]string{}
	for i := 0; i < len(values); i += 2 {
		settings[values[i]] = values[i+1]
	}
	if settings["appendonly"] != "yes" || settings["appendfsync"] != "everysec" || settings["maxmemory-policy"] != "noeviction" {
		return errors.New("redis state: requires appendonly yes, appendfsync everysec and noeviction")
	}
	// Admission reserves headroom against maxmemory; without one it cannot protect anything.
	if settings["maxmemory"] == "0" {
		return errors.New("redis state: requires an explicit maxmemory")
	}
	return nil
}
