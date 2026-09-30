package mongotools

import "testing"

func TestWithConnectionDefaults(t *testing.T) {
	const both = "serverSelectionTimeoutMS=30000&connectTimeoutMS=10000"
	cases := []struct{ in, want string }{
		{"mongodb://localhost:27017", "mongodb://localhost:27017/?" + both},
		{"mongodb://u:p%40ss@h1:1,h2:2/db", "mongodb://u:p%40ss@h1:1,h2:2/db?authSource=db&" + both},
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

func TestWithConnectionDefaultsAuthSource(t *testing.T) {
	const both = "serverSelectionTimeoutMS=30000&connectTimeoutMS=10000"
	cases := []struct{ name, in, want string }{
		{"no credentials", "mongodb://10.1.1.190:27017", "mongodb://10.1.1.190:27017/?" + both},
		{"no credentials with path", "mongodb://h:27017/mydb", "mongodb://h:27017/mydb?" + both},
		{"credentials without path", "mongodb://sa:pw@10.1.1.190:27017", "mongodb://sa:pw@10.1.1.190:27017/?authSource=admin&" + both},
		{"credentials with empty path", "mongodb://sa:pw@h/", "mongodb://sa:pw@h/?authSource=admin&" + both},
		{"credentials with path", "mongodb://sa:pw@h:27017/mydb", "mongodb://sa:pw@h:27017/mydb?authSource=mydb&" + both},
		{"path is escaped", "mongodb://sa:pw@h/my%20db", "mongodb://sa:pw@h/my%20db?authSource=my+db&" + both},
		{"plus in path is a space", "mongodb://sa:pw@h/my+db", "mongodb://sa:pw@h/my+db?authSource=my+db&" + both},
		{"escaped option name", "mongodb://sa:pw@h/?auth%53ource=other", "mongodb://sa:pw@h/?auth%53ource=other&" + both},
		{"existing authSource", "mongodb://sa:pw@h/mydb?authSource=other", "mongodb://sa:pw@h/mydb?authSource=other&" + both},
		{"existing authsource lower case", "mongodb://sa:pw@h/?authsource=other", "mongodb://sa:pw@h/?authsource=other&" + both},
		{"existing AUTHSOURCE upper case", "mongodb://sa:pw@h/?AUTHSOURCE=other", "mongodb://sa:pw@h/?AUTHSOURCE=other&" + both},
		{"x509 mechanism", "mongodb://CN=client@h/?authMechanism=MONGODB-X509", "mongodb://CN=client@h/?authMechanism=MONGODB-X509&" + both},
		{"gssapi mechanism", "mongodb://u%40REALM@h/?authMechanism=GSSAPI", "mongodb://u%40REALM@h/?authMechanism=GSSAPI&" + both},
		{"aws mechanism", "mongodb://k:s@h/?authMechanism=MONGODB-AWS", "mongodb://k:s@h/?authMechanism=MONGODB-AWS&" + both},
		{"scram-sha-256 mechanism", "mongodb://sa:pw@h/?authMechanism=SCRAM-SHA-256", "mongodb://sa:pw@h/?authMechanism=SCRAM-SHA-256&authSource=admin&" + both},
		{"scram-sha-1 mechanism lower case", "mongodb://sa:pw@h/db?authmechanism=scram-sha-1", "mongodb://sa:pw@h/db?authmechanism=scram-sha-1&authSource=db&" + both},
		{"srv scheme", "mongodb+srv://sa:pw@cluster.example.net/mydb", "mongodb+srv://sa:pw@cluster.example.net/mydb?" + both},
		{"existing query string", "mongodb://sa:pw@h/mydb?tls=true&replicaSet=rs0", "mongodb://sa:pw@h/mydb?tls=true&replicaSet=rs0&authSource=mydb&" + both},
		{"query without path", "mongodb://sa:pw@h?tls=true", "mongodb://sa:pw@h?tls=true&authSource=admin&" + both},
		{"trailing question mark", "mongodb://sa:pw@h/mydb?", "mongodb://sa:pw@h/mydb?authSource=mydb&" + both},
		{"encoded at sign in password", "mongodb://sa:p%40ss@h:27017", "mongodb://sa:p%40ss@h:27017/?authSource=admin&" + both},
		{"all options present", "mongodb://sa:pw@h/?authSource=admin&serverSelectionTimeoutMS=1&connectTimeoutMS=2", "mongodb://sa:pw@h/?authSource=admin&serverSelectionTimeoutMS=1&connectTimeoutMS=2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithConnectionDefaults(tc.in); got != tc.want {
				t.Errorf("WithConnectionDefaults(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
