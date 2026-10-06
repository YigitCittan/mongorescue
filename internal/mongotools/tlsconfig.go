package mongotools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// ErrInvalidKeyPassword is returned when a TLS client key password contains ASCII
// control characters, which cannot be represented safely in the configuration file.
var ErrInvalidKeyPassword = errors.New("mongotools: tls client key password contains control characters")

// TLSDirPattern is the os.MkdirTemp pattern of the private directories that hold the
// tools configuration and TLS files of one run. CleanupStale removes leftovers.
const TLSDirPattern = "mongorescue-tls-*"

// Files of a TLS directory.
const (
	tlsConfigFile = "config.yaml"
	tlsCAFile     = "ca.pem"
	tlsClientFile = "client.pem"
)

// tlsDirPerm restricts a TLS directory to the current user.
const tlsDirPerm = 0o700

// WriteConfig returns the mongodump/mongorestore arguments that connect with uri and
// the TLS material t, and a cleanup function that removes every file it wrote.
// Callers must defer cleanup right away; it is safe to call more than once.
//
// Without TLS material it is WriteURIConfig. With it, a private directory (0700) is
// created in dir (os.TempDir when empty) holding config.yaml (the URI and, for an
// encrypted key, sslPEMKeyPassword: the only secrets the tools read from --config),
// ca.pem and client.pem (the certificate followed by its key), each 0600. The
// arguments name the files (--sslCAFile, --sslPEMKeyFile), whose paths are not
// secret, and turn TLS on (--ssl) together with the loosened checks t asks for.
func WriteConfig(dir, uri string, t *models.ConnectionTLS) (args []string, cleanup func(), err error) {
	if t.IsZero() {
		arg, c, uriErr := WriteURIConfig(dir, uri)
		if uriErr != nil {
			return nil, nil, uriErr
		}
		return []string{arg}, c, nil
	}
	if strings.ContainsFunc(uri, isControl) {
		return nil, nil, ErrInvalidURI
	}
	if strings.ContainsFunc(t.ClientKeyPassword, isControl) {
		return nil, nil, ErrInvalidKeyPassword
	}

	tmp, err := os.MkdirTemp(dir, TLSDirPattern)
	if err != nil {
		return nil, nil, fmt.Errorf("create tools TLS directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	if err = os.Chmod(tmp, tlsDirPerm); err != nil {
		return nil, nil, fmt.Errorf("restrict tools TLS directory permissions: %w", err)
	}

	config := "uri: " + yamlScalar(uri) + "\n"
	if t.ClientKeyPassword != "" {
		config += "sslPEMKeyPassword: " + yamlScalar(t.ClientKeyPassword) + "\n"
	}
	configPath := filepath.Join(tmp, tlsConfigFile)
	if err = writePrivate(configPath, config); err != nil {
		return nil, nil, err
	}
	args = []string{"--config=" + configPath, "--ssl"}
	if t.CAPEM != "" {
		path := filepath.Join(tmp, tlsCAFile)
		if err = writePrivate(path, t.CAPEM); err != nil {
			return nil, nil, err
		}
		args = append(args, "--sslCAFile="+path)
	}
	if t.ClientCertPEM != "" {
		path := filepath.Join(tmp, tlsClientFile)
		if err = writePrivate(path, pemJoin(t.ClientCertPEM, t.ClientKeyPEM)); err != nil {
			return nil, nil, err
		}
		args = append(args, "--sslPEMKeyFile="+path)
	}
	switch {
	case t.Insecure:
		args = append(args, "--sslAllowInvalidCertificates", "--sslAllowInvalidHostnames")
	case t.AllowInvalidHostnames:
		args = append(args, "--sslAllowInvalidHostnames")
	}
	return args, cleanup, nil
}

// isControl reports an ASCII control character.
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// pemJoin concatenates PEM values, each ending in a newline.
func pemJoin(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p)
		if !strings.HasSuffix(p, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// writePrivate creates path, which must not exist, with mode 0600 and writes data.
func writePrivate(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, configFilePerm)
	if err != nil {
		return fmt.Errorf("create tools TLS file: %w", err)
	}
	if err = f.Chmod(configFilePerm); err != nil {
		_ = f.Close()
		return fmt.Errorf("restrict tools TLS file permissions: %w", err)
	}
	if _, err = f.WriteString(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write tools TLS file: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close tools TLS file: %w", err)
	}
	return nil
}

// RequiresTLS reports whether uri asks for TLS: tls=true or ssl=true (any case).
func RequiresTLS(uri string) bool {
	for _, name := range []string{"tls", "ssl"} {
		if v, ok := OptionValue(uri, name); ok && strings.EqualFold(v, "true") {
			return true
		}
	}
	return false
}

// EnsureTLS returns uri with tls=true appended unless it already asks for TLS
// (RequiresTLS). A connection with TLS material stores such a URI, so that a
// client built from the URI alone uses TLS too, and fails against a server whose
// certificate the system roots do not trust instead of connecting in plain text.
// uri must not turn TLS off (tls=false or ssl=false): callers refuse that first.
func EnsureTLS(uri string) string {
	if RequiresTLS(uri) {
		return uri
	}
	schemeEnd := strings.Index(uri, "://")
	if schemeEnd == -1 {
		return uri
	}
	rest := uri[schemeEnd+3:]
	switch {
	case strings.Contains(rest, "?"):
		if strings.HasSuffix(uri, "?") || strings.HasSuffix(uri, "&") {
			return uri + "tls=true"
		}
		return uri + "&tls=true"
	case strings.Contains(rest, "/"):
		return uri + "?tls=true"
	default:
		return uri + "/?tls=true"
	}
}
