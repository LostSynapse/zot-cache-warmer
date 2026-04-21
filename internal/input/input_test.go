package input

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCollect_SingleImage(t *testing.T) {
	r, err := Collect("ghcr.io/foo/bar:v1.2.3", false)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if r.Source != SourceImage {
		t.Errorf("Source = %q, want image", r.Source)
	}
	if !reflect.DeepEqual(r.Images, []string{"ghcr.io/foo/bar:v1.2.3"}) {
		t.Errorf("Images = %v", r.Images)
	}
}

func TestCollect_AutodetectsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "images.txt")
	if err := os.WriteFile(path, []byte("alpine:3.20\nbusybox:latest\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := Collect(path, false)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if r.Source != SourceFile {
		t.Errorf("Source = %q, want file", r.Source)
	}
	want := []string{"alpine:3.20", "busybox:latest"}
	if !reflect.DeepEqual(r.Images, want) {
		t.Errorf("Images = %v, want %v", r.Images, want)
	}
}

func TestCollect_ForceFile_NonexistentPath(t *testing.T) {
	r, err := Collect("/no/such/file.txt", true)
	if err == nil {
		t.Fatalf("expected error opening nonexistent file, got %v", r)
	}
}

func TestCollect_EmptyArg(t *testing.T) {
	_, err := Collect("", false)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParseReader_YAMLManifest(t *testing.T) {
	yaml := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  template:
    spec:
      initContainers:
        - name: init
          image: busybox:1.36
      containers:
        - name: app
          image: ghcr.io/myorg/myapp:v1.2.3
          imagePullPolicy: IfNotPresent
        - name: sidecar
          image: "docker.io/library/nginx:1.25"
`
	got, err := parseReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{
		"busybox:1.36",
		"docker.io/library/nginx:1.25",
		"ghcr.io/myorg/myapp:v1.2.3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseReader_JSONManifest(t *testing.T) {
	j := `{
  "spec": {
    "containers": [
      {"name": "a", "image": "ghcr.io/foo/bar:v1"},
      {"name": "b", "image": "quay.io/baz/qux:v2"}
    ]
  }
}`
	got, err := parseReader(strings.NewReader(j))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"ghcr.io/foo/bar:v1", "quay.io/baz/qux:v2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseReader_PlainText(t *testing.T) {
	txt := `# comment
alpine:3.20

busybox:latest
# trailing comment
ghcr.io/foo/bar:v1.2.3
`
	got, err := parseReader(strings.NewReader(txt))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"alpine:3.20", "busybox:latest", "ghcr.io/foo/bar:v1.2.3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseReader_Dedup(t *testing.T) {
	txt := `alpine:3.20
alpine:3.20
alpine:3.20
`
	got, err := parseReader(strings.NewReader(txt))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0] != "alpine:3.20" {
		t.Errorf("dedup failed: %v", got)
	}
}

func TestParseReader_IgnoresPullPolicy(t *testing.T) {
	yaml := `containers:
  - name: app
    image: nginx:1.25
    imagePullPolicy: IfNotPresent
`
	got, err := parseReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"nginx:1.25"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLooksLikeImageRef(t *testing.T) {
	cases := map[string]bool{
		"alpine:3.20":                  true,
		"ghcr.io/foo/bar:v1":           true,
		"foo@sha256:abc":               true,
		"library/postgres":             true,
		"":                             false,
		"true":                         false,
		"false":                        false,
		"null":                         false,
		"IfNotPresent":                 false,
		"Always":                       false,
		"plain-text":                   false,
		"12345":                        false,
	}
	for in, want := range cases {
		if got := looksLikeImageRef(in); got != want {
			t.Errorf("looksLikeImageRef(%q) = %v, want %v", in, got, want)
		}
	}
}
