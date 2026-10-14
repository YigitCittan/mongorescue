package vaultcreds

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// S3Keys is the access key pair of an S3 storage target.
type S3Keys struct {
	// AccessKeyID, SecretAccessKey and SessionToken (optional) sign requests.
	AccessKeyID, SecretAccessKey, SessionToken string
	// Expires is when the keys must be read again: the end of the static cache
	// TTL, or the lease's expiry less its safety margin.
	Expires time.Time
}

// S3 returns the access keys ref names. Static kv-v2 keys come from the cache
// while fresh. Every call with the AWS engine issues new keys whose lease is
// left to expire (or revoked by Close): the driver's credentials cache asks for
// new ones before they end, so a long upload never signs with expired keys.
func (r *Resolver) S3(ctx context.Context, ref models.VaultRef) (S3Keys, error) {
	if err := r.ready(); err != nil {
		return S3Keys{}, err
	}
	switch ref.Engine {
	case models.VaultKV2:
		vals, err := r.kv(ctx, ref, []string{ref.AccessKeyField, ref.SecretKeyField, ref.SessionTokenField})
		if err != nil {
			return S3Keys{}, r.failed(err)
		}
		r.succeeded()
		return S3Keys{AccessKeyID: vals[ref.AccessKeyField], SecretAccessKey: vals[ref.SecretKeyField], SessionToken: vals[ref.SessionTokenField],
			Expires: r.now().Add(r.cacheTTL)}, nil
	case models.VaultAWS:
		l, err := r.issue(ctx, ref)
		if err != nil {
			return S3Keys{}, r.failed(err)
		}
		r.succeeded()
		expires := l.expires.Add(-l.margin())
		if l.ttl0 <= 0 {
			expires = r.now().Add(r.cacheTTL)
		}
		r.mu.Lock()
		now := r.now()
		for id, exp := range r.s3Leases {
			if !exp.After(now) {
				delete(r.s3Leases, id)
			}
		}
		closed := r.closed
		if !closed && l.id != "" {
			r.s3Leases[l.id] = l.expires
		}
		r.mu.Unlock()
		if closed {
			r.revokeAll([]string{l.id})
			return S3Keys{}, fmt.Errorf("%w: MongoRescue is shutting down", ErrLeaseLost)
		}
		return S3Keys{AccessKeyID: l.values["access_key"], SecretAccessKey: l.values["secret_key"], SessionToken: l.values["security_token"],
			Expires: expires}, nil
	}
	return S3Keys{}, fmt.Errorf("%w: engine %q does not supply S3 access keys", models.ErrInvalidVaultRef, ref.Engine)
}

// S3Provider returns an AWS credentials provider reading ref, cached until the
// keys must be read again (see S3).
func (r *Resolver) S3Provider(ref models.VaultRef) aws.CredentialsProvider {
	return aws.NewCredentialsCache(s3Provider{r: r, ref: ref})
}

// s3Provider adapts Resolver.S3 to aws.CredentialsProvider.
type s3Provider struct {
	r   *Resolver
	ref models.VaultRef
}

// Retrieve implements aws.CredentialsProvider.
func (p s3Provider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	k, err := p.r.S3(ctx, p.ref)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("access keys from Vault: %w", err)
	}
	return aws.Credentials{AccessKeyID: k.AccessKeyID, SecretAccessKey: k.SecretAccessKey, SessionToken: k.SessionToken,
		Source: "vault", CanExpire: true, Expires: k.Expires}, nil
}
