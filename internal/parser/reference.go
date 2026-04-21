// Package parser normalizes raw Kubernetes container image references for the
// Zot pull-through cache warmer.
//
// Implements the workaround for project-zot/zot#2584 by splitting
// "repo:tag@digest" inputs into a tag-only pull reference (used for the actual
// Zot manifest request) and a canonical full reference (preserved for logging).
package parser

import (
	_ "crypto/sha256" // REQUIRED: registers sha256 with opencontainers/go-digest
	_ "crypto/sha512" // optional: accept sha512-pinned references

	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/distribution/reference"
)

// maxRefLen caps input size defensively. Real refs top out ≈ 780 bytes with
// sha512; 1024 is a safety ceiling against hostile input.
const maxRefLen = 1024

// Sentinel errors for classification by the cache-warmer log pipeline.
// These are produced by pre-validation, before the library parser runs.
var (
	ErrEmpty       = errors.New("parser: empty or whitespace-only reference")
	ErrTooLong     = errors.New("parser: reference exceeds 1024 bytes")
	ErrNotUTF8     = errors.New("parser: reference is not valid UTF-8")
	ErrControlChar = errors.New("parser: reference contains control characters")
)

// Parsed is the normalized view used by the cache warmer. All fields are
// populated on success; on error Parse returns a zero-valued Parsed.
type Parsed struct {
	// Raw is the original pre-trim input, preserved verbatim for diagnostics.
	Raw string

	// Canonical is the full normalized form — e.g. "docker.io/library/nginx:1.25@sha256:..."
	// Use this for logging so operators see exactly what the Pod spec contains.
	Canonical string

	// PullRef is the form to pass to Zot. Tag-only when HadBothTagAndDigest is
	// true (the #2584 workaround), otherwise identical to Canonical.
	PullRef string

	// Registry is the OCI domain — e.g. "docker.io", "registry.k8s.io",
	// "localhost:5000". Useful for downstream Zot sync-config mapping.
	Registry string

	// Repository is the path component — e.g. "library/nginx". This is the
	// Zot-local path when Zot is configured without prefix rewriting.
	Repository string

	// Tag is the tag component or "" when digest-only.
	Tag string

	// Digest is the "sha256:..." digest string or "".
	Digest string

	// HadBothTagAndDigest indicates the original ref used the "repo:tag@digest"
	// form. Informational — the Zot #2584 workaround is already applied via
	// PullRef.
	HadBothTagAndDigest bool

	// IsDigestOnly indicates the ref pinned a digest without a tag.
	// PullRef uses the digest directly in this case.
	IsDigestOnly bool
}

// Parse normalizes a raw Kubernetes image reference. On any error the returned
// Parsed is zero-valued and err can be classified via ClassifyError or
// errors.Is against the Err* sentinels and distribution/reference's error values.
func Parse(raw string) (Parsed, error) {
	trimmed, err := preValidate(raw)
	if err != nil {
		return Parsed{}, err
	}

	// ParseNormalizedNamed handles 99% of Kubernetes inputs: expands "nginx"
	// to "docker.io/library/nginx", rewrites "index.docker.io" → "docker.io",
	// and rejects digest-only inputs (which are separately handled below via
	// reference.Parse if we ever extend this).
	named, err := reference.ParseNormalizedNamed(trimmed)
	if err != nil {
		return Parsed{}, fmt.Errorf("parser: %w", err)
	}

	// Inject :latest only when there is neither tag nor digest.
	if reference.IsNameOnly(named) {
		named = reference.TagNameOnly(named)
	}

	// Canonicalize Docker Hub hostname aliases. distribution/reference
	// preserves whatever domain string the caller wrote; for registry-
	// routing purposes, "docker.io", "registry-1.docker.io", and
	// "index.docker.io" all designate the same registry. Collapse them to
	// the canonical short name so downstream consumers (ZOT_REGISTRY_MAP
	// lookups, dedup by Registry) see a single identity.
	registry := reference.Domain(named)
	switch registry {
	case "registry-1.docker.io", "index.docker.io":
		registry = "docker.io"
	}

	out := Parsed{
		Raw:        raw,
		Canonical:  named.String(),
		Registry:   registry,
		Repository: reference.Path(named),
	}

	tagged, hasTag := named.(reference.Tagged)
	digested, hasDigest := named.(reference.Digested)
	if hasTag {
		out.Tag = tagged.Tag()
	}
	if hasDigest {
		out.Digest = digested.Digest().String()
	}

	switch {
	case hasTag && hasDigest:
		// repo:tag@digest — Zot #2584 workaround: strip digest, pull by tag.
		out.HadBothTagAndDigest = true
		bare := reference.TrimNamed(named)
		withTag, wErr := reference.WithTag(bare, tagged.Tag())
		if wErr != nil {
			return Parsed{}, fmt.Errorf("parser: rebuild tag-only ref: %w", wErr)
		}
		out.PullRef = withTag.String()

	case hasDigest:
		// repo@digest — pull by digest directly. Zot #2584 only affects the
		// combined form; digest-only works correctly.
		out.IsDigestOnly = true
		out.PullRef = named.String()

	default:
		// repo:tag — plain case.
		out.PullRef = named.String()
	}

	return out, nil
}

// preValidate applies defensive checks that short-circuit the library parser
// on hostile or malformed input before any allocation-heavy work.
func preValidate(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", ErrEmpty
	}
	if len(t) > maxRefLen {
		return "", ErrTooLong
	}
	if !utf8.ValidString(t) {
		return "", ErrNotUTF8
	}
	for _, r := range t {
		if r < 0x20 || r == 0x7f {
			return "", ErrControlChar
		}
	}
	return t, nil
}

// ClassifyError returns a short category string for metrics labels and
// structured log fields. Every known error category maps to a stable label;
// unknown errors become "invalid_format".
func ClassifyError(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, ErrEmpty):
		return "empty"
	case errors.Is(err, ErrTooLong):
		return "too_long"
	case errors.Is(err, ErrNotUTF8):
		return "non_utf8"
	case errors.Is(err, ErrControlChar):
		return "control_chars"
	case errors.Is(err, reference.ErrNameContainsUppercase):
		return "uppercase"
	case errors.Is(err, reference.ErrTagInvalidFormat):
		return "bad_tag"
	case errors.Is(err, reference.ErrDigestInvalidFormat):
		return "bad_digest"
	case errors.Is(err, reference.ErrNameEmpty):
		return "empty"
	case errors.Is(err, reference.ErrNameTooLong):
		return "too_long"
	case errors.Is(err, reference.ErrReferenceInvalidFormat):
		return "invalid_format"
	default:
		return "invalid_format"
	}
}

// Sanitize trims a reference string for safe inclusion in log lines,
// replacing control characters and capping length.
func Sanitize(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if len(s) > 256 {
		s = s[:256] + "…"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}
