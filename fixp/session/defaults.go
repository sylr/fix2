package session

import (
	"time"

	"github.com/sylr/fix2/fixp"
)

// Defaults of Config and its roles' configurations.
const (
	defaultKeepalive          = time.Second
	defaultLeniency           = 1.5
	defaultWriteTimeout       = 10 * time.Second
	defaultStoreTimeout       = 10 * time.Second
	defaultMaxFrameLength     = 1 << 20
	defaultMaxPendingMessages = 4096
	defaultMaxPendingBytes    = 16 << 20
	defaultMaxHeldMessages    = 4096
	defaultMaxHeldBytes       = 16 << 20
	defaultRetransmitBatch    = 1000
	defaultMaxReasonLength    = 256

	defaultReconnectMin = time.Second
	defaultReconnectMax = 30 * time.Second
	defaultDialTimeout  = 10 * time.Second

	defaultMinKeepalive          = 100 * time.Millisecond
	defaultMaxKeepalive          = time.Minute
	defaultMaxClockSkew          = time.Minute
	defaultRetention             = 7 * 24 * time.Hour
	defaultMaxSessions           = 1024
	defaultUnestablishedTimeout  = time.Minute
	defaultMaxCredentialsLength  = 1024
	defaultMaxFirstFrameLength   = 4096
	defaultMaxPendingConnections = 1024
)

// maxKeepalive is the longest keepalive interval the wire holds: a uint32
// of milliseconds.
const maxKeepalive = time.Duration(1<<32-1) * time.Millisecond

func orDuration(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

func orInt(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}

// resolve returns c with its defaults applied, or a *ConfigError.
func (c Config) resolve() (Config, error) {
	if c.Handler == nil {
		return c, &ConfigError{"Handler", "required"}
	}
	c.KeepaliveInterval = orDuration(c.KeepaliveInterval, defaultKeepalive)
	if c.KeepaliveInterval < time.Millisecond || c.KeepaliveInterval > maxKeepalive {
		return c, &ConfigError{"KeepaliveInterval", "between 1ms and 2^32-1 ms"}
	}
	if c.Leniency == 0 {
		c.Leniency = defaultLeniency
	}
	if c.Leniency < 1 {
		return c, &ConfigError{"Leniency", "at least 1"}
	}
	c.HandshakeTimeout = orDuration(c.HandshakeTimeout, c.KeepaliveInterval)
	c.WriteTimeout = orDuration(c.WriteTimeout, defaultWriteTimeout)
	c.StoreTimeout = orDuration(c.StoreTimeout, defaultStoreTimeout)
	c.MaxFrameLength = orInt(c.MaxFrameLength, defaultMaxFrameLength)
	c.MaxPendingMessages = orInt(c.MaxPendingMessages, defaultMaxPendingMessages)
	c.MaxPendingBytes = orInt(c.MaxPendingBytes, defaultMaxPendingBytes)
	c.MaxHeldMessages = orInt(c.MaxHeldMessages, defaultMaxHeldMessages)
	c.MaxHeldBytes = orInt(c.MaxHeldBytes, defaultMaxHeldBytes)
	c.RetransmitBatch = orInt(c.RetransmitBatch, defaultRetransmitBatch)
	c.MaxReasonLength = orInt(c.MaxReasonLength, defaultMaxReasonLength)
	for _, v := range []struct {
		name string
		d    time.Duration
	}{{"HandshakeTimeout", c.HandshakeTimeout}, {"WriteTimeout", c.WriteTimeout}, {"StoreTimeout", c.StoreTimeout}} {
		if v.d < 0 {
			return c, &ConfigError{v.name, "negative"}
		}
	}
	for _, v := range []struct {
		name string
		n    int
	}{
		{"MaxFrameLength", c.MaxFrameLength}, {"MaxPendingMessages", c.MaxPendingMessages},
		{"MaxPendingBytes", c.MaxPendingBytes}, {"MaxHeldMessages", c.MaxHeldMessages},
		{"MaxHeldBytes", c.MaxHeldBytes}, {"RetransmitBatch", c.RetransmitBatch},
		{"MaxReasonLength", c.MaxReasonLength},
	} {
		if v.n < 0 {
			return c, &ConfigError{v.name, "negative"}
		}
	}
	if c.MaxFrameLength < 256 {
		return c, &ConfigError{"MaxFrameLength", "at least 256 bytes"}
	}
	if c.MaxReasonLength > 65534 {
		return c, &ConfigError{"MaxReasonLength", "65534 bytes at most, what a reason's uint16 length holds"}
	}
	return c, nil
}

func validFlow(f fixp.FlowType) bool { return f <= fixp.None }

// flowsAllowed reports whether the pair of flows may be negotiated: not
// None both ways (§3.4.4.1).
func flowsAllowed(client, server fixp.FlowType) bool {
	return validFlow(client) && validFlow(server) && !(client == fixp.None && server == fixp.None)
}

func (c ClientConfig) resolve() (ClientConfig, error) {
	var err error
	if c.Config, err = c.Config.resolve(); err != nil {
		return c, err
	}
	if !validFlow(c.Flow) {
		return c, &ConfigError{"Flow", "not a flow type"}
	}
	for _, f := range c.ServerFlows {
		if !validFlow(f) {
			return c, &ConfigError{"ServerFlows", "not a flow type"}
		}
	}
	if c.Store == nil {
		return c, &ConfigError{"Store", "required"}
	}
	c.ReconnectMin = orDuration(c.ReconnectMin, defaultReconnectMin)
	c.ReconnectMax = orDuration(c.ReconnectMax, defaultReconnectMax)
	c.DialTimeout = orDuration(c.DialTimeout, defaultDialTimeout)
	if c.ReconnectMin < 0 || c.ReconnectMax < c.ReconnectMin {
		return c, &ConfigError{"ReconnectMax", "below ReconnectMin"}
	}
	return c, nil
}

func (c ServerConfig) resolve() (ServerConfig, error) {
	var err error
	if c.Config, err = c.Config.resolve(); err != nil {
		return c, err
	}
	if !validFlow(c.Flow) {
		return c, &ConfigError{"Flow", "not a flow type"}
	}
	for _, f := range c.ClientFlows {
		if !validFlow(f) {
			return c, &ConfigError{"ClientFlows", "not a flow type"}
		}
	}
	if c.Store == nil {
		return c, &ConfigError{"Store", "required"}
	}
	if c.Authenticate == nil {
		return c, &ConfigError{"Authenticate", "required"}
	}
	c.MinKeepalive = orDuration(c.MinKeepalive, defaultMinKeepalive)
	c.MaxKeepalive = orDuration(c.MaxKeepalive, defaultMaxKeepalive)
	if c.MinKeepalive < 0 || c.MaxKeepalive < c.MinKeepalive || c.MaxKeepalive > maxKeepalive {
		return c, &ConfigError{"MaxKeepalive", "below MinKeepalive, or beyond 2^32-1 ms"}
	}
	c.MaxClockSkew = orDuration(c.MaxClockSkew, defaultMaxClockSkew)
	c.Retention = orDuration(c.Retention, defaultRetention)
	c.MaxSessions = orInt(c.MaxSessions, defaultMaxSessions)
	c.UnestablishedTimeout = orDuration(c.UnestablishedTimeout, defaultUnestablishedTimeout)
	c.MaxCredentialsLength = orInt(c.MaxCredentialsLength, defaultMaxCredentialsLength)
	c.FirstMessageTimeout = orDuration(c.FirstMessageTimeout, c.HandshakeTimeout)
	c.MaxFirstFrameLength = orInt(c.MaxFirstFrameLength, defaultMaxFirstFrameLength)
	c.MaxPendingConnections = orInt(c.MaxPendingConnections, defaultMaxPendingConnections)
	if c.MaxSessions < 0 || c.MaxCredentialsLength < 0 || c.MaxFirstFrameLength < 64 || c.MaxPendingConnections < 0 ||
		c.MaxClockSkew < 0 || c.UnestablishedTimeout < 0 || c.FirstMessageTimeout < 0 {
		return c, &ConfigError{"ServerConfig", "a negative limit, or MaxFirstFrameLength below 64"}
	}
	return c, nil
}
