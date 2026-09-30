//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
)

const (
	// envLarge enables the large-data run (nightly): about 2 GiB of generated data
	// and a hard memory limit.
	envLarge = "MONGORESCUE_TEST_LARGE"
	// envLargeMB overrides the generated data size in MiB.
	envLargeMB = "MONGORESCUE_TEST_LARGE_MB"

	defaultSmokeMB = 64
	defaultLargeMB = 2048
	// maxPeakMemory is the memory limit of the large run: streaming must not buffer,
	// so memory must not grow with the dump size.
	maxPeakMemory = 256 << 20
	largeDocSize  = 256 << 10
)

// TestThroughputAndMemory backs up and restores generated incompressible data and
// reports throughput (MB/s) and the peak memory of the MongoRescue process during
// each phase. With MONGORESCUE_TEST_LARGE=1 it uses about 2 GiB and fails when the
// peak exceeds 256 MiB (skipped under the race detector, which inflates memory);
// otherwise it runs a 64 MiB smoke version that only reports.
func TestThroughputAndMemory(t *testing.T) {
	env := requireMongo(t)
	large := os.Getenv(envLarge) == "1"
	sizeMB := defaultSmokeMB
	if large {
		sizeMB = defaultLargeMB
	}
	if v := os.Getenv(envLargeMB); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("%s=%q is not a positive number", envLargeMB, v)
		}
		sizeMB = n
	}

	db := env.uniqueDB(t, "big")
	seedStart := time.Now()
	env.seedRandom(t, db, "bulk", int64(sizeMB)<<20)
	t.Logf("seeded %d MiB in %s", sizeMB, time.Since(seedStart).Round(time.Second))
	sourceHash := env.collectionHash(t, db, "bulk")

	enc, dec := keyPair(t)
	type run struct {
		target    storageTarget
		encrypted bool
	}
	var runs []run
	for _, tg := range storageTargets(t) {
		runs = append(runs, run{target: tg})
		if tg.Name == "local" {
			runs = append(runs, run{target: tg, encrypted: true})
		}
	}

	report := []string{
		fmt.Sprintf("### Throughput and memory (%d MiB, MongoDB %s)", sizeMB, env.versionString(t)),
		"",
		"| Target | Encrypted | Backup MB/s | Backup peak MiB | Restore MB/s | Restore peak MiB |",
		"| :--- | :--- | ---: | ---: | ---: | ---: |",
	}
	for _, r := range runs {
		t.Run(fmt.Sprintf("%s/encrypted=%v", r.target.Name, r.encrypted), func(t *testing.T) {
			var bOpts []backup.Option
			var rOpts []restore.Option
			if r.encrypted {
				bOpts, rOpts = append(bOpts, backup.WithEncryptor(enc)), append(rOpts, restore.WithDecryptor(dec))
			}

			var bkp *models.BackupRecord
			bStats := measure(func() {
				bkp = mustBackup(t, env, r.target.Storage, models.BackupOptions{Database: db}, bOpts...)
			})
			var rst *models.RestoreRecord
			rStats := measure(func() {
				rst = mustRestore(t, env, r.target.Storage, models.RestoreRequest{}, bkp, rOpts...)
			})
			if got := env.collectionHash(t, rst.TargetDatabase, "bulk"); got != sourceHash {
				t.Fatalf("restored data differs: dbHash %s, want %s", got, sourceHash)
			}
			env.dropDB(t, rst.TargetDatabase)

			mb := float64(bkp.SizeBytes) / 1e6
			line := fmt.Sprintf("| %s | %v | %.1f | %.0f | %.1f | %.0f |", r.target.Name, r.encrypted,
				mb/bStats.elapsed.Seconds(), mib(bStats.peak), mb/rStats.elapsed.Seconds(), mib(rStats.peak))
			t.Logf("%s: archive %.0f MB; backup %s (%.1f MB/s, peak %.0f MiB %s); restore %s (%.1f MB/s, peak %.0f MiB)",
				r.target.Name, mb, bStats.elapsed.Round(time.Millisecond), mb/bStats.elapsed.Seconds(), mib(bStats.peak), bStats.source,
				rStats.elapsed.Round(time.Millisecond), mb/rStats.elapsed.Seconds(), mib(rStats.peak))
			report = append(report, line)

			if large && !raceEnabled {
				for phase, s := range map[string]memStats{"backup": bStats, "restore": rStats} {
					if s.peak > maxPeakMemory {
						t.Errorf("%s peak memory %.0f MiB exceeds %d MiB: the stream is being buffered", phase, mib(s.peak), maxPeakMemory>>20)
					}
				}
			}
		})
	}
	writeStepSummary(t, report)
}

// seedRandom inserts about size bytes of random binary documents into db.coll.
func (m *mongoEnv) seedRandom(t *testing.T, db, coll string, size int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	rng := rand.NewChaCha8([32]byte{1, 2, 3})
	const batch = 32
	docs := int(size / largeDocSize)
	buffers := make([][]byte, batch)
	for i := range buffers {
		buffers[i] = make([]byte, largeDocSize)
	}
	for start := 0; start < docs; start += batch {
		items := make([]any, 0, batch)
		for i := start; i < min(start+batch, docs); i++ {
			buf := buffers[i-start]
			_, _ = rng.Read(buf)
			items = append(items, bson.D{{Key: "_id", Value: i}, {Key: "payload", Value: bson.Binary{Data: buf}}})
		}
		if _, err := m.Client.Database(db).Collection(coll).InsertMany(ctx, items); err != nil {
			t.Fatalf("seed %s.%s: %v", db, coll, err)
		}
	}
}

// collectionHash returns the server-side dbHash of db.coll (documents in _id order).
func (m *mongoEnv) collectionHash(t *testing.T, db, coll string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var res struct {
		Collections map[string]string `bson:"collections"`
	}
	if err := m.Client.Database(db).RunCommand(ctx, bson.D{{Key: "dbHash", Value: 1}, {Key: "collections", Value: bson.A{coll}}}).Decode(&res); err != nil {
		t.Fatalf("dbHash %s.%s: %v", db, coll, err)
	}
	return res.Collections[coll]
}

func (m *mongoEnv) versionString(t *testing.T) string {
	major, minor := m.serverVersion(t)
	return fmt.Sprintf("%d.%d", major, minor)
}

// memStats is the outcome of measure.
type memStats struct {
	elapsed time.Duration
	peak    uint64
	// source names what peak measures: "RSS" (Linux) or "Go runtime" elsewhere.
	source string
}

// measure runs fn and samples the process memory while it runs, after returning
// memory left over from earlier phases (seeding) to the OS.
func measure(fn func()) memStats {
	runtime.GC()
	debug.FreeOSMemory()
	var peak atomic.Uint64
	source := "Go runtime"
	if _, ok := processRSS(); ok {
		source = "RSS"
	}
	sample := func() {
		v, ok := processRSS()
		if !ok {
			v = goMemory()
		}
		if v > peak.Load() {
			peak.Store(v)
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			sample()
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	start := time.Now()
	fn()
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()
	sample()
	return memStats{elapsed: elapsed, peak: peak.Load(), source: source}
}

// processRSS returns the resident set size of this process on Linux.
func processRSS() (uint64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kb << 10, err == nil
		}
	}
	return 0, false
}

// goMemory returns the memory the Go runtime holds from the OS.
func goMemory() uint64 {
	samples := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(samples)
	return samples[0].Value.Uint64() - samples[1].Value.Uint64()
}

func mib(v uint64) float64 { return float64(v) / (1 << 20) }

// writeStepSummary appends lines to the GitHub Actions job summary, when there is one.
func writeStepSummary(t *testing.T, lines []string) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Logf("step summary: %v", err)
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintln(f, strings.Join(lines, "\n")+"\n")
}
