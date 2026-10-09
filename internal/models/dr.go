package models

import (
	"net/url"
	"strings"
)

// MaxRegionLength is the longest region label of a storage target.
const MaxRegionLength = 64

// DRRegion returns the region (failure domain) of t for the disaster recovery
// checks, lower-cased: Region when set, else the region of an S3 target unless it
// is "auto" (Cloudflare R2 picks one), else "" (unknown).
func (t *StorageTarget) DRRegion() string {
	if t == nil {
		return ""
	}
	if r := strings.ToLower(strings.TrimSpace(t.Region)); r != "" {
		return r
	}
	if t.Type == StorageS3 && t.S3 != nil {
		if r := strings.ToLower(strings.TrimSpace(t.S3.Region)); r != "auto" {
			return r
		}
	}
	return ""
}

// CrossRegion reports whether copy target cp and primary are in known, different
// regions (see DRRegion). Unknown regions never count as different.
func CrossRegion(primary, cp *StorageTarget) bool {
	a, b := primary.DRRegion(), cp.DRRegion()
	return a != "" && b != "" && a != b
}

// SameCredentials reports whether one credential or account detectably reaches
// both a and b, so a single leaked key can delete from both: two local targets
// (one host, one operating system user), two S3 targets with the same access key
// ID, two S3 targets on the same endpoint without keys (both use the process's
// default credentials), or two S3 targets on the same endpoint whose host names
// the account (Cloudflare R2). Anything else, including an unknown account, is not
// reported.
func SameCredentials(a, b *StorageTarget) bool {
	if a == nil || b == nil || a.Type != b.Type {
		return false
	}
	switch a.Type {
	case StorageLocal:
		return true
	case StorageS3:
		if a.S3 == nil || b.S3 == nil {
			return false
		}
		ka, kb := strings.TrimSpace(a.S3.AccessKeyID), strings.TrimSpace(b.S3.AccessKeyID)
		if ka != "" && ka == kb {
			return true
		}
		ha, hb := endpointHost(a.S3.Endpoint), endpointHost(b.S3.Endpoint)
		if ha != hb {
			return false
		}
		if ka == "" && kb == "" {
			return true
		}
		return accountOf(ha) != ""
	}
	return false
}

// endpointHost returns the lower-cased host of an S3 endpoint ("" for AWS S3).
func endpointHost(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return strings.ToLower(endpoint)
	}
	return strings.ToLower(u.Hostname())
}

// accountOf returns the account an S3 endpoint host names, or "" when it names
// none: "<account>.r2.cloudflarestorage.com", also with a jurisdiction such as
// "<account>.eu.r2.cloudflarestorage.com".
func accountOf(host string) string {
	acct, rest, ok := strings.Cut(host, ".")
	if !ok || acct == "" {
		return ""
	}
	if rest == "r2.cloudflarestorage.com" || strings.HasSuffix(rest, ".r2.cloudflarestorage.com") {
		return acct
	}
	return ""
}
