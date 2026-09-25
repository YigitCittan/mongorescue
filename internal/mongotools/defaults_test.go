package mongotools

import "testing"

func TestWithConnectionDefaults(t *testing.T) {
	const both = "serverSelectionTimeoutMS=30000&connectTimeoutMS=10000"
	cases := []struct{ in, want string }{
		{"mongodb://localhost:27017", "mongodb://localhost:27017/?" + both},
		{"mongodb://u:p%40ss@h1:1,h2:2/db", "mongodb://u:p%40ss@h1:1,h2:2/db?" + both},
		{"mongodb://h/db?authSource=admin", "mongodb://h/db?authSource=admin&" + both},
		{"mongodb://h/?", "mongodb://h/?" + both},
		{"mongodb+srv://cluster.example.net/?retryWrites=true", "mongodb+srv://cluster.example.net/?retryWrites=true&" + both},
		{"mongodb://h/?serverSelectionTimeoutMS=5000", "mongodb://h/?serverSelectionTimeoutMS=5000&connectTimeoutMS=10000"},
		{"mongodb://h/?connecttimeoutms=1&SERVERSELECTIONTIMEOUTMS=2", "mongodb://h/?connecttimeoutms=1&SERVERSELECTIONTIMEOUTMS=2"},
		{"not-a-uri", "not-a-uri"},
	}
	for _, tc := range cases {
		if got := WithConnectionDefaults(tc.in); got != tc.want {
			t.Errorf("WithConnectionDefaults(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
