// Package registry is the Zot-facing HTTP client. It wraps
// github.com/google/go-containerregistry to perform HEAD (cache-presence
// probe) and GET (pull-through trigger) on manifests.
//
// The core algorithm is: HEAD to detect, GET to warm, recurse on index
// children. An OCI index (or Docker manifest list) describes per-platform
// manifests; warming only the index leaves platform-specific content
// uncached unless Zot's prefetch config pulls it eagerly. Explicitly warming
// each child by digest guarantees multi-arch coverage.
package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Manifest media types in Accept-header preference order. OCI first lets Zot
// convert Docker manifest-list → OCI index when upstream served Docker form,
// simplifying downstream parsing.
const (
	MediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerListV2   = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// TransportConfig controls the HTTP transport used to talk to Zot. The zero
// value is usable; DefaultTransportConfig supplies sensible production values.
type TransportConfig struct {
	// InsecureSkipVerify disables TLS certificate verification. Intended only
	// for self-signed dev registries.
	InsecureSkipVerify bool

	// CAFile optionally augments the system cert pool with an internal CA in
	// PEM format. Pool augmentation (not replacement) so publicly-signed
	// certificates continue to validate.
	CAFile string

	TLSHandshakeTimeout time.Duration
	DialTimeout         time.Duration
	IdleConnTimeout     time.Duration

	// MaxIdleConnsPerHost defaults to Go's 2 in http.DefaultTransport. That
	// value causes SYN floods against a single Zot host when warming many
	// images; 64 is a sane production value.
	MaxIdleConnsPerHost int
}

// DefaultTransportConfig returns a production-sane TransportConfig.
func DefaultTransportConfig() TransportConfig {
	return TransportConfig{
		TLSHandshakeTimeout: 10 * time.Second,
		DialTimeout:         5 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 64,
	}
}

// NewTransport builds an http.Transport with the supplied configuration. The
// system cert pool is augmented (never replaced) with the optional CAFile.
// This works in gcr.io/distroless/static-debian12 because the image ships
// /etc/ssl/certs/ca-certificates.crt.
func NewTransport(cfg TransportConfig) (*http.Transport, error) {
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file %q: %w", cfg.CAFile, err)
		}
		if !rootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certs parsed from %q", cfg.CAFile)
		}
	}

	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.TLSHandshakeTimeout == 0 {
		cfg.TLSHandshakeTimeout = 10 * time.Second
	}
	if cfg.IdleConnTimeout == 0 {
		cfg.IdleConnTimeout = 90 * time.Second
	}
	if cfg.MaxIdleConnsPerHost == 0 {
		cfg.MaxIdleConnsPerHost = 64
	}

	dialer := &net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			RootCAs:            rootCAs,
			InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // opt-in flag
		},
	}, nil
}

// Warmer is the thin Zot-facing client. It resolves references against a
// fixed zotHost, adding name.Insecure when the caller's Zot endpoint is plain
// HTTP. It does NOT perform reference rewriting for Zot's multi-upstream
// sync.content configuration — that mapping must be done by the caller.
type Warmer struct {
	baseOpts []remote.Option
	nameOpts []name.Option
	zotHost  string
}

// NewWarmer constructs a Warmer.
//
//   - zotHost is the registry host:port (no scheme).
//   - httpClient supplies the transport used for all HTTP operations.
//   - user/pass are HTTP Basic credentials; empty user ⇒ anonymous.
//   - insecure uses HTTP instead of HTTPS (maps to name.Insecure).
//   - userAgent identifies the warmer in Zot access logs.
func NewWarmer(
	zotHost string,
	httpClient *http.Client,
	user, pass string,
	insecure bool,
	userAgent string,
) (*Warmer, error) {
	if zotHost == "" {
		return nil, errors.New("registry.NewWarmer: zotHost is required")
	}
	if httpClient == nil || httpClient.Transport == nil {
		return nil, errors.New("registry.NewWarmer: httpClient with Transport is required")
	}

	var auth authn.Authenticator = authn.Anonymous
	if user != "" {
		auth = &authn.Basic{Username: user, Password: pass}
	}

	baseOpts := []remote.Option{
		remote.WithAuth(auth),
		remote.WithTransport(httpClient.Transport),
		// remote.WithRetryBackoff honors Retry-After on 429 responses.
		// DO NOT also wrap with hashicorp/go-retryablehttp — it produces
		// multiplicative retries and amplifies throttling.
		remote.WithRetryBackoff(remote.Backoff{
			Duration: 200 * time.Millisecond,
			Factor:   2.0,
			Jitter:   0.1,
			Steps:    5,
			Cap:      10 * time.Second,
		}),
	}
	if userAgent != "" {
		baseOpts = append(baseOpts, remote.WithUserAgent(userAgent))
	}

	nameOpts := []name.Option{name.WithDefaultRegistry(zotHost)}
	if insecure {
		nameOpts = append(nameOpts, name.Insecure)
	}

	return &Warmer{
		baseOpts: baseOpts,
		nameOpts: nameOpts,
		zotHost:  zotHost,
	}, nil
}

// IsCached issues HEAD /v2/<repo>/manifests/<tag-or-digest>. A 200 proves the
// manifest is already present in Zot; a 404 means Zot has not fetched it from
// upstream yet and WarmMultiArch should be called to populate the cache.
func (w *Warmer) IsCached(ctx context.Context, ref string) (*v1.Descriptor, bool, error) {
	r, err := name.ParseReference(ref, w.nameOpts...)
	if err != nil {
		return nil, false, fmt.Errorf("parse ref %q: %w", ref, err)
	}
	desc, err := remote.Head(r, append(w.baseOpts, remote.WithContext(ctx))...)
	if err != nil {
		var te *transport.Error
		if errors.As(err, &te) && te.StatusCode == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("HEAD %s: %w", ref, err)
	}
	return desc, true, nil
}

// Warm issues GET /v2/<repo>/manifests/<tag-or-digest>. Zot treats GET as the
// signal to fetch upstream and blocks until the manifest is cached locally.
// For manifest lists / OCI indexes the returned Descriptor lets the caller
// recurse into per-platform children.
func (w *Warmer) Warm(ctx context.Context, ref string) (*remote.Descriptor, error) {
	r, err := name.ParseReference(ref, w.nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("parse ref %q: %w", ref, err)
	}
	desc, err := remote.Get(r, append(w.baseOpts, remote.WithContext(ctx))...)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", ref, err)
	}
	return desc, nil
}

// WarmMultiArch warms the top-level manifest and, if it is an index, every
// per-platform child manifest addressed by digest. Addressing children by
// digest (rather than tag@digest) avoids Zot issue #2584 and eliminates
// tag-moved races.
func (w *Warmer) WarmMultiArch(ctx context.Context, ref string) error {
	desc, err := w.Warm(ctx, ref)
	if err != nil {
		return err
	}
	switch string(desc.MediaType) {
	case MediaTypeOCIIndex, MediaTypeDockerListV2:
		idx, err := desc.ImageIndex()
		if err != nil {
			return fmt.Errorf("parse index for %s: %w", ref, err)
		}
		manifest, err := idx.IndexManifest()
		if err != nil {
			return fmt.Errorf("index manifest for %s: %w", ref, err)
		}
		base := strippedBase(ref)
		for _, m := range manifest.Manifests {
			child := fmt.Sprintf("%s@%s", base, m.Digest.String())
			if _, err := w.Warm(ctx, child); err != nil {
				return fmt.Errorf("warm child %s: %w", child, err)
			}
		}
	}
	return nil
}

// strippedBase returns the repo portion of ref without any :tag or @digest
// suffix. It guards against false positives on ':' in host:port by requiring
// that nothing after the colon contains a '/'.
func strippedBase(ref string) string {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '@' {
			return ref[:i]
		}
		if ref[i] == ':' {
			rest := ref[i+1:]
			for _, c := range rest {
				if c == '/' {
					return ref
				}
			}
			return ref[:i]
		}
	}
	return ref
}
