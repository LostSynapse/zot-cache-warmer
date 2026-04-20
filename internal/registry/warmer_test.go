package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// zotMock returns a minimal httptest.Server that mimics enough Zot behavior to
// exercise the warmer's HEAD and GET paths. The caller supplies whether the
// manifest is "cached" — on cache-miss the server counts GET calls to verify
// the warmer triggered the pull-through.
type zotMock struct {
	srv      *httptest.Server
	cached   bool
	getCount int32
}

func newZotMock(t *testing.T, cached bool) *zotMock {
	t.Helper()
	m := &zotMock{cached: cached}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			// Every ggcr operation begins with this probe.
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/manifests/"):
			if !m.cached {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", MediaTypeOCIManifest)
			w.Header().Set("Docker-Content-Digest",
				"sha256:deadbeef0000000000000000000000000000000000000000000000000000beef")
			w.Header().Set("Content-Length", "527")
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/"):
			atomic.AddInt32(&m.getCount, 1)
			// Minimal OCI manifest body. After the first GET the server
			// flips to "cached" so follow-up HEADs succeed.
			m.cached = true
			body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":2},"layers":[]}`)
			w.Header().Set("Content-Type", MediaTypeOCIManifest)
			w.Header().Set("Docker-Content-Digest",
				"sha256:deadbeef0000000000000000000000000000000000000000000000000000beef")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// host returns the host:port portion of the test server URL.
func (m *zotMock) host() string {
	return strings.TrimPrefix(m.srv.URL, "http://")
}

// newWarmer constructs a Warmer wired to the mock server. Uses insecure=true
// because httptest serves plain HTTP.
func newWarmer(t *testing.T, m *zotMock) *Warmer {
	t.Helper()
	w, err := NewWarmer(
		m.host(),
		m.srv.Client(),
		"", "",
		true,
		"test-agent/1.0",
	)
	if err != nil {
		t.Fatalf("NewWarmer: %v", err)
	}
	return w
}

func TestIsCached_Hit(t *testing.T) {
	m := newZotMock(t, true)
	w := newWarmer(t, m)

	desc, ok, err := w.IsCached(context.Background(), "library/alpine:3.20")
	if err != nil {
		t.Fatalf("IsCached: %v", err)
	}
	if !ok {
		t.Fatalf("expected cached=true, got false")
	}
	if desc == nil {
		t.Fatalf("expected non-nil descriptor")
	}
}

func TestIsCached_Miss(t *testing.T) {
	m := newZotMock(t, false)
	w := newWarmer(t, m)

	desc, ok, err := w.IsCached(context.Background(), "library/alpine:3.20")
	if err != nil {
		t.Fatalf("IsCached: %v", err)
	}
	if ok {
		t.Fatalf("expected cached=false on 404")
	}
	if desc != nil {
		t.Fatalf("expected nil descriptor on miss")
	}
}

func TestWarm_TriggersGet(t *testing.T) {
	m := newZotMock(t, false)
	w := newWarmer(t, m)

	if _, err := w.Warm(context.Background(), "library/alpine:3.20"); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if got := atomic.LoadInt32(&m.getCount); got != 1 {
		t.Errorf("expected 1 GET, got %d", got)
	}
}

func TestNewWarmer_RequiresHost(t *testing.T) {
	_, err := NewWarmer("", &http.Client{Transport: http.DefaultTransport}, "", "", false, "")
	if err == nil {
		t.Fatal("expected error for empty zotHost")
	}
}

func TestNewWarmer_RequiresTransport(t *testing.T) {
	_, err := NewWarmer("zot.example.com:5000", nil, "", "", false, "")
	if err == nil {
		t.Fatal("expected error for nil httpClient")
	}
}

func TestNewTransport_Defaults(t *testing.T) {
	tr, err := NewTransport(TransportConfig{})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if tr.MaxIdleConnsPerHost != 64 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 64", tr.MaxIdleConnsPerHost)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("expected non-nil TLSClientConfig")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must default to false")
	}
}

func TestNewTransport_InsecureFlagPropagates(t *testing.T) {
	tr, err := NewTransport(TransportConfig{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify=true flag was not propagated to TLS config")
	}
}

func TestStrippedBase(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"registry.example.com/repo:tag", "registry.example.com/repo"},
		{"registry.example.com/repo@sha256:abc", "registry.example.com/repo"},
		{"registry.example.com:5000/repo:tag", "registry.example.com:5000/repo"},
		{"registry.example.com:5000/repo", "registry.example.com:5000/repo"},
		{"repo", "repo"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := strippedBase(tc.in); got != tc.want {
				t.Errorf("strippedBase(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
