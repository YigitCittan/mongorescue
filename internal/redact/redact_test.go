package redact

import "testing"

func TestURI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "no scheme", input: "localhost:27017", want: "localhost:27017"},
		{name: "no credentials", input: "mongodb://localhost:27017/db", want: "mongodb://localhost:27017/db"},
		{name: "user only", input: "mongodb://app@h/db", want: "mongodb://app@h/db"},
		{name: "user and password", input: "mongodb://u:secret@h/db", want: "mongodb://u:******@h/db"},
		{name: "srv with query", input: "mongodb+srv://u:p%40ss@c.net/?w=1", want: "mongodb+srv://u:******@c.net/?w=1"},
		{name: "password query param", input: "mongodb://h/db?authSource=admin&password=hunter2&w=1", want: "mongodb://h/db?authSource=admin&password=******&w=1"},
		{name: "query param case-insensitive", input: "mongodb://h/?PassWord=x", want: "mongodb://h/?PassWord=******"},
		{
			name:  "aws auth mechanism properties masked whole",
			input: "mongodb://h/?authMechanism=MONGODB-AWS&authMechanismProperties=AWS_SESSION_TOKEN:tok123,SERVICE_NAME:x&w=1",
			want:  "mongodb://h/?authMechanism=MONGODB-AWS&authMechanismProperties=******&w=1",
		},
		{name: "tls key password", input: "mongodb://h/?tlsCertificateKeyFilePassword=k1&tls=true", want: "mongodb://h/?tlsCertificateKeyFilePassword=******&tls=true"},
		{name: "ssl pem password", input: "mongodb://h/?sslPEMKeyPassword=k2", want: "mongodb://h/?sslPEMKeyPassword=******"},
		{name: "secret key variants", input: "mongodb://h/?secretKey=a&secret_key=b", want: "mongodb://h/?secretKey=******&secret_key=******"},
		{name: "standalone session token", input: "mongodb://h/?AWS_SESSION_TOKEN=t", want: "mongodb://h/?AWS_SESSION_TOKEN=******"},
		{name: "userinfo and query", input: "mongodb://u:p@h/?password=q", want: "mongodb://u:******@h/?password=******"},
		{name: "non-sensitive param kept", input: "mongodb://h/?passwordless=1&replicaSet=rs0", want: "mongodb://h/?passwordless=1&replicaSet=rs0"},
		// Round 3: userinfo delimited by the LAST '@'; raw reserved chars in the password.
		{name: "raw slash in password", input: "mongodb://u:pa/ss@h/db", want: "mongodb://u:******@h/db"},
		{name: "raw question mark in password", input: "mongodb://u:pa?ss@h/db", want: "mongodb://u:******@h/db"},
		{name: "raw hash in password", input: "mongodb://u:pa#ss@h/db", want: "mongodb://u:******@h/db"},
		{name: "raw at in password multi-host", input: "mongodb://u:p@ss@h1,h2/db", want: "mongodb://u:******@h1,h2/db"},
		{name: "empty username", input: "mongodb://:secret@h/db", want: "mongodb://:******@h/db"},
		{name: "srv with query", input: "mongodb+srv://u:p@h/db?x=1", want: "mongodb+srv://u:******@h/db?x=1"},
		{name: "no password unchanged", input: "mongodb://u@h", want: "mongodb://u@h"},
		{name: "no userinfo unchanged", input: "mongodb://h1:27017,h2:27017/db?replicaSet=rs0", want: "mongodb://h1:27017,h2:27017/db?replicaSet=rs0"},
		// Round 3: percent-encoded query keys.
		{name: "percent-encoded key", input: "mongodb://h/?pass%77ord=x&w=1", want: "mongodb://h/?pass%77ord=******&w=1"},
		{name: "percent-encoded mixed case key", input: "mongodb://h/?%50ASSWORD=x", want: "mongodb://h/?%50ASSWORD=******"},
		{name: "undecodable key matched raw", input: "mongodb://h/?password%zz=x&secretKey=y", want: "mongodb://h/?password%zz=x&secretKey=******"},
		{name: "value stops at fragment", input: "mongodb://h/?password=x#frag", want: "mongodb://h/?password=******#frag"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := URI(tt.input); got != tt.want {
				t.Errorf("URI(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "no uri", input: "connection refused", want: "connection refused"},
		{
			name:  "two uris in one string",
			input: "failed mongodb://a:pass1@h1:27017/db then retried mongodb+srv://b:pass2@c.net/x",
			want:  "failed mongodb://a:******@h1:27017/db then retried mongodb+srv://b:******@c.net/x",
		},
		{
			name:  "multiline stderr",
			input: "error: mongodb://a:s1@h/\nerror: mongodb://b:s2@h/?password=s3",
			want:  "error: mongodb://a:******@h/\nerror: mongodb://b:******@h/?password=******",
		},
		{name: "user only kept", input: "mongodb://app@h/db", want: "mongodb://app@h/db"},
		{
			name:  "each uri independent despite raw at signs",
			input: "a mongodb://u:p@ss@h1/db b mongodb://v:q/r@h2/db c",
			want:  "a mongodb://u:******@h1/db b mongodb://v:******@h2/db c",
		},
		{
			name:  "adjacent uris without whitespace",
			input: "mongodb://a:p1@h1,mongodb://b:p2@h2",
			want:  "mongodb://a:******@h1,mongodb://b:******@h2",
		},
		{name: "empty username in text", input: "dial mongodb://:secret@h/db failed", want: "dial mongodb://:******@h/db failed"},
		{name: "query secret in text", input: "x mongodb://h/?PASSWORD=s1 y", want: "x mongodb://h/?PASSWORD=****** y"},
		{name: "text without uri untouched", input: "open /tmp/x?password=1", want: "open /tmp/x?password=1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.input); got != tt.want {
				t.Errorf("Text(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}
