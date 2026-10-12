package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/ha"
)

// Bootstrap environment variables of active-passive high availability (see
// docs/design/ha.md).
const (
	// EnvHAMode is "off" (the default) or "active-passive".
	EnvHAMode = "MONGORESCUE_HA_MODE"
	// EnvHATarget is the storage target of the lease and the metadata handover: a
	// target ID or name, or a location URL (s3://bucket/prefix?endpoint=… or
	// file:///path).
	EnvHATarget = "MONGORESCUE_HA_TARGET"
	// EnvHACluster is the cluster name (default "default").
	EnvHACluster = "MONGORESCUE_HA_CLUSTER"
	// EnvHALeaseTTL is how long the lease lives without renewal (default 30s).
	EnvHALeaseTTL = "MONGORESCUE_HA_LEASE_TTL"
	// EnvHARenewInterval is how often the lease is renewed or read (default 10s).
	EnvHARenewInterval = "MONGORESCUE_HA_RENEW_INTERVAL"
	// EnvHASnapshotInterval is the longest time between two metadata snapshots of
	// the active instance (default 5m).
	EnvHASnapshotInterval = "MONGORESCUE_HA_SNAPSHOT_INTERVAL"
)

// HA modes.
const (
	// HAModeOff runs a single instance (the default).
	HAModeOff = "off"
	// HAModeActivePassive runs the instance as a member of an active-passive cluster.
	HAModeActivePassive = "active-passive"
)

// Defaults and bounds of the HA settings.
const (
	// DefaultHACluster is the cluster name when none is set.
	DefaultHACluster = "default"
	// DefaultHASnapshotInterval is the default snapshot interval.
	DefaultHASnapshotInterval = 5 * time.Minute
	// MinHASnapshotInterval and MaxHASnapshotInterval bound it.
	MinHASnapshotInterval = 30 * time.Second
	MaxHASnapshotInterval = 24 * time.Hour
)

// ErrInvalidHA is returned for HA settings out of range.
var ErrInvalidHA = errors.New("config: invalid high availability settings")

// HAConfig holds the active-passive HA settings.
type HAConfig struct {
	// Mode is HAModeOff or HAModeActivePassive.
	Mode string
	// Target is the storage target of the lease and snapshots (see EnvHATarget).
	Target string
	// Cluster is the cluster name (see ha.ValidateCluster).
	Cluster string
	// LeaseTTL and RenewInterval time the lease (see ha.ValidateTiming).
	LeaseTTL      time.Duration
	RenewInterval time.Duration
	// SnapshotInterval is the longest time between two metadata snapshots.
	SnapshotInterval time.Duration
}

// defaultHA returns the HA defaults (off).
func defaultHA() HAConfig {
	return HAConfig{Mode: HAModeOff, Cluster: DefaultHACluster, LeaseTTL: ha.DefaultTTL,
		RenewInterval: ha.DefaultRenew, SnapshotInterval: DefaultHASnapshotInterval}
}

// Enabled reports whether the instance runs in active-passive HA mode.
func (h HAConfig) Enabled() bool { return h.Mode == HAModeActivePassive }

// haFromEnv overrides h with the HA environment variables.
func haFromEnv(h *HAConfig, getenv func(string) string) error {
	if v := strings.TrimSpace(getenv(EnvHAMode)); v != "" {
		h.Mode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(getenv(EnvHATarget)); v != "" {
		h.Target = v
	}
	if v := strings.TrimSpace(getenv(EnvHACluster)); v != "" {
		h.Cluster = v
	}
	for _, d := range []struct {
		env string
		dst *time.Duration
	}{{EnvHALeaseTTL, &h.LeaseTTL}, {EnvHARenewInterval, &h.RenewInterval}, {EnvHASnapshotInterval, &h.SnapshotInterval}} {
		v := strings.TrimSpace(getenv(d.env))
		if v == "" {
			continue
		}
		parsed, err := ParseHADuration(v)
		if err != nil {
			return fmt.Errorf("%s: %w", d.env, err)
		}
		*d.dst = parsed
	}
	return nil
}

// ParseHADuration parses a Go duration of an HA setting, such as "30s".
func ParseHADuration(v string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%w: %q is not a positive duration such as 30s", ErrInvalidHA, v)
	}
	return d, nil
}

// Validate checks the HA settings. With HA off only the mode is checked.
func (h HAConfig) Validate() error {
	switch h.Mode {
	case HAModeOff, "":
		return nil
	case HAModeActivePassive:
	default:
		return fmt.Errorf("%s: %w: mode must be %q or %q", EnvHAMode, ErrInvalidHA, HAModeOff, HAModeActivePassive)
	}
	if strings.TrimSpace(h.Target) == "" {
		return fmt.Errorf("%s: %w: active-passive mode needs the storage target of the lease", EnvHATarget, ErrInvalidHA)
	}
	if _, err := ParseHATarget(h.Target); err != nil {
		return fmt.Errorf("%s: %w", EnvHATarget, err)
	}
	if err := ha.ValidateCluster(h.Cluster); err != nil {
		return fmt.Errorf("%s: %w: %w", EnvHACluster, ErrInvalidHA, err)
	}
	if err := ha.ValidateTiming(h.LeaseTTL, h.RenewInterval); err != nil {
		return fmt.Errorf("%s/%s: %w: %w", EnvHALeaseTTL, EnvHARenewInterval, ErrInvalidHA, err)
	}
	if h.SnapshotInterval < MinHASnapshotInterval || h.SnapshotInterval > MaxHASnapshotInterval {
		return fmt.Errorf("%s: %w: the snapshot interval must be %s to %s", EnvHASnapshotInterval, ErrInvalidHA, MinHASnapshotInterval, MaxHASnapshotInterval)
	}
	return nil
}

// HATarget is a parsed EnvHATarget: a reference to a storage target of the
// database (Ref), or a location given in the bootstrap configuration.
type HATarget struct {
	// Ref is a storage target ID or name ("" for a location).
	Ref string
	// Scheme is "s3" or "file" for a location.
	Scheme string
	// Bucket and Prefix locate an S3 location; Endpoint, Region and PathStyle
	// configure its client. Credentials come from the AWS default chain.
	Bucket    string
	Prefix    string
	Endpoint  string
	Region    string
	PathStyle bool
	// Path is the absolute directory of a file location.
	Path string
}

// ParseHATarget parses a storage target reference or location URL:
// s3://bucket[/prefix][?endpoint=URL&region=R&path_style=true] or file:///path.
// Anything without "://" is a target ID or name.
func ParseHATarget(v string) (HATarget, error) {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "://") {
		if v == "" {
			return HATarget{}, fmt.Errorf("%w: empty storage target", ErrInvalidHA)
		}
		return HATarget{Ref: v}, nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return HATarget{}, fmt.Errorf("%w: the storage location is not a valid URL", ErrInvalidHA)
	}
	if u.User != nil {
		return HATarget{}, fmt.Errorf("%w: the storage location must not contain credentials; use the AWS default credential chain", ErrInvalidHA)
	}
	switch u.Scheme {
	case "s3":
		if u.Host == "" {
			return HATarget{}, fmt.Errorf("%w: s3:// needs a bucket, as in s3://bucket/prefix", ErrInvalidHA)
		}
		q := u.Query()
		t := HATarget{Scheme: "s3", Bucket: u.Host, Prefix: strings.Trim(u.Path, "/"), Endpoint: q.Get("endpoint"), Region: q.Get("region")}
		if ps := q.Get("path_style"); ps != "" {
			t.PathStyle = ps == "true" || ps == "1"
		}
		if t.Endpoint != "" {
			eu, perr := url.Parse(t.Endpoint)
			if perr != nil || (eu.Scheme != "http" && eu.Scheme != "https") || eu.Host == "" {
				return HATarget{}, fmt.Errorf("%w: the endpoint must be an http(s) URL", ErrInvalidHA)
			}
		}
		return t, nil
	case "file":
		// file:///C:/dir on Windows has the path "/C:/dir".
		if len(u.Path) > 2 && u.Path[0] == '/' && u.Path[2] == ':' {
			u.Path = u.Path[1:]
		}
		if u.Host != "" || !filepath.IsAbs(filepath.FromSlash(u.Path)) {
			return HATarget{}, fmt.Errorf("%w: file:// needs an absolute path, as in file:///shared/mongorescue-ha", ErrInvalidHA)
		}
		return HATarget{Scheme: "file", Path: filepath.Clean(filepath.FromSlash(u.Path))}, nil
	}
	return HATarget{}, fmt.Errorf("%w: unsupported storage location scheme %q (s3 or file)", ErrInvalidHA, u.Scheme)
}
