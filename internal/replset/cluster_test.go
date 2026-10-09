//go:build replset3

// Package replset holds the point-in-time recovery tests that need a
// three-member replica set: the failure scenarios (build tag replset3: failovers
// during collection and during a restore, a rollback through network isolation,
// divergence after a forced reconfiguration) and the soak test (build tags
// replset3 and soak).
// scripts/test-replset-docker.sh starts the replica set (scripts/replset) and
// runs them in a container on its network; they inject failures through the
// docker CLI. Every test skips unless the environment names the replica set:
//
//	MONGORESCUE_RS_URI       replica set URI with credentials (replicaSet=rs0)
//	MONGORESCUE_RS_MEMBERS   host=container pairs, comma-separated (m1:27017=<id>,…)
//	MONGORESCUE_RS_NETWORK   the docker network of the members
//	MONGORESCUE_RS_PASSWORD  the root password (mongosh in the members, redaction checks)
package replset

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// opTimeout bounds one administrative operation on the replica set.
const opTimeout = 2 * time.Minute

// cluster is the three-member replica set under test.
type cluster struct {
	uri, password, network string
	// hosts are the member host names (m1:27017, …) and containers their
	// containers.
	hosts      []string
	containers map[string]string
	// client follows the replica set topology.
	client *mongo.Client
}

// requireCluster returns the replica set the environment names, or skips.
func requireCluster(t *testing.T) *cluster {
	t.Helper()
	uri := os.Getenv("MONGORESCUE_RS_URI")
	members := os.Getenv("MONGORESCUE_RS_MEMBERS")
	if uri == "" || members == "" {
		t.Skip("MONGORESCUE_RS_URI and MONGORESCUE_RS_MEMBERS are not set (run scripts/test-replset-docker.sh)")
	}
	c := &cluster{uri: uri, password: os.Getenv("MONGORESCUE_RS_PASSWORD"), network: os.Getenv("MONGORESCUE_RS_NETWORK"),
		containers: map[string]string{}}
	for _, pair := range strings.Split(members, ",") {
		host, id, ok := strings.Cut(pair, "=")
		if !ok || host == "" || id == "" {
			t.Fatalf("malformed MONGORESCUE_RS_MEMBERS entry %q", pair)
		}
		c.hosts = append(c.hosts, host)
		c.containers[host] = id
	}
	if len(c.hosts) != 3 || c.network == "" || c.password == "" {
		t.Fatalf("need three members, a network and a password (%d members)", len(c.hosts))
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetRetryWrites(true).SetTimeout(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Disconnect(ctx)
	})
	c.client = client
	c.primary(t)
	return c
}

// docker runs the docker CLI and returns its combined output.
func (c *cluster) docker(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	text := strings.ReplaceAll(string(out), c.password, "******")
	if err != nil {
		return text, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(text))
	}
	return text, nil
}

// mustDocker runs the docker CLI and fails the test on an error.
func (c *cluster) mustDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := c.docker(context.Background(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// shell runs script through mongosh inside member host, as root, and returns its
// output. It works while the member is cut off the network.
func (c *cluster) shell(t *testing.T, host, script string) string {
	t.Helper()
	out, err := c.docker(context.Background(), "exec", c.containers[host], "mongosh", "--quiet",
		"-u", "root", "-p", c.password, "--authenticationDatabase", "admin", "--eval", script)
	if err != nil {
		t.Fatalf("mongosh on %s: %v", host, err)
	}
	return strings.TrimSpace(out)
}

// service returns the compose service (and network alias) of host.
func service(host string) string {
	name, _, _ := strings.Cut(host, ":")
	return name
}

// isolate cuts member host off the network: neither the other members nor the
// test reach it, and it reaches nobody.
func (c *cluster) isolate(t *testing.T, host string) {
	t.Helper()
	c.mustDocker(t, "network", "disconnect", c.network, c.containers[host])
	t.Logf("isolated %s", host)
}

// reconnect puts member host back on the network under its name.
func (c *cluster) reconnect(t *testing.T, host string) {
	t.Helper()
	c.mustDocker(t, "network", "connect", "--alias", service(host), c.network, c.containers[host])
	t.Logf("reconnected %s", host)
}

// kill stops member host at once (SIGKILL).
func (c *cluster) kill(t *testing.T, host string) {
	t.Helper()
	c.mustDocker(t, "kill", c.containers[host])
	t.Logf("killed %s", host)
}

// start starts the killed member host again.
func (c *cluster) start(t *testing.T, host string) {
	t.Helper()
	c.mustDocker(t, "start", c.containers[host])
	t.Logf("started %s", host)
}

// hello runs hello on the primary the driver selects.
func (c *cluster) hello(ctx context.Context) (me string, primary bool, err error) {
	var res struct {
		Me                string `bson:"me"`
		IsWritablePrimary bool   `bson:"isWritablePrimary"`
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = c.client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}},
		options.RunCmd().SetReadPreference(readpref.Primary())).Decode(&res)
	return res.Me, res.IsWritablePrimary, err
}

// primary waits for a writable primary and returns its host.
func (c *cluster) primary(t *testing.T) string {
	t.Helper()
	return c.newPrimary(t, "")
}

// newPrimary waits for a writable primary other than old and returns its host.
func (c *cluster) newPrimary(t *testing.T, old string) string {
	t.Helper()
	deadline := time.Now().Add(opTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		me, ok, err := c.hello(context.Background())
		if err == nil && ok && me != old && slices.Contains(c.hosts, me) {
			return me
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("no primary other than %q within %s (last error: %v)", old, opTimeout, lastErr)
	return ""
}

// stepDown asks the primary to step down and waits for another one.
func (c *cluster) stepDown(t *testing.T) (old, current string) {
	t.Helper()
	old = c.primary(t)
	// The shell's connection may close while the member steps down.
	_, _ = c.docker(context.Background(), "exec", c.containers[old], "mongosh", "--quiet",
		"-u", "root", "-p", c.password, "--authenticationDatabase", "admin",
		"--eval", "try { rs.stepDown(30, 5) } catch (e) { print(e.message) }")
	current = c.newPrimary(t, old)
	t.Logf("stepped down %s, %s is primary", old, current)
	return old, current
}

// waitHealthy waits until every member is primary or secondary and the
// secondaries have caught up with the primary.
func (c *cluster) waitHealthy(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		var status struct {
			Members []struct {
				Name     string `bson:"name"`
				State    int    `bson:"state"`
				StateStr string `bson:"stateStr"`
				Optime   struct {
					TS bson.Timestamp `bson:"ts"`
				} `bson:"optime"`
			} `bson:"members"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&status)
		cancel()
		if err == nil {
			var primaryTS bson.Timestamp
			ok := len(status.Members) == len(c.hosts)
			parts := make([]string, 0, len(status.Members))
			for _, m := range status.Members {
				parts = append(parts, m.Name+"="+m.StateStr)
				if m.State == 1 {
					primaryTS = m.Optime.TS
				}
				if m.State != 1 && m.State != 2 {
					ok = false
				}
			}
			// Caught up: within two seconds of the primary (writers may still run).
			for _, m := range status.Members {
				if m.Optime.TS.T+2 < primaryTS.T {
					ok = false
				}
			}
			if ok && primaryTS.T != 0 {
				return
			}
			last = strings.Join(parts, " ")
		} else {
			last = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the replica set did not become healthy: %s", last)
}

// direct returns a client of member host alone.
func (c *cluster) direct(t *testing.T, host string) *mongo.Client {
	t.Helper()
	uri := strings.Replace(c.uri, strings.Join(c.hosts, ","), host, 1)
	if uri == c.uri {
		t.Fatalf("the URI does not list the members as %s", strings.Join(c.hosts, ","))
	}
	uri = strings.Replace(uri, "replicaSet=rs0", "directConnection=true", 1)
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetTimeout(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Disconnect(ctx)
	})
	return client
}

// oplogEntry is the part of an oplog entry the tests compare.
type oplogEntry struct {
	TS   bson.Timestamp `bson:"ts"`
	Term int64          `bson:"t"`
	Op   string         `bson:"op"`
	NS   string         `bson:"ns"`
	O    bson.Raw       `bson:"o"`
}

// key identifies an entry in the history.
func (e oplogEntry) key() string { return fmt.Sprintf("%d.%d/%d", e.TS.T, e.TS.I, e.Term) }

// markers returns the integer field name of every document the entry inserts
// into a namespace accepted by ns: a single insert, or the inserts of an
// applyOps entry (MongoDB batches the inserts of one insertMany into one).
func (e oplogEntry) markers(name string, ns func(string) bool) []int64 {
	field := func(o bson.Raw) (int64, bool) {
		v, err := o.LookupErr(name)
		if err != nil {
			return 0, false
		}
		return v.AsInt64OK()
	}
	var out []int64
	switch {
	case e.Op == "i" && e.O != nil && ns(e.NS):
		if n, ok := field(e.O); ok {
			out = append(out, n)
		}
	case e.Op == "c" && e.O != nil:
		ops, err := e.O.LookupErr("applyOps")
		if err != nil {
			return nil
		}
		arr, ok := ops.ArrayOK()
		if !ok {
			return nil
		}
		values, _ := arr.Values()
		for _, v := range values {
			doc, isDoc := v.DocumentOK()
			if !isDoc {
				continue
			}
			op, _ := doc.Lookup("op").StringValueOK()
			opNS, _ := doc.Lookup("ns").StringValueOK()
			o, hasO := doc.Lookup("o").DocumentOK()
			if op != "i" || !hasO || !ns(opNS) {
				continue
			}
			if n, ok := field(o); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

// anyNS accepts every namespace.
func anyNS(string) bool { return true }

// readOplog reads the oplog of client's member in (from, to], in order.
func readOplog(t *testing.T, client *mongo.Client, from, to bson.Timestamp) []oplogEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	filter := bson.D{{Key: "ts", Value: bson.D{{Key: "$gt", Value: from}, {Key: "$lte", Value: to}}}}
	cur, err := client.Database("local").Collection("oplog.rs").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "$natural", Value: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	var out []oplogEntry
	if err = cur.All(ctx, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
