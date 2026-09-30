package restore

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/encryption"
)

// Stored backups are layered as mongodump archive -> [gzip] -> [age]. The restore
// pipeline peels the layers from the content itself where it carries a signature,
// and falls back to the storage key and the backup record otherwise, so a backup
// whose key does not follow the naming scheme (a custom target_key accepted by earlier
// releases) or whose record lost its encryption flag still restores correctly.

// ageMagic is the first line of every binary age file (https://age-encryption.org/v1).
const ageMagic = "age-encryption.org/v1\n"

var (
	// gzipMagic starts every gzip stream (RFC 1952), as written by mongodump --gzip.
	gzipMagic = []byte{0x1f, 0x8b}
	// archiveMagic starts every uncompressed mongodump archive: the little-endian
	// encoding of the archive magic number 0x8199e26d.
	archiveMagic = []byte{0x6d, 0xe2, 0x99, 0x81}
)

// keyEncrypted reports whether a storage key names an age-encrypted artifact.
func keyEncrypted(key string) bool {
	return strings.HasSuffix(key, encryption.FileExtension)
}

// keyGzip reports whether a storage key names a gzip-compressed archive.
func keyGzip(key string) bool {
	return strings.HasSuffix(strings.TrimSuffix(key, encryption.FileExtension), ".gz")
}

// sniffEncrypted reports whether r starts with the age header, without consuming it.
// A read error of the source is returned; a stream shorter than the header is not
// encrypted.
func sniffEncrypted(r *bufio.Reader) (bool, error) {
	head, err := r.Peek(len(ageMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return string(head) == ageMagic, nil
}

// sniffGzip reports whether the plaintext archive in r is gzip-compressed, without
// consuming it. The content decides when it starts with the gzip or the mongodump
// archive signature; otherwise the storage key does. A read error (for an encrypted
// backup, a failure to authenticate the first chunk) is returned.
func sniffGzip(r *bufio.Reader, key string) (bool, error) {
	head, err := r.Peek(len(archiveMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch {
	case bytes.HasPrefix(head, gzipMagic):
		return true, nil
	case bytes.Equal(head, archiveMagic):
		return false, nil
	default:
		return keyGzip(key), nil
	}
}
