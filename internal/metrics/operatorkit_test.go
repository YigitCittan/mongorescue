package metrics

// The operator kit (deploy/prometheus, deploy/grafana, docs/runbooks.md) is checked
// against the code here, so it cannot drift from the metrics MongoRescue exports:
// every alert links to a runbook section that exists, and the Grafana dashboard only
// queries metrics that are registered.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	alertsFile        = "../../deploy/prometheus/alerts.yml"
	alertsTestFile    = "../../deploy/prometheus/alerts_test.yml"
	helmAlertsFile    = "../../deploy/helm/mongorescue/files/alerts.yml"
	runbooksFile      = "../../docs/runbooks.md"
	dashboardFile     = "../../deploy/grafana/mongorescue.json"
	helmDashboardFile = "../../deploy/helm/mongorescue/files/grafana-dashboard.json"
	runbookURLBase    = "https://yigitcittan.github.io/mongorescue/runbooks/#"
	runbookURLField   = "runbook_url:"
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// alertRunbookURLs maps every alert of an alert rules file to its runbook_url
// ("" when the rule has none).
func alertRunbookURLs(t *testing.T, text string) map[string]string {
	t.Helper()
	alertRe := regexp.MustCompile(`^\s*- alert: (\w+)\s*$`)
	urlRe := regexp.MustCompile(`^\s*runbook_url: "([^"]*)"\s*$`)
	out := map[string]string{}
	current := ""
	for _, line := range strings.Split(text, "\n") {
		if m := alertRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			out[current] = ""
			continue
		}
		if m := urlRe.FindStringSubmatch(line); m != nil && current != "" {
			out[current] = m[1]
		}
	}
	return out
}

func TestAlertsHaveRunbooks(t *testing.T) {
	alerts := alertRunbookURLs(t, readRepoFile(t, alertsFile))
	if len(alerts) < 30 {
		t.Fatalf("found only %d alerts in %s", len(alerts), alertsFile)
	}
	runbooks := readRepoFile(t, runbooksFile)
	for name, url := range alerts {
		want := runbookURLBase + strings.ToLower(name)
		if url != want {
			t.Errorf("alert %s: runbook_url = %q, want %q", name, url, want)
			continue
		}
		anchor := strings.TrimPrefix(url, runbookURLBase)
		if !strings.Contains(runbooks, "{ #"+anchor+" }") {
			t.Errorf("alert %s: docs/runbooks.md has no heading with the anchor { #%s }", name, anchor)
		}
	}
}

func TestRunbookSectionsHaveAlerts(t *testing.T) {
	alerts := alertRunbookURLs(t, readRepoFile(t, alertsFile))
	known := map[string]bool{}
	for name := range alerts {
		known[strings.ToLower(name)] = true
	}
	headingRe := regexp.MustCompile(`(?m)^#{2,4} .*\{ #(\w+) \}\s*$`)
	for _, m := range headingRe.FindAllStringSubmatch(readRepoFile(t, runbooksFile), -1) {
		if !known[m[1]] {
			t.Errorf("docs/runbooks.md has a section %q for an alert that does not exist", m[1])
		}
	}
}

func TestAlertTestsAssertRunbookURL(t *testing.T) {
	text := readRepoFile(t, alertsTestFile)
	if blocks, urls := strings.Count(text, "exp_annotations:"), strings.Count(text, runbookURLField); blocks == 0 || blocks != urls {
		t.Fatalf("%s has %d exp_annotations blocks and %d runbook_url lines: every expected alert must assert its runbook_url", alertsTestFile, blocks, urls)
	}
}

func TestHelmAlertsMatchShippedAlerts(t *testing.T) {
	if readRepoFile(t, alertsFile) != readRepoFile(t, helmAlertsFile) {
		t.Fatal("deploy/helm/mongorescue/files/alerts.yml differs from deploy/prometheus/alerts.yml")
	}
}

func TestHelmDashboardMatchesShippedDashboard(t *testing.T) {
	if readRepoFile(t, dashboardFile) != readRepoFile(t, helmDashboardFile) {
		t.Fatal("deploy/helm/mongorescue/files/grafana-dashboard.json differs from deploy/grafana/mongorescue.json")
	}
}

// registeredMetricNames returns the metric names the package can register, read from
// the string literals of its non-test files: the HA and PITR collectors prefix their
// names with a subsystem, the others do not.
func registeredMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	subsystems := map[string]string{"ha.go": "ha_", "pitr.go": "pitr_"}
	nameRe := regexp.MustCompile(`^_?[a-z][a-z0-9_]*$`)
	names := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !nameRe.MatchString(s) {
				return true
			}
			switch {
			case strings.HasPrefix(s, "mongorescue_"):
				names[s] = true
			case strings.HasPrefix(s, "_"):
				names["mongorescue"+s] = true
			default:
				names["mongorescue_"+subsystems[file]+s] = true
			}
			return true
		})
	}
	return names
}

func TestRegisteredMetricNamesAreFound(t *testing.T) {
	names := registeredMetricNames(t)
	for _, want := range []string{
		"mongorescue_backups_total", "mongorescue_build_info", "mongorescue_data_dir_free_bytes",
		"mongorescue_job_rpo_met", "mongorescue_ha_role", "mongorescue_pitr_lag_seconds",
		"mongorescue_api_rate_limited_total", "mongorescue_backup_duration_seconds",
	} {
		if !names[want] {
			t.Errorf("the metric name scan does not find %s", want)
		}
	}
}

func TestGrafanaDashboard(t *testing.T) {
	raw := readRepoFile(t, dashboardFile)
	var dash struct {
		UID           string `json:"uid"`
		Title         string `json:"title"`
		SchemaVersion int    `json:"schemaVersion"`
		Templating    struct {
			List []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"list"`
		} `json:"templating"`
		Panels []struct {
			ID      int    `json:"id"`
			Type    string `json:"type"`
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(raw), &dash); err != nil {
		t.Fatalf("the dashboard is not valid JSON: %v", err)
	}
	if dash.UID == "" || dash.Title == "" {
		t.Error("the dashboard needs a uid and a title")
	}
	if dash.SchemaVersion < 39 {
		t.Errorf("schemaVersion = %d, want 39 or newer (Grafana 10)", dash.SchemaVersion)
	}

	vars := map[string]string{}
	for _, v := range dash.Templating.List {
		vars[v.Name] = v.Type
	}
	if vars["datasource"] != "datasource" {
		t.Error("the dashboard needs a datasource variable")
	}
	for _, name := range []string{"job", "database", "stream"} {
		if vars[name] != "query" {
			t.Errorf("the dashboard needs a %q query variable", name)
		}
	}

	ids := map[int]bool{}
	charts := 0
	for _, p := range dash.Panels {
		if p.ID == 0 || ids[p.ID] {
			t.Errorf("panel %q has a missing or duplicate id %d", p.Title, p.ID)
		}
		ids[p.ID] = true
		if p.Type == "row" {
			continue
		}
		charts++
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no query", p.Title)
		}
		for _, target := range p.Targets {
			if strings.TrimSpace(target.Expr) == "" {
				t.Errorf("panel %q has an empty query", p.Title)
			}
		}
	}
	if charts < 30 {
		t.Errorf("the dashboard has only %d panels", charts)
	}

	registered := registeredMetricNames(t)
	suffixes := []string{"_bucket", "_sum", "_count"}
	used := map[string]bool{}
	for _, name := range regexp.MustCompile(`mongorescue_[a-z0-9_]+`).FindAllString(raw, -1) {
		used[name] = true
	}
	var unknown []string
	for name := range used {
		if registered[name] {
			continue
		}
		base := name
		for _, suffix := range suffixes {
			base = strings.TrimSuffix(base, suffix)
		}
		if base != name && registered[base] {
			continue
		}
		unknown = append(unknown, name)
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("the dashboard queries metrics that are not registered: %s", strings.Join(unknown, ", "))
	}
}
