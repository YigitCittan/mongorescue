package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Download errors.
var (
	// ErrNoChecksum is returned when the release has no checksums file or the file
	// lists no valid SHA-256 for the asset.
	ErrNoChecksum = errors.New("no checksum for the release asset")
	// ErrChecksumMismatch is returned when the downloaded file does not match its
	// published SHA-256.
	ErrChecksumMismatch = errors.New("checksum mismatch")
	// ErrAssetTooLarge is returned when the asset exceeds MaxAssetSize.
	ErrAssetTooLarge = errors.New("release asset too large")
)

// File is a downloaded and verified release file.
type File struct {
	// Path is the file's location.
	Path string
	// SHA256 is its published and verified digest; check it again right before
	// using the file when others can write to its directory.
	SHA256 []byte
}

// Download fetches the checksums file and the asset of res into dir, verifying the
// asset's SHA-256 while it streams to a temporary file (mode 0600) in dir. On
// success the file is renamed to the asset name, replacing an older copy, and
// returned; on failure nothing is left in dir. It returns an error wrapping
// ErrUnsupportedPlatform when res has no asset, ErrNoChecksum or ErrChecksumMismatch
// when the file cannot be verified.
func (c *Checker) Download(ctx context.Context, res Result, dir string) (File, error) {
	return c.DownloadAsset(ctx, res, res.Asset, dir, nil)
}

// Progress receives the bytes of a download received so far and the expected
// total, or 0 when the total is unknown. It is called from the downloading
// goroutine and must not block.
type Progress func(done, total int64)

// DownloadAsset is Download for asset, which must be the platform's file of
// res.Latest: res.Asset or, on Windows, res.Portable (checked by name). It reports
// the progress to progress when that is not nil.
func (c *Checker) DownloadAsset(ctx context.Context, res Result, asset Asset, dir string, progress Progress) (File, error) {
	if asset.URL == "" {
		return File{}, fmt.Errorf("%w: %s/%s", ErrUnsupportedPlatform, c.goos(), c.goarch())
	}
	name, err := AssetName(c.goos(), c.goarch(), res.Latest)
	if err != nil {
		return File{}, err
	}
	if portable, perr := PortableAssetName(c.goos(), c.goarch(), res.Latest); perr == nil && asset.Name == portable {
		name = portable
	}
	if asset.Name != name {
		return File{}, fmt.Errorf("%w: asset %q, want %q", ErrUntrustedURL, asset.Name, name)
	}
	if !c.trusted(asset.URL, c.downloadPrefix()) {
		return File{}, fmt.Errorf("%w: %s", ErrUntrustedURL, asset.URL)
	}
	if res.ChecksumsURL == "" {
		return File{}, ErrNoChecksum
	}
	if !c.trusted(res.ChecksumsURL, c.downloadPrefix()) {
		return File{}, fmt.Errorf("%w: %s", ErrUntrustedURL, res.ChecksumsURL)
	}
	ua := UserAgent(res.Current.String())
	want, err := c.checksum(ctx, res.ChecksumsURL, name, ua)
	if err != nil {
		return File{}, err
	}

	body, size, err := c.get(ctx, c.downloadClient(), asset.URL, ua)
	if err != nil {
		return File{}, fmt.Errorf("download %s: %w", name, err)
	}
	if size <= 0 {
		size = max(asset.Size, 0)
	}
	defer func() { _ = body.Close() }()

	tmp, err := os.CreateTemp(dir, ".mongorescue-update-*.part")
	if err != nil {
		return File{}, fmt.Errorf("create download file: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	h := sha256.New()
	var w io.Writer = io.MultiWriter(tmp, h)
	if progress != nil {
		progress(0, size)
		w = &progressWriter{w: w, total: size, report: progress}
	}
	n, err := io.Copy(w, io.LimitReader(body, MaxAssetSize+1))
	if err != nil {
		return File{}, fmt.Errorf("download %s: %w", name, err)
	}
	if n > MaxAssetSize {
		return File{}, fmt.Errorf("download %s: %w", name, ErrAssetTooLarge)
	}
	if subtle.ConstantTimeCompare(h.Sum(nil), want) != 1 {
		return File{}, fmt.Errorf("%s: %w", name, ErrChecksumMismatch)
	}
	if err := tmp.Sync(); err != nil {
		return File{}, fmt.Errorf("sync download file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return File{}, fmt.Errorf("close download file: %w", err)
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(tmpPath, final); err != nil {
		return File{}, fmt.Errorf("save %s: %w", name, err)
	}
	ok = true
	return File{Path: final, SHA256: want}, nil
}

// checksum returns the SHA-256 listed for name in the checksums file at rawURL.
func (c *Checker) checksum(ctx context.Context, rawURL, name, ua string) ([]byte, error) {
	body, _, err := c.get(ctx, c.client(), rawURL, ua)
	if err != nil {
		return nil, fmt.Errorf("fetch checksums: %w", err)
	}
	defer func() { _ = body.Close() }()
	sum, err := findChecksum(io.LimitReader(body, maxChecksums), name)
	if err != nil {
		return nil, fmt.Errorf("read checksums: %w", err)
	}
	return sum, nil
}

// findChecksum parses sha256sum output ("<hex>  <name>", or "<hex> *<name>" in
// binary mode) and returns the digest of name. It returns ErrNoChecksum when name is
// not listed with a valid SHA-256.
func findChecksum(r io.Reader, name string) ([]byte, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("%w: malformed digest for %s", ErrNoChecksum, name)
		}
		return sum, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s is not listed", ErrNoChecksum, name)
}

// get sends a GET request and returns the body of a 200 response and its length,
// or -1 when the length is unknown.
func (c *Checker) get(ctx context.Context, client *http.Client, rawURL, ua string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("%w: %s", ErrUnexpectedStatus, resp.Status)
	}
	return resp.Body, resp.ContentLength, nil
}

// progressWriter passes writes to w and reports the running byte count.
type progressWriter struct {
	w      io.Writer
	done   int64
	total  int64
	report Progress
}

// Write writes b to the underlying writer and reports the progress.
func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	p.report(p.done, p.total)
	return n, err
}
