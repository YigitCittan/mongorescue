package mongouri

import "testing"

func TestHasUserinfo(t *testing.T) {
	for uri, want := range map[string]bool{
		"mongodb://u:p@h:27017/db":                    true,
		"mongodb://u@h":                               true,
		"mongodb+srv://h.example.net/?authSource=a@b": false,
		"mongodb://h1,h2/db?replicaSet=rs0":           false,
		"postgres://u:p@h":                            false,
	} {
		if got := HasUserinfo(uri); got != want {
			t.Errorf("HasUserinfo(%q) = %v; want %v", uri, got, want)
		}
	}
}

func TestWithUserinfo(t *testing.T) {
	for _, tc := range []struct{ in, user, pass, want string }{
		{"mongodb://h:27017/?authSource=admin", "v-user", "p@ss/w:rd?#[]%", "mongodb://v-user:p%40ss%2Fw%3Ard%3F%23%5B%5D%25@h:27017/?authSource=admin"},
		{"mongodb://old:pw@h1,h2/db?x=a@b", "new", "pw2", "mongodb://new:pw2@h1,h2/db?x=a@b"},
		{"mongodb+srv://cluster.example.net", "u", "p", "mongodb+srv://u:p@cluster.example.net"},
		{"ftp://h", "u", "p", "ftp://h"},
	} {
		got := WithUserinfo(tc.in, tc.user, tc.pass)
		if got != tc.want {
			t.Errorf("WithUserinfo(%q) = %q; want %q", tc.in, got, tc.want)
		}
		if tc.in != "ftp://h" {
			if err := Validate(got); err != nil {
				t.Errorf("WithUserinfo(%q) = %q does not validate: %v", tc.in, got, err)
			}
		}
	}
}
