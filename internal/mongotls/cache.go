package mongotls

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"sync"
	"sync/atomic"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// maxCachedCerts bounds the parsed client certificates kept in memory.
const maxCachedCerts = 64

// certCache keeps parsed client certificates, so an encrypted key's derivation
// runs once per connection material instead of once per client. Entries are
// keyed by an HMAC of the certificate, key and password under a random
// per-process key: a changed connection (update) gets a new entry, and the
// connection service drops the old one (Forget); a secret.key rotation drops all
// (ForgetAll). The key never leaves the process, so the entry names are not a
// password-guessing oracle.
type certCache struct {
	mu    sync.Mutex
	key   []byte
	certs map[[sha256.Size]byte]tls.Certificate
	order [][sha256.Size]byte
}

// cache is the process-wide certificate cache.
var cache = newCertCache()

// kdfRuns counts the key derivations of encrypted keys (tests).
var kdfRuns atomic.Int64

func newCertCache() *certCache {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		panic("mongotls: random cache key: " + err.Error())
	}
	return &certCache{key: key, certs: map[[sha256.Size]byte]tls.Certificate{}}
}

// id returns the cache key of a certificate, key and password.
func (c *certCache) id(certPEM, keyPEM, password string) [sha256.Size]byte {
	m := hmac.New(sha256.New, c.key)
	for _, part := range []string{certPEM, keyPEM, password} {
		m.Write(binary.LittleEndian.AppendUint64(nil, uint64(len(part))))
		m.Write([]byte(part))
	}
	var out [sha256.Size]byte
	copy(out[:], m.Sum(nil))
	return out
}

func (c *certCache) get(id [sha256.Size]byte) (tls.Certificate, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cert, ok := c.certs[id]
	return cert, ok
}

func (c *certCache) put(id [sha256.Size]byte, cert tls.Certificate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.certs[id]; ok {
		return
	}
	for len(c.order) >= maxCachedCerts {
		delete(c.certs, c.order[0])
		c.order = c.order[1:]
	}
	c.certs[id] = cert
	c.order = append(c.order, id)
}

func (c *certCache) forget(id [sha256.Size]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.certs[id]; !ok {
		return
	}
	delete(c.certs, id)
	for i, o := range c.order {
		if o == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

func (c *certCache) forgetAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.certs = map[[sha256.Size]byte]tls.Certificate{}
	c.order = nil
}

// Forget drops the parsed client certificate of t from the cache. The connection
// service calls it with a connection's previous material when it changes or the
// connection is deleted.
func Forget(t *models.ConnectionTLS) {
	if t.IsZero() || t.ClientCertPEM == "" {
		return
	}
	cache.forget(cache.id(t.ClientCertPEM, t.ClientKeyPEM, t.ClientKeyPassword))
}

// ForgetAll drops every cached client certificate (after a secret.key rotation).
func ForgetAll() {
	cache.forgetAll()
}
