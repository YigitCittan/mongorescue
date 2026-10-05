// Package heartbeat sends outbound heartbeat pings to an external dead-man's-switch
// monitor (healthchecks.io or another service that alerts when HTTP pings stop
// arriving). A monitor raises its alert when the pings stop, which
// also covers the failures MongoRescue cannot report itself: a crash, a host that is
// down, a hung scheduler or a full disk.
//
// Two kinds of pings exist. The global heartbeat (settings monitoring.heartbeat_url)
// is pinged every monitoring.heartbeat_interval while the scheduler is healthy. A
// job's own heartbeat (models.Job.HeartbeatURL) follows the healthchecks.io
// protocol: "<url>/start" when a run starts, "<url>" when it succeeds and
// "<url>/fail" when it fails, is partial or is cancelled.
//
// Pings are fire-and-forget: they never block or fail a run, every attempt has a
// timeout, retryable failures are retried, and the pings of one job are sent in
// order. URLs are secrets (they carry the check's token): only their host is ever
// logged or returned in errors.
package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
)

// ErrPingFailed is returned by Test when the monitor could not be reached or did not
// answer with a 2xx status. The message names the URL's host only.
var ErrPingFailed = errors.New("heartbeat: ping failed")

// errPermanent marks a ping failure that is not retried (a 4xx answer other than 408
// and 429, a blocked destination).
var errPermanent = errors.New("permanent")

// Signal is the kind of a ping: the path suffix appended to the URL.
type Signal string

// Ping signals (healthchecks.io protocol).
const (
	// SignalSuccess pings the URL itself: the instance is alive, or a run succeeded.
	SignalSuccess Signal = ""
	// SignalStart pings "<url>/start": a run started.
	SignalStart Signal = "start"
	// SignalFail pings "<url>/fail": a run failed, was partial or was cancelled.
	SignalFail Signal = "fail"
)

// Defaults of Config.
const (
	// DefaultAttempts is how often a ping is tried before it is given up.
	DefaultAttempts = 3
	// DefaultRetryDelay is the wait before the second attempt; it doubles for every
	// further one.
	DefaultRetryDelay = 2 * time.Second
	// DefaultTimeout bounds one attempt.
	DefaultTimeout = 10 * time.Second
	// DefaultCheckEvery is how often the global heartbeat checks whether a ping is
	// due (the interval itself is a setting, at least a minute).
	DefaultCheckEvery = 15 * time.Second
	// DefaultDrainTimeout is how long Run waits for pings still in flight after its
	// context ended (the /fail pings of runs cancelled by a shutdown).
	DefaultDrainTimeout = 15 * time.Second
)

// userAgent identifies the pings.
const userAgent = "MongoRescue-Heartbeat/1"

// Config configures a Service.
type Config struct {
	// Client sends the pings; nil means notify.NewHTTPClient (no redirects, blocked
	// destinations refused after DNS resolution).
	Client *http.Client
	// Global returns the live global heartbeat URL ("" = off) and interval; nil means
	// no global heartbeat.
	Global func() (rawURL string, interval time.Duration)
	// Healthy reports whether the scheduler is healthy; the global heartbeat is only
	// sent while it is. nil means always healthy.
	Healthy func() bool
	// Logger receives failures (host only, never the URL); nil means slog.Default.
	Logger *slog.Logger
	// Attempts, RetryDelay, Timeout, CheckEvery and DrainTimeout override the
	// defaults above when positive.
	Attempts     int
	RetryDelay   time.Duration
	Timeout      time.Duration
	CheckEvery   time.Duration
	DrainTimeout time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Service sends the global heartbeat and the job pings. Job pings are accepted
// while Run runs; create it with New and run Run on its own goroutine.
type Service struct {
	cfg    Config
	client *http.Client
	logger *slog.Logger

	// pingCtx bounds every job ping; it is cancelled when Run gives up draining.
	pingCtx     context.Context
	cancelPings context.CancelFunc
	// wg tracks the job pings in flight.
	wg sync.WaitGroup

	// mu guards accepting, ran and queue.
	mu        sync.Mutex
	accepting bool
	ran       bool
	// queue maps a job ID to the completion of its last queued ping, so the pings of
	// one job are sent in order (a /start never overtakes the outcome).
	queue map[string]chan struct{}
}

// New returns a Service for cfg.
func New(cfg Config) *Service {
	if cfg.Attempts <= 0 {
		cfg.Attempts = DefaultAttempts
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = DefaultRetryDelay
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = DefaultCheckEvery
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Service{cfg: cfg, client: cfg.Client, logger: cfg.Logger, queue: map[string]chan struct{}{}}
	if s.client == nil {
		s.client = notify.NewHTTPClient()
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	s.pingCtx, s.cancelPings = context.WithCancel(context.Background())
	return s
}

// Run sends the global heartbeat until ctx ends and accepts job pings meanwhile.
// It then waits up to the drain timeout for the job pings in flight and returns.
// Run runs at most once; later calls return at once.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	if s.ran {
		s.mu.Unlock()
		return
	}
	s.ran, s.accepting = true, true
	s.mu.Unlock()

	s.loop(ctx)

	s.mu.Lock()
	s.accepting = false
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(s.cfg.DrainTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		s.logger.Warn("heartbeat pings still in flight at shutdown were abandoned")
		s.cancelPings()
		<-done
	}
	s.cancelPings()
}

// loop sends the global heartbeat: at start and whenever the interval has passed
// since the last ping or the URL changed, while the scheduler is healthy.
func (s *Service) loop(ctx context.Context) {
	if s.cfg.Global == nil {
		<-ctx.Done()
		return
	}
	var lastURL string
	var lastPing time.Time
	unhealthy := false
	check := func() {
		raw, interval := s.cfg.Global()
		if raw == "" {
			lastURL = ""
			return
		}
		if s.cfg.Healthy != nil && !s.cfg.Healthy() {
			if !unhealthy {
				unhealthy = true
				s.logger.Warn("heartbeat not sent: the scheduler is not healthy", logsafe.Attr("host", hostOf(raw)))
			}
			return
		}
		if unhealthy {
			unhealthy = false
			s.logger.Info("scheduler healthy again; heartbeat resumed", logsafe.Attr("host", hostOf(raw)))
		}
		now := s.cfg.Now()
		if raw == lastURL && now.Sub(lastPing) < interval {
			return
		}
		lastURL, lastPing = raw, now
		if err := s.send(ctx, raw, SignalSuccess); err != nil && ctx.Err() == nil {
			s.logger.Warn("global heartbeat ping failed", logsafe.Attr("host", hostOf(raw)), logsafe.Error(err))
		}
	}
	check()
	ticker := time.NewTicker(s.cfg.CheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

// JobRunStarted pings "<url>/start" of the job's heartbeat (see
// scheduler.RunObserver). It returns at once.
func (s *Service) JobRunStarted(job *models.Job, _ *models.JobRun) {
	if job == nil || job.HeartbeatURL == "" {
		return
	}
	s.dispatch(job.ID, job.HeartbeatURL, SignalStart)
}

// JobRunFinished pings the job's heartbeat with the run's outcome: "<url>" for ok,
// "<url>/fail" for failed, partial and cancelled runs (see scheduler.RunObserver).
// It returns at once.
func (s *Service) JobRunFinished(job *models.Job, run *models.JobRun) {
	if job == nil || run == nil || job.HeartbeatURL == "" {
		return
	}
	s.dispatch(job.ID, job.HeartbeatURL, SignalOf(run.Status))
}

// SignalOf returns the ping signal of a finished run's status: SignalSuccess for
// ok, SignalFail for anything else.
func SignalOf(status models.JobRunStatus) Signal {
	if status == models.JobRunOK {
		return SignalSuccess
	}
	return SignalFail
}

// dispatch sends a ping of job jobID in the background, after the job's previous
// ping. Pings outside Run are dropped.
func (s *Service) dispatch(jobID, raw string, sig Signal) {
	s.mu.Lock()
	if !s.accepting {
		s.mu.Unlock()
		s.logger.Debug("heartbeat ping dropped: the heartbeat service is not running", logsafe.Attr("job_id", jobID))
		return
	}
	prev := s.queue[jobID]
	done := make(chan struct{})
	s.queue[jobID] = done
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			if s.queue[jobID] == done {
				delete(s.queue, jobID)
			}
			s.mu.Unlock()
			close(done)
		}()
		if prev != nil {
			select {
			case <-prev:
			case <-s.pingCtx.Done():
				return
			}
		}
		if err := s.send(s.pingCtx, raw, sig); err != nil && s.pingCtx.Err() == nil {
			s.logger.Warn("job heartbeat ping failed", logsafe.Attr("job_id", jobID),
				slog.String("signal", signalName(sig)), logsafe.Attr("host", hostOf(raw)), logsafe.Error(err))
		}
	}()
}

// Test sends one success ping to rawURL (a single attempt) and returns nil on a 2xx
// answer, else an error wrapping ErrPingFailed that names the host only.
func (s *Service) Test(ctx context.Context, rawURL string) error {
	if err := models.ValidateHeartbeatURL(rawURL); err != nil || rawURL == "" {
		return fmt.Errorf("%w: %w", ErrPingFailed, models.ErrInvalidHeartbeatURL)
	}
	if err := s.attempt(ctx, rawURL, SignalSuccess); err != nil {
		return fmt.Errorf("%w: %w", ErrPingFailed, err)
	}
	return nil
}

// send pings raw with sig, retrying retryable failures with a doubling delay.
func (s *Service) send(ctx context.Context, raw string, sig Signal) error {
	delay := s.cfg.RetryDelay
	var err error
	for attempt := 1; attempt <= s.cfg.Attempts; attempt++ {
		if err = s.attempt(ctx, raw, sig); err == nil {
			s.logger.Debug("heartbeat ping sent", slog.String("signal", signalName(sig)), logsafe.Attr("host", hostOf(raw)))
			return nil
		}
		if errors.Is(err, errPermanent) || attempt == s.cfg.Attempts {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
	return err
}

// attempt sends one ping. Its error never contains the URL, only the host.
func (s *Service) attempt(ctx context.Context, raw string, sig Signal) error {
	target, err := PingURL(raw, sig)
	if err != nil {
		return fmt.Errorf("%w: %w", errPermanent, err)
	}
	host := hostOf(raw)
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("%w: %s: invalid request", errPermanent, host)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		cause := err
		var ue *url.Error
		if errors.As(err, &ue) {
			cause = ue.Err
		}
		if errors.Is(cause, notify.ErrBlockedDestination) {
			return fmt.Errorf("%w: %s: %w", errPermanent, host, cause)
		}
		return fmt.Errorf("%s: %w", host, cause)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		return nil
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests, code >= 500:
		return fmt.Errorf("%s: http status %d", host, code)
	default:
		return fmt.Errorf("%w: %s: http status %d", errPermanent, host, code)
	}
}

// PingURL returns the URL a ping with sig requests: raw itself for SignalSuccess,
// else raw with "/<sig>" appended to its path (the query is kept).
func PingURL(raw string, sig Signal) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", models.ErrInvalidHeartbeatURL
	}
	if sig == SignalSuccess {
		return raw, nil
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + string(sig)
	u.RawPath = ""
	return u.String(), nil
}

// hostOf returns the host of raw (without port), the only part of a heartbeat URL
// that is logged.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "invalid-url"
	}
	return u.Hostname()
}

// signalName names sig in logs.
func signalName(sig Signal) string {
	if sig == SignalSuccess {
		return "success"
	}
	return string(sig)
}
