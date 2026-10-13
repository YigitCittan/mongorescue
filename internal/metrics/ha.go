package metrics

import (
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// HAState is the state of active-passive high availability reported by the HA
// series. The counters cover the life of the process, across role changes.
type HAState struct {
	// Cluster is the cluster name (the cluster label of every HA series).
	Cluster string
	// Role is "active" or "standby".
	Role string
	// Epoch is the lease epoch held (active) or last seen (standby).
	Epoch uint64
	// LeaseHealthy reports that the last read or write of the lease succeeded.
	LeaseHealthy bool
	// LeaseErrors counts failed reads and writes of the lease.
	LeaseErrors uint64
	// Takeovers counts the leases this instance acquired.
	Takeovers uint64
	// LastTakeoverSeconds is how long the last takeover took, from acquiring the
	// lease to serving as active (0 before the first).
	LastTakeoverSeconds float64
	// TakeoverFailures counts the takeovers this instance gave up (no usable
	// metadata snapshot).
	TakeoverFailures uint64
	// StepDowns counts the times this instance lost the lease while active.
	StepDowns uint64
	// SnapshotsRejected counts the metadata snapshots refused because their
	// authenticated header did not match their name or cluster, or they did not
	// decrypt.
	SnapshotsRejected uint64
	// RefusedSnapshots is how many of the snapshots on the HA target were found
	// refused at the last check: they are kept until someone deletes them.
	RefusedSnapshots int
	// RollbackSuspected counts the takeovers refused because they could roll the
	// metadata back, by reason (every reason present, 0 included).
	RollbackSuspected map[string]uint64
	// EmptyStarts counts the times this instance became active from empty state
	// (no lease and no snapshot on the HA target).
	EmptyStarts uint64
}

// haRoles are the values of the role label.
var haRoles = []string{"active", "standby"}

// haCollector exports the HA series while a source is set (HA mode only).
type haCollector struct {
	source   atomic.Pointer[func() HAState]
	role     *prometheus.Desc
	epoch    *prometheus.Desc
	healthy  *prometheus.Desc
	errors   *prometheus.Desc
	takeover *prometheus.Desc
	duration *prometheus.Desc
	failures *prometheus.Desc
	down     *prometheus.Desc
	rejected *prometheus.Desc
	refused  *prometheus.Desc
	rollback *prometheus.Desc
	empty    *prometheus.Desc
}

// newHACollector returns the collector of mongorescue_ha_role,
// mongorescue_ha_epoch, mongorescue_ha_lease_healthy,
// mongorescue_ha_lease_errors_total, mongorescue_ha_takeovers_total and
// mongorescue_ha_takeover_duration_seconds, mongorescue_ha_takeover_failures_total,
// mongorescue_ha_step_downs_total, mongorescue_ha_snapshots_rejected_total,
// mongorescue_ha_refused_snapshots, mongorescue_ha_rollback_suspected_total and
// mongorescue_ha_empty_starts_total.
func newHACollector() *haCollector {
	name := func(n string) string { return prometheus.BuildFQName(namespace, "ha", n) }
	cluster := []string{"cluster"}
	return &haCollector{
		role:     prometheus.NewDesc(name("role"), "Role of this instance in its active-passive HA cluster: 1 for the current role (active or standby), 0 for the other.", []string{"cluster", "role"}, nil),
		epoch:    prometheus.NewDesc(name("epoch"), "Epoch (fencing token) of the HA lease this instance holds, or the highest it saw as standby.", cluster, nil),
		healthy:  prometheus.NewDesc(name("lease_healthy"), "1 when the last read or write of the HA lease succeeded, 0 otherwise.", cluster, nil),
		errors:   prometheus.NewDesc(name("lease_errors_total"), "Total number of failed reads and writes (renewals included) of the HA lease.", cluster, nil),
		takeover: prometheus.NewDesc(name("takeovers_total"), "Total number of times this instance acquired the HA lease and became active.", cluster, nil),
		duration: prometheus.NewDesc(name("takeover_duration_seconds"), "Duration of the last takeover, from acquiring the lease to serving as active.", cluster, nil),
		down:     prometheus.NewDesc(name("step_downs_total"), "Total number of times this instance lost the HA lease while active and stepped down to standby.", cluster, nil),
		failures: prometheus.NewDesc(name("takeover_failures_total"), "Total number of takeovers this instance gave up because no usable metadata snapshot was found; it stays standby.", cluster, nil),
		rollback: prometheus.NewDesc(name("rollback_suspected_total"), "Total number of takeovers this instance refused because they could roll the cluster's metadata back (lease not authentic or missing, newest snapshot older than the lease or this instance knows), by reason; an operator forces them with mongorescue ha force-takeover.", []string{"cluster", "reason"}, nil),
		empty:    prometheus.NewDesc(name("empty_starts_total"), "Total number of times this instance became active from empty state: no lease and no metadata snapshot on the HA target, so it kept its own database. Expected once for a new cluster; otherwise check the HA target.", cluster, nil),
		refused:  prometheus.NewDesc(name("refused_snapshots"), "Number of metadata snapshots on the HA target that this instance found refused at its last check (copied, renamed or altered); they are kept until someone deletes them.", cluster, nil),
		rejected: prometheus.NewDesc(name("snapshots_rejected_total"), "Total number of metadata snapshots this instance refused because their authenticated header did not match their name or cluster, or they did not decrypt (copied, renamed or altered on the HA target).", cluster, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *haCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.role, c.epoch, c.healthy, c.errors, c.takeover, c.duration, c.failures, c.down, c.rejected, c.refused, c.rollback, c.empty} {
		ch <- d
	}
}

// Collect implements prometheus.Collector; it yields nothing outside HA mode.
func (c *haCollector) Collect(ch chan<- prometheus.Metric) {
	fn := c.source.Load()
	if fn == nil {
		return
	}
	st := (*fn)()
	for _, r := range haRoles {
		ch <- prometheus.MustNewConstMetric(c.role, prometheus.GaugeValue, boolFloat(st.Role == r), st.Cluster, r)
	}
	ch <- prometheus.MustNewConstMetric(c.epoch, prometheus.GaugeValue, float64(st.Epoch), st.Cluster)
	ch <- prometheus.MustNewConstMetric(c.healthy, prometheus.GaugeValue, boolFloat(st.LeaseHealthy), st.Cluster)
	ch <- prometheus.MustNewConstMetric(c.errors, prometheus.CounterValue, float64(st.LeaseErrors), st.Cluster)
	ch <- prometheus.MustNewConstMetric(c.takeover, prometheus.CounterValue, float64(st.Takeovers), st.Cluster)
	ch <- prometheus.MustNewConstMetric(c.duration, prometheus.GaugeValue, st.LastTakeoverSeconds, st.Cluster)
	ch <- prometheus.MustNewConstMetric(c.failures, prometheus.CounterValue, float64(st.TakeoverFailures), st.Cluster)
	ch <- prometheus.MustNewConstMetric(c.down, prometheus.CounterValue, float64(st.StepDowns), st.Cluster)
	ch <- prometheus.MustNewConstMetric(c.rejected, prometheus.CounterValue, float64(st.SnapshotsRejected), st.Cluster)
	ch <- prometheus.MustNewConstMetric(c.refused, prometheus.GaugeValue, float64(st.RefusedSnapshots), st.Cluster)
	ch <- prometheus.MustNewConstMetric(c.empty, prometheus.CounterValue, float64(st.EmptyStarts), st.Cluster)
	for reason, n := range st.RollbackSuspected {
		ch <- prometheus.MustNewConstMetric(c.rollback, prometheus.CounterValue, float64(n), st.Cluster, reason)
	}
}

// boolFloat returns 1 for true and 0 for false.
func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// standbyFamilies are the prefixes of the metric families a standby exposes
// without credentials: process, runtime, build and HA series, never data.
var standbyFamilies = []string{"mongorescue_ha_", "mongorescue_build_info", "go_", "process_"}

// StandbyHandler serves the metrics a standby of an active-passive HA cluster
// exposes without credentials (it trusts no API key of its database): the
// process, Go runtime, build and HA series only.
func (m *Metrics) StandbyHandler() http.Handler {
	gather := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		families, err := m.registry.Gather()
		out := families[:0]
		for _, f := range families {
			for _, p := range standbyFamilies {
				if strings.HasPrefix(f.GetName(), p) {
					out = append(out, f)
					break
				}
			}
		}
		return out, err
	})
	return promhttp.HandlerFor(gather, promhttp.HandlerOpts{})
}

// SetHASource registers the function reporting the HA state; the HA series are
// exported only while one is set. It is safe to call at any time.
func (m *Metrics) SetHASource(fn func() HAState) {
	if fn == nil {
		m.ha.source.Store(nil)
		return
	}
	m.ha.source.Store(&fn)
}
