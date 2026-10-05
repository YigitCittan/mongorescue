package mongotools

import (
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
)

func TestWithReadPreference(t *testing.T) {
	east := map[string]string{"dc": "east", "use": "backup"}
	cases := []struct {
		name string
		uri  string
		mode string
		tags []map[string]string
		want string
	}{
		{"no mode keeps the uri", "mongodb://u:p@h1/?readPreference=secondary", "", east1(), "mongodb://u:p@h1/?readPreference=secondary"},
		{"bare host", "mongodb://h1", "secondary", nil, "mongodb://h1/?readPreference=secondary"},
		{"path without query", "mongodb://h1/admin", "nearest", nil, "mongodb://h1/admin?readPreference=nearest"},
		{"trailing question mark", "mongodb://h1/?", "secondary", nil, "mongodb://h1/?readPreference=secondary"},
		{"keeps other options", "mongodb://u:p@h1,h2/?replicaSet=rs0&authSource=admin", "secondaryPreferred", nil,
			"mongodb://u:p@h1,h2/?replicaSet=rs0&authSource=admin&readPreference=secondaryPreferred"},
		{"replaces the uri's own, any case", "mongodb://h1/?READPREFERENCE=primary&readPreferenceTags=dc:west&w=1", "secondary", nil,
			"mongodb://h1/?w=1&readPreference=secondary"},
		{"replaces percent-encoded names", "mongodb://h1/?read%50reference=primary", "secondary", nil,
			"mongodb://h1/?readPreference=secondary"},
		{"tag sets in order, keys sorted", "mongodb+srv://c.example.net/", "secondary", []map[string]string{east, {"dc": "west"}, {}},
			"mongodb+srv://c.example.net/?readPreference=secondary&readPreferenceTags=dc:east,use:backup&readPreferenceTags=dc:west&readPreferenceTags="},
		{"escapes tag values", "mongodb://h1/", "nearest", []map[string]string{{"rack": "a&b=c d"}},
			"mongodb://h1/?readPreference=nearest&readPreferenceTags=rack:a%26b%3Dc+d"},
		{"not a uri", "h1:27017", "secondary", nil, "h1:27017"},
		{"primary drops maxStalenessSeconds", "mongodb://h1/?readPreference=secondary&maxStalenessSeconds=120&w=1", "primary", nil,
			"mongodb://h1/?w=1&readPreference=primary"},
		{"primary drops it in any case", "mongodb://h1/?MAXSTALENESSSECONDS=120", "primary", nil, "mongodb://h1/?readPreference=primary"},
		{"other modes keep maxStalenessSeconds", "mongodb://h1/?maxStalenessSeconds=120", "secondary", nil,
			"mongodb://h1/?maxStalenessSeconds=120&readPreference=secondary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithReadPreference(tc.uri, tc.mode, tc.tags); got != tc.want {
				t.Fatalf("WithReadPreference = %q, want %q", got, tc.want)
			}
		})
	}
}

func east1() []map[string]string { return []map[string]string{{"dc": "east"}} }

func TestHasOption(t *testing.T) {
	for uri, want := range map[string]bool{
		"mongodb://h1/?maxStalenessSeconds=90":      true,
		"mongodb://h1/?w=1&maxstalenessseconds=90":  true,
		"mongodb://h1/?max%53talenessSeconds=90":    true,
		"mongodb://h1/?w=1":                         false,
		"mongodb://h1/maxStalenessSeconds":          false,
		"mongodb://u:maxStalenessSeconds=1@h1/?w=1": false,
	} {
		if got := HasOption(uri, "maxStalenessSeconds"); got != want {
			t.Errorf("HasOption(%q) = %v, want %v", uri, got, want)
		}
	}
}

// A job that switches a connection with maxStalenessSeconds to the primary gets a
// connection string the driver accepts.
func TestPrimaryWithoutMaxStalenessParses(t *testing.T) {
	uri := WithReadPreference("mongodb://h1,h2/?replicaSet=rs0&readPreference=secondary&maxStalenessSeconds=120", "primary", nil)
	cs, err := connstring.ParseAndValidate(uri)
	if err != nil || cs.ReadPreference != "primary" || cs.MaxStalenessSet {
		t.Fatalf("parsed %q: %+v, %v", uri, cs, err)
	}
}

// The driver (and the Database Tools, which use its parser) read back exactly the
// read preference and tag sets that were set.
func TestWithReadPreferenceParsesInTheDriver(t *testing.T) {
	tags := []map[string]string{{"dc": "east", "use": "backup"}, {"rack": "a&b=c d"}, {}}
	for _, mode := range []string{"primaryPreferred", "secondary", "secondaryPreferred", "nearest"} {
		uri := WithReadPreference("mongodb://u:p@h1:27017,h2:27017/?replicaSet=rs0&readPreference=primary", mode, tags)
		cs, err := connstring.ParseAndValidate(uri)
		if err != nil {
			t.Fatalf("%s: parse %q: %v", mode, uri, err)
		}
		if cs.ReadPreference != mode {
			t.Errorf("%s: read preference %q", mode, cs.ReadPreference)
		}
		if !reflect.DeepEqual(cs.ReadPreferenceTagSets, tags) {
			t.Errorf("%s: tag sets %v, want %v", mode, cs.ReadPreferenceTagSets, tags)
		}
		if cs.ReplicaSet != "rs0" || cs.Username != "u" {
			t.Errorf("%s: other options lost: %+v", mode, cs)
		}
	}
	// The connection defaults compose with it.
	cs, err := connstring.ParseAndValidate(WithConnectionDefaults(WithReadPreference("mongodb://u:p@h1", "secondary", nil)))
	if err != nil || cs.ReadPreference != "secondary" || cs.AuthSource != "admin" {
		t.Fatalf("with defaults: %+v, %v", cs, err)
	}
}
