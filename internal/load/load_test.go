//go:build load

package load

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// config is the scale of a run (MONGORESCUE_LOAD_* variables).
type config struct {
	Profile     string `json:"profile"`
	DataMB      int    `json:"data_mb"`
	Jobs        int    `json:"jobs"`
	Connections int    `json:"connections"`
	Concurrent  int    `json:"concurrent_backups"`
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func loadConfig() config {
	c := config{
		Profile:     os.Getenv("MONGORESCUE_LOAD_PROFILE"),
		DataMB:      envInt("MONGORESCUE_LOAD_DATA_MB", 5120),
		Jobs:        envInt("MONGORESCUE_LOAD_JOBS", 10000),
		Connections: envInt("MONGORESCUE_LOAD_CONNECTIONS", 500),
		Concurrent:  envInt("MONGORESCUE_LOAD_CONCURRENT", 20),
	}
	if c.Profile == "" {
		c.Profile = "ci"
	}
	return c
}

// phase is the latency of the primary probe during one phase.
type phase struct {
	latencies
	OpsPerSecond float64 `json:"ops_per_second"`
	// Impact is the p95 relative to the idle phase.
	Impact float64 `json:"p95_vs_idle"`
}

// report is the JSON output of a run.
type report struct {
	Config      config               `json:"config"`
	Environment map[string]string    `json:"environment"`
	StartedAt   time.Time            `json:"started_at"`
	Seconds     float64              `json:"seconds"`
	Setup       map[string]latencies `json:"setup"`
	API         map[string]latencies `json:"api"`
	Primary     map[string]phase     `json:"primary_impact"`
	Metrics     map[string]float64   `json:"metrics"`
	Regressions []string             `json:"regressions"`
	Notes       map[string]string    `json:"notes,omitempty"`
	mu          sync.Mutex
}

func (r *report) metric(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Metrics[name] = v
}

// sampler records the process memory every second while running.
type sampler struct {
	stop chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	rss  float64
	heap float64
}

func startSampler(p *proc, c *api) *sampler {
	s := &sampler{stop: make(chan struct{})}
	s.wg.Go(func() {
		for {
			rss := p.rssMiB()
			heap := heapInuseMiB(c)
			s.mu.Lock()
			s.rss, s.heap = max(s.rss, rss), max(s.heap, heap)
			s.mu.Unlock()
			select {
			case <-s.stop:
				return
			case <-time.After(time.Second):
			}
		}
	})
	return s
}

func (s *sampler) halt() (rss, heap float64) {
	close(s.stop)
	s.wg.Wait()
	return s.rss, s.heap
}

// heapInuseMiB reads go_memstats_heap_inuse_bytes from /metrics.
func heapInuseMiB(c *api) float64 {
	req, _ := http.NewRequest("GET", c.base+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "go_memstats_heap_inuse_bytes "); ok {
			f, _ := strconv.ParseFloat(v, 64)
			return f / (1 << 20)
		}
	}
	return 0
}

// generate fills db.docs with about mb MiB of incompressible 256 KiB documents
// using parallel writers, unless it already holds that much (reruns reuse it).
func generate(t *testing.T, client *mongo.Client, db string, mb int) {
	t.Helper()
	ctx := context.Background()
	coll := client.Database(db).Collection("docs")
	var stats struct {
		DataSize float64 `bson:"dataSize"`
	}
	_ = client.Database(db).RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&stats)
	if stats.DataSize >= float64(mb)*(1<<20)*0.95 {
		t.Logf("%s already holds %.0f MiB", db, stats.DataSize/(1<<20))
		return
	}
	_ = client.Database(db).Drop(ctx)
	const docSize = 256 << 10
	total := mb * 4
	var next atomic.Int64
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			pool := make([]byte, 4<<20)
			_, _ = rand.Read(pool)
			for {
				first := next.Add(32) - 32
				if first >= int64(total) {
					return
				}
				docs := make([]any, 0, 32)
				for i := first; i < first+32 && i < int64(total); i++ {
					off := (i * 7919) % int64(len(pool)-docSize)
					blob := make([]byte, docSize)
					copy(blob, pool[off:off+docSize])
					copy(blob, strconv.FormatInt(i, 10))
					docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "blob", Value: bson.Binary{Data: blob}}})
				}
				if _, err := coll.InsertMany(ctx, docs); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		t.Fatalf("generate %s: %v", db, err)
	}
	t.Logf("generated %d MiB in %s", mb, time.Since(start).Round(time.Second))
}

// cronFarFromNow returns a daily schedule hours away from now, so the jobs never
// fire during a run; i spreads them over minutes and six hours.
func cronFarFromNow(i int) string {
	h := (time.Now().UTC().Hour() + 9 + i%6) % 24
	return fmt.Sprintf("%d %d * * *", i%60, h)
}

// probe measures the latency of an insert and a read by _id on the primary in a
// loop until stopped.
type probe struct {
	stop chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	ds   []time.Duration
}

func startProbe(coll *mongo.Collection) *probe {
	p := &probe{stop: make(chan struct{})}
	p.wg.Go(func() {
		var i int64
		for {
			select {
			case <-p.stop:
				return
			default:
			}
			i++
			id := fmt.Sprintf("%d-%d", time.Now().UnixNano(), i)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			start := time.Now()
			_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "v", Value: i}})
			if err == nil {
				err = coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Err()
			}
			d := time.Since(start)
			cancel()
			if err == nil {
				p.mu.Lock()
				p.ds = append(p.ds, d)
				p.mu.Unlock()
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	return p
}

func (p *probe) halt(window time.Duration) phase {
	close(p.stop)
	p.wg.Wait()
	return phase{latencies: summarize(p.ds), OpsPerSecond: float64(len(p.ds)) / window.Seconds()}
}

// waitBackup polls a backup until it leaves in_progress.
func waitBackup(t *testing.T, c *api, db, id string, timeout time.Duration) *models.BackupRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var list []*models.BackupRecord
		c.data("GET", "/api/v1/backups?database="+db+"&id="+id, nil, http.StatusOK, &list)
		if len(list) == 1 && list[0].Status != models.StatusInProgress && list[0].Status != models.StatusPending {
			return list[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup %s did not finish in %s", id, timeout)
		}
		time.Sleep(time.Second)
	}
}

// TestLoad runs the load scenario and compares it with the baseline.
func TestLoad(t *testing.T) {
	uri, endpoint := os.Getenv("MONGORESCUE_CHAOS_MONGO_URI"), os.Getenv("MONGORESCUE_CHAOS_S3_ENDPOINT")
	if uri == "" || endpoint == "" {
		t.Skip("the load services are not configured; run SUITE=load scripts/test-chaos-docker.sh")
	}
	cfg := loadConfig()
	rep := &report{Config: cfg, StartedAt: time.Now().UTC(), Setup: map[string]latencies{}, API: map[string]latencies{},
		Primary: map[string]phase{}, Metrics: map[string]float64{}, Notes: map[string]string{},
		Environment: map[string]string{"goos": runtime.GOOS, "goarch": runtime.GOARCH, "cpus": strconv.Itoa(runtime.NumCPU())}}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		rep.Environment["runner"] = "github-actions"
	}

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	// Data: the large database and the small ones of the concurrent backups.
	generate(t, client, "load_big", cfg.DataMB)
	for i := range cfg.Concurrent {
		generate(t, client, fmt.Sprintf("load_small_%02d", i), 16)
	}

	bucket := os.Getenv("MONGORESCUE_CHAOS_S3_BUCKET")
	s3c := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint), Region: "us-east-1", UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(os.Getenv("MONGORESCUE_CHAOS_S3_ACCESS_KEY"), os.Getenv("MONGORESCUE_CHAOS_S3_SECRET_KEY"), ""),
	})
	if _, err := s3c.HeadBucket(context.Background(), &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if _, err := s3c.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatalf("create bucket %s: %v", bucket, err)
		}
	}

	p := startServer(t)
	c := setup(t, p)
	mem := startSampler(p, c)
	var target models.StorageTarget
	c.data("POST", "/api/v1/storage-targets", map[string]any{"name": "minio", "type": "s3", "s3": map[string]any{
		"endpoint": endpoint, "region": "us-east-1", "bucket": bucket, "prefix": fmt.Sprintf("load/%d/", time.Now().Unix()),
		"access_key_id": os.Getenv("MONGORESCUE_CHAOS_S3_ACCESS_KEY"), "secret_access_key": os.Getenv("MONGORESCUE_CHAOS_S3_SECRET_KEY"), "use_path_style": true,
	}}, http.StatusCreated, &target)
	c.data("POST", "/api/v1/storage-targets/"+target.ID+"/default", nil, http.StatusOK, nil)

	// Scale: connections and scheduled jobs, created by 16 clients at once.
	parallel := func(n int, fn func(i int) time.Duration) (latencies, float64) {
		var mu sync.Mutex
		ds := make([]time.Duration, 0, n)
		var next atomic.Int64
		start := time.Now()
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				for {
					i := int(next.Add(1) - 1)
					if i >= n {
						return
					}
					d := fn(i)
					mu.Lock()
					ds = append(ds, d)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		return summarize(ds), float64(n) / time.Since(start).Seconds()
	}
	conns := make([]string, cfg.Connections)
	lat, rate := parallel(cfg.Connections, func(i int) time.Duration {
		var conn models.Connection
		d := c.data("POST", "/api/v1/connections", map[string]any{"name": fmt.Sprintf("load-%04d", i), "uri": uri, "max_concurrent_backups": 4}, http.StatusCreated, &conn)
		conns[i] = conn.ID
		return d
	})
	rep.Setup["create_connection"] = lat
	rep.metric("setup.connections_per_second", rate)
	lat, rate = parallel(cfg.Jobs, func(i int) time.Duration {
		return c.data("POST", "/api/v1/jobs", map[string]any{
			"name": fmt.Sprintf("load-%05d", i), "database": fmt.Sprintf("load_small_%02d", i%cfg.Concurrent),
			"cron_expression": cronFarFromNow(i), "enabled": true, "connection_id": conns[i%len(conns)], "retention_count": 7,
		}, http.StatusCreated, nil)
	})
	rep.Setup["create_job"] = lat
	rep.metric("setup.jobs_per_second", rate)
	t.Logf("created %d connections and %d jobs (%.0f jobs/s)", cfg.Connections, cfg.Jobs, rate)

	// Idle at scale: scheduler ticks and API latencies.
	tickLag := watchTicks(t, c, 100*time.Second, func() {
		endpoints := map[string]string{
			"jobs_page": "/api/v1/jobs?limit=50", "jobs_all": "/api/v1/jobs", "backups_page": "/api/v1/backups?limit=50",
			"connections": "/api/v1/connections", "overview": "/api/v1/stats", "history": "/api/v1/stats/history",
			"readiness": "/api/v1/readiness", "health": "/api/v1/health",
		}
		for name, path := range endpoints {
			var ds []time.Duration
			for range 50 {
				code, _, d := c.do("GET", path, nil)
				if code != http.StatusOK {
					t.Fatalf("GET %s: %d", path, code)
				}
				ds = append(ds, d)
			}
			rep.API[name] = summarize(ds)
			rep.metric("api."+name+".p95_ms", rep.API[name].P95)
			lat, _ := parallel(200, func(int) time.Duration { _, _, d := c.do("GET", path, nil); return d })
			rep.API[name+"_x16"] = lat
			rep.metric("api."+name+"_x16.p95_ms", lat.P95)
		}
	})
	rep.metric("scheduler.tick_lag_max_s", tickLag)
	rss, heap := mem.halt()
	rep.metric("memory.idle_rss_peak_mib", rss)
	rep.metric("memory.idle_heap_inuse_peak_mib", heap)

	// Primary impact and throughput.
	probeColl := client.Database("load_probe").Collection("p")
	idleStart := time.Now()
	pr := startProbe(probeColl)
	time.Sleep(20 * time.Second)
	idle := pr.halt(time.Since(idleStart))
	idle.Impact = 1
	rep.Primary["idle"] = idle

	mem = startSampler(p, c)
	run := func(name string, start func() string, limit time.Duration) *models.BackupRecord {
		begin := time.Now()
		pr := startProbe(probeColl)
		id := start()
		var rec *models.BackupRecord
		deadline := time.Now().Add(limit)
		for {
			var list []*models.BackupRecord
			c.data("GET", "/api/v1/backups?database=load_big&id="+id, nil, http.StatusOK, &list)
			if len(list) == 1 && list[0].Status != models.StatusInProgress && list[0].Status != models.StatusPending {
				rec = list[0]
				break
			}
			if time.Now().After(deadline) {
				if code, body, _ := c.do("POST", "/api/v1/backups/"+id+"/cancel", nil); code/100 != 2 {
					t.Fatalf("cancel %s: %d %s", id, code, body)
				}
				rec = waitBackup(t, c, "load_big", id, 5*time.Minute)
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		ph := pr.halt(time.Since(begin))
		if idle.P95 > 0 {
			ph.Impact = ph.P95 / idle.P95
		}
		rep.Primary[name] = ph
		rep.metric("primary."+name+".p95_ms", ph.P95)
		return rec
	}
	big := run("dump_primary", func() string {
		var rec models.BackupRecord
		c.data("POST", "/api/v1/backups", map[string]any{"connection_id": conns[0], "database": "load_big", "gzip": false}, http.StatusAccepted, &rec)
		return rec.ID
	}, 2*time.Hour)
	if big.Status != models.StatusCompleted || big.DurationSeconds <= 0 {
		t.Fatalf("the large backup did not complete: %+v", big)
	}
	rep.metric("backup.throughput_mb_s", float64(big.SizeBytes)/1e6/big.DurationSeconds)
	rep.metric("backup.size_mb", float64(big.SizeBytes)/1e6)
	t.Logf("backup of %.0f MB at %.1f MB/s", float64(big.SizeBytes)/1e6, float64(big.SizeBytes)/1e6/big.DurationSeconds)

	if sec := os.Getenv("MONGORESCUE_LOAD_SECONDARY_URI"); sec != "" {
		var conn models.Connection
		c.data("POST", "/api/v1/connections", map[string]any{"name": "load-secondary", "uri": sec, "read_preference": "secondary"}, http.StatusCreated, &conn)
		run("dump_secondary", func() string {
			var rec models.BackupRecord
			c.data("POST", "/api/v1/backups", map[string]any{"connection_id": conn.ID, "database": "load_big", "gzip": false}, http.StatusAccepted, &rec)
			return rec.ID
		}, 60*time.Second)
	} else {
		rep.Notes["dump_secondary"] = "skipped: MONGORESCUE_LOAD_SECONDARY_URI is not set"
	}
	var throttled models.Job
	c.data("POST", "/api/v1/jobs", map[string]any{
		"name": "load-throttled", "database": "load_big", "cron_expression": cronFarFromNow(0), "enabled": true,
		"connection_id": conns[0], "max_upload_mbps": 80, "num_parallel_collections": 1,
	}, http.StatusCreated, &throttled)
	run("dump_throttled", func() string {
		var rec models.BackupRecord
		c.data("POST", "/api/v1/jobs/"+throttled.ID+"/run", nil, http.StatusAccepted, &rec)
		return rec.ID
	}, 60*time.Second)
	rss, heap = mem.halt()
	rep.metric("memory.backup_rss_peak_mib", rss)
	rep.metric("memory.backup_heap_inuse_peak_mib", heap)

	// Concurrent backups (four at a time per connection) and SQLite contention.
	mem = startSampler(p, c)
	stop := make(chan struct{})
	var apiDs []time.Duration
	var apiWg sync.WaitGroup
	apiWg.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			_, _, d := c.do("GET", "/api/v1/backups?limit=50", nil)
			_, _, d2 := c.do("GET", "/api/v1/stats", nil)
			apiDs = append(apiDs, d, d2)
		}
	})
	start := time.Now()
	ids := make([]string, cfg.Concurrent)
	var wg sync.WaitGroup
	for i := range cfg.Concurrent {
		wg.Go(func() {
			var rec models.BackupRecord
			c.data("POST", "/api/v1/backups", map[string]any{"connection_id": conns[0], "database": fmt.Sprintf("load_small_%02d", i), "gzip": true}, http.StatusAccepted, &rec)
			ids[i] = rec.ID
		})
	}
	wg.Wait()
	for i, id := range ids {
		if b := waitBackup(t, c, fmt.Sprintf("load_small_%02d", i), id, 30*time.Minute); b.Status != models.StatusCompleted {
			t.Fatalf("concurrent backup %d: %+v", i, b)
		}
	}
	close(stop)
	apiWg.Wait()
	rep.metric("concurrent.wall_s", time.Since(start).Seconds())
	rep.API["during_concurrent_backups"] = summarize(apiDs)
	rep.metric("concurrent.api_p95_ms", rep.API["during_concurrent_backups"].P95)
	busy := strings.Count(p.logs.String(), "database is locked") + strings.Count(p.logs.String(), "SQLITE_BUSY")
	rep.metric("concurrent.sqlite_busy_errors", float64(busy))
	rss, heap = mem.halt()
	rep.metric("memory.concurrent_rss_peak_mib", rss)
	rep.metric("memory.concurrent_heap_inuse_peak_mib", heap)

	rep.Seconds = time.Since(rep.StartedAt).Seconds()
	rep.Regressions = compare(t, rep)
	writeReport(t, rep)
	if len(rep.Regressions) > 0 {
		t.Fatalf("regressions against the %q baseline:\n  %s", cfg.Profile, strings.Join(rep.Regressions, "\n  "))
	}
}

// watchTicks runs work while sampling the scheduler's last tick every second for
// at least d, and returns the largest delay of a tick beyond its 30 s interval.
func watchTicks(t *testing.T, c *api, d time.Duration, work func()) float64 {
	t.Helper()
	stop := make(chan struct{})
	var ticks []time.Time
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			var h struct {
				LastTick time.Time `json:"scheduler_last_tick"`
			}
			_, raw, _ := c.do("GET", "/api/v1/health", nil)
			var env envelope
			if json.Unmarshal(raw, &env) == nil && json.Unmarshal(env.Data, &h) == nil && !h.LastTick.IsZero() {
				if len(ticks) == 0 || !h.LastTick.Equal(ticks[len(ticks)-1]) {
					ticks = append(ticks, h.LastTick)
				}
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
		}
	})
	begin := time.Now()
	work()
	if rest := d - time.Since(begin); rest > 0 {
		time.Sleep(rest)
	}
	close(stop)
	wg.Wait()
	if len(ticks) < 3 {
		t.Fatalf("only %d scheduler ticks seen in %s", len(ticks), time.Since(begin))
	}
	worst := 0.0
	for i := 1; i < len(ticks); i++ {
		worst = max(worst, ticks[i].Sub(ticks[i-1]).Seconds()-30)
	}
	return worst
}

// baseline is testdata/baseline.json: per profile, the scale it was recorded at
// and, per metric, the recorded value, which way is better and the factor a run
// may be worse by.
type baseline struct {
	Note     string                     `json:"note"`
	Profiles map[string]baselineProfile `json:"profiles"`
}

type baselineProfile struct {
	Recorded string                    `json:"recorded"`
	Config   config                    `json:"config"`
	Metrics  map[string]baselineMetric `json:"metrics"`
}

type baselineMetric struct {
	Value     float64 `json:"value"`
	Better    string  `json:"better"`
	Tolerance float64 `json:"tolerance"`
	// Slack is added to a lower-is-better limit, so tiny values (a few
	// milliseconds, zero errors) do not fail on noise.
	Slack float64 `json:"slack,omitempty"`
}

func baselinePath() string {
	if p := os.Getenv("MONGORESCUE_LOAD_BASELINE"); p != "" {
		return p
	}
	return filepath.Join("testdata", "baseline.json")
}

// compare checks the run against its profile's baseline; with
// MONGORESCUE_LOAD_UPDATE_BASELINE=1 it records the run as the new baseline of the
// profile (keeping the tolerances) instead.
func compare(t *testing.T, rep *report) []string {
	t.Helper()
	var b baseline
	raw, err := os.ReadFile(baselinePath())
	if err != nil {
		t.Fatalf("read the baseline: %v", err)
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("parse the baseline: %v", err)
	}
	prof, ok := b.Profiles[rep.Config.Profile]
	if os.Getenv("MONGORESCUE_LOAD_UPDATE_BASELINE") == "1" {
		if !ok {
			prof = baselineProfile{Metrics: map[string]baselineMetric{}}
		}
		prof.Recorded = fmt.Sprintf("%s on %s/%s, %s CPUs", rep.StartedAt.Format("2006-01-02"), runtime.GOOS, runtime.GOARCH, rep.Environment["cpus"])
		prof.Config = rep.Config
		for name, v := range rep.Metrics {
			m, known := prof.Metrics[name]
			if !known {
				m = defaultTolerance(name)
			}
			m.Value = v
			prof.Metrics[name] = m
		}
		if b.Profiles == nil {
			b.Profiles = map[string]baselineProfile{}
		}
		b.Profiles[rep.Config.Profile] = prof
		out, _ := json.MarshalIndent(b, "", "  ")
		if err := os.WriteFile(baselinePath(), append(out, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded the %q baseline", rep.Config.Profile)
		return nil
	}
	if !ok {
		t.Fatalf("no %q profile in %s; record one with MONGORESCUE_LOAD_UPDATE_BASELINE=1", rep.Config.Profile, baselinePath())
	}
	if prof.Config != rep.Config {
		t.Fatalf("the %q baseline was recorded at %+v, this run is %+v", rep.Config.Profile, prof.Config, rep.Config)
	}
	var out []string
	for name, m := range prof.Metrics {
		v, ok := rep.Metrics[name]
		if !ok {
			continue
		}
		switch m.Better {
		case "lower":
			if limit := m.Value*m.Tolerance + m.Slack; v > limit {
				out = append(out, fmt.Sprintf("%s = %.2f, above %.2f (baseline %.2f x %.1f + %.1f)", name, v, limit, m.Value, m.Tolerance, m.Slack))
			}
		case "higher":
			if limit := m.Value / m.Tolerance; v < limit {
				out = append(out, fmt.Sprintf("%s = %.2f, below %.2f (baseline %.2f / %.1f)", name, v, limit, m.Value, m.Tolerance))
			}
		}
	}
	return out
}

// defaultTolerance is the tolerance of a metric new to the baseline.
func defaultTolerance(name string) baselineMetric {
	switch {
	case strings.HasSuffix(name, "_per_second"), strings.HasSuffix(name, "throughput_mb_s"):
		return baselineMetric{Better: "higher", Tolerance: 3}
	case strings.HasPrefix(name, "memory."):
		return baselineMetric{Better: "lower", Tolerance: 2, Slack: 32}
	case name == "concurrent.sqlite_busy_errors":
		return baselineMetric{Better: "lower", Tolerance: 1, Slack: 0}
	case name == "scheduler.tick_lag_max_s":
		return baselineMetric{Better: "lower", Tolerance: 1, Slack: 5}
	case name == "backup.size_mb":
		return baselineMetric{Better: "lower", Tolerance: 1.2}
	default:
		return baselineMetric{Better: "lower", Tolerance: 3, Slack: 10}
	}
}

// writeReport writes the JSON report to MONGORESCUE_LOAD_REPORT (default
// load-report.json in the report directory or the package directory) and logs it.
func writeReport(t *testing.T, rep *report) {
	t.Helper()
	path := os.Getenv("MONGORESCUE_LOAD_REPORT")
	if path == "" {
		dir := os.Getenv("MONGORESCUE_CHAOS_REPORT_DIR")
		if dir == "" {
			dir = t.TempDir()
		}
		path = filepath.Join(dir, "load-report.json")
	}
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("report written to %s:\n%s", path, raw)
	if summary := os.Getenv("GITHUB_STEP_SUMMARY"); summary != "" {
		if f, err := os.OpenFile(summary, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
			fmt.Fprintf(f, "### Load test (%s)\n\n| Metric | Value |\n| --- | --- |\n", rep.Config.Profile)
			for name, v := range rep.Metrics {
				fmt.Fprintf(f, "| %s | %.2f |\n", name, v)
			}
			_ = f.Close()
		}
	}
}
