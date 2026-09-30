package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongouri"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestBackupRunsURIsFromOlderReleases feeds stored connection strings that the
// validation of v0.7.0 accepted, and the stricter mongouri.Validate rejects, through
// a backup run: the run must still reach mongodump with the URI in its config file.
func TestBackupRunsURIsFromOlderReleases(t *testing.T) {
	for _, uri := range []string{
		"mongodb://u:pa\u00a0ss@h:27017/db", // unencoded no-break space in the password
		"mongodb://u:pa\u3000ss@h/db",       // ideographic space
		"mongodb://u:p\u2028x@h/db",         // line separator
		"mongodb://u:p\u0085x@h/db",         // NEL
		"mongodb://u:100%@h/db",             // raw '%'
		"mongodb://h{x}/db",                 // host character outside RFC 3986
	} {
		if mongouri.Validate(uri) == nil || mongouri.ValidateStored(uri) != nil {
			t.Fatalf("test input %q must be valid only under the older rules", uri)
		}
		var config []byte
		runner := func(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
			for _, a := range args {
				if p, ok := strings.CutPrefix(a, "--config="); ok {
					config, _ = os.ReadFile(p) //nolint:gosec // G304: the config file of this run.
				}
			}
			return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
		}
		engine := NewEngine(storage.NewMockStorage(), "mongodb://unused", WithRunner(runner))
		if _, err := engine.Run(context.Background(), models.BackupOptions{Database: "db", MongoURI: uri}); err != nil {
			t.Errorf("backup with stored URI %q: %v", uri, err)
			continue
		}
		if !bytes.HasPrefix(config, []byte("uri: ")) {
			t.Errorf("backup with stored URI %q: config file %q", uri, config)
		}
	}
}
