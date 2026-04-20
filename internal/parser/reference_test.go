package parser

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name             string
		input            string
		wantErr          bool
		wantRegistry     string
		wantRepo         string
		wantTag          string
		wantDigest       string
		wantPullRef      string
		wantHadBoth      bool
		wantIsDigestOnly bool
	}{
		// --- Docker Hub normalization ---
		{"hub-short", "ubuntu", false, "docker.io", "library/ubuntu", "latest", "", "docker.io/library/ubuntu:latest", false, false},
		{"hub-tag", "ubuntu:22.04", false, "docker.io", "library/ubuntu", "22.04", "", "docker.io/library/ubuntu:22.04", false, false},
		{"hub-user", "bitnami/nginx:1.25.0", false, "docker.io", "bitnami/nginx", "1.25.0", "", "docker.io/bitnami/nginx:1.25.0", false, false},
		{"hub-legacy-index", "index.docker.io/redis", false, "docker.io", "library/redis", "latest", "", "docker.io/library/redis:latest", false, false},

		// --- Other registries ---
		{"k8s-current", "registry.k8s.io/pause:3.9", false, "registry.k8s.io", "pause", "3.9", "", "registry.k8s.io/pause:3.9", false, false},
		{"k8s-legacy", "k8s.gcr.io/pause:3.9", false, "k8s.gcr.io", "pause", "3.9", "", "k8s.gcr.io/pause:3.9", false, false},
		{"gar-deep", "us-docker.pkg.dev/p/r/app:v1", false, "us-docker.pkg.dev", "p/r/app", "v1", "", "us-docker.pkg.dev/p/r/app:v1", false, false},
		{"quay", "quay.io/prometheus/node-exporter:v1.8.0", false, "quay.io", "prometheus/node-exporter", "v1.8.0", "", "quay.io/prometheus/node-exporter:v1.8.0", false, false},

		// --- Private registries ---
		{"localhost-port", "localhost:5000/app:v1", false, "localhost:5000", "app", "v1", "", "localhost:5000/app:v1", false, false},
		{"ipv4-port", "10.0.0.1:5000/app:v1", false, "10.0.0.1:5000", "app", "v1", "", "10.0.0.1:5000/app:v1", false, false},

		// --- Digest handling (the whole point) ---
		{
			name:         "tag-and-digest",
			input:        "nginx:1.25.0@sha256:aaaabbbbccccdddd0000111122223333444455556666777788889999aaaabbbb",
			wantRegistry: "docker.io",
			wantRepo:     "library/nginx",
			wantTag:      "1.25.0",
			wantDigest:   "sha256:aaaabbbbccccdddd0000111122223333444455556666777788889999aaaabbbb",
			// digest stripped for Zot #2584:
			wantPullRef: "docker.io/library/nginx:1.25.0",
			wantHadBoth: true,
		},
		{
			name:             "digest-only",
			input:            "nginx@sha256:aaaabbbbccccdddd0000111122223333444455556666777788889999aaaabbbb",
			wantRegistry:     "docker.io",
			wantRepo:         "library/nginx",
			wantDigest:       "sha256:aaaabbbbccccdddd0000111122223333444455556666777788889999aaaabbbb",
			wantPullRef:      "docker.io/library/nginx@sha256:aaaabbbbccccdddd0000111122223333444455556666777788889999aaaabbbb",
			wantIsDigestOnly: true,
		},

		// --- Error cases ---
		{"empty", "", true, "", "", "", "", "", false, false},
		{"whitespace", "   ", true, "", "", "", "", "", false, false},
		// Uppercase must appear in the PATH to trigger ErrNameContainsUppercase.
		// "UPPERCASE/repo" would actually parse (UPPERCASE treated as the
		// registry domain since it has uppercase → triggers the domain branch
		// in splitDockerDomain); the path "repo" is valid lowercase.
		// "foo/BAR" forces the path branch and fails on the uppercase check.
		{"uppercase-in-path", "foo/BAR", true, "", "", "", "", "", false, false},
		{"bad-colon", "repo:tag:tag", true, "", "", "", "", "", false, false},
		{"missing-name", ":tag", true, "", "", "", "", "", false, false},
		{"bad-digest", "repo@notadigest", true, "", "", "", "", "", false, false},
		{"control-char", "nginx\x00evil", true, "", "", "", "", "", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.Registry != tc.wantRegistry {
				t.Errorf("Registry = %q, want %q", got.Registry, tc.wantRegistry)
			}
			if got.Repository != tc.wantRepo {
				t.Errorf("Repository = %q, want %q", got.Repository, tc.wantRepo)
			}
			if got.Tag != tc.wantTag {
				t.Errorf("Tag = %q, want %q", got.Tag, tc.wantTag)
			}
			if got.Digest != tc.wantDigest {
				t.Errorf("Digest = %q, want %q", got.Digest, tc.wantDigest)
			}
			if got.PullRef != tc.wantPullRef {
				t.Errorf("PullRef = %q, want %q", got.PullRef, tc.wantPullRef)
			}
			if got.HadBothTagAndDigest != tc.wantHadBoth {
				t.Errorf("HadBothTagAndDigest = %v, want %v", got.HadBothTagAndDigest, tc.wantHadBoth)
			}
			if got.IsDigestOnly != tc.wantIsDigestOnly {
				t.Errorf("IsDigestOnly = %v, want %v", got.IsDigestOnly, tc.wantIsDigestOnly)
			}
		})
	}
}

func TestPreValidate(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{"empty", "", ErrEmpty},
		{"whitespace-only", "   \t\n", ErrEmpty},
		{"control-null", "nginx\x00", ErrControlChar},
		{"control-del", "nginx\x7f", ErrControlChar},
		{"too-long", string(make([]byte, maxRefLen+1)), ErrTooLong},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// too-long input is all zero bytes — that's also control-char input,
			// but ErrTooLong is checked first.
			_, err := preValidate(tc.input)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("preValidate(%q) err = %v, want %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want string
	}{
		{"nil", nil, "none"},
		{"empty", ErrEmpty, "empty"},
		{"too-long", ErrTooLong, "too_long"},
		{"non-utf8", ErrNotUTF8, "non_utf8"},
		{"control", ErrControlChar, "control_chars"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.in); got != tc.want {
				t.Errorf("ClassifyError(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
