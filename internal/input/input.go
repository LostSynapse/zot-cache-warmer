// Package input collects image references from the diverse input sources
// the standalone CLI accepts: a single image as an argument, a file path
// containing YAML/JSON/plain-text image references, or standard input.
//
// The core challenge is auto-detection: when the user runs
//
//	zot-warm something
//
// "something" might be a file on disk, an image reference, or a typo. The
// rules are deterministic: if the argument exists as a regular file, it is
// treated as a file; if it equals "-", it is treated as stdin; otherwise it
// is treated as an image reference. Use the explicit --file flag to force
// file interpretation (e.g. for paths that don't yet exist on disk).
package input

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Source identifies how an image list was assembled, for logging.
type Source string

const (
	SourceImage  Source = "image"   // single image arg
	SourceFile   Source = "file"    // file on disk
	SourceStdin  Source = "stdin"   // "-" or piped
)

// Result is the output of Collect.
type Result struct {
	Images []string
	Source Source
}

// Collect resolves the supplied argument into a deduplicated list of image
// references. forceFile, when true, treats arg as a file path even if no
// file exists on disk (useful for clear errors when the path is wrong).
//
// Auto-detect rules:
//   - arg == "-"            → stdin
//   - file exists at arg    → file
//   - otherwise             → single image reference
//
// Returns an error if reading fails or no images are found.
func Collect(arg string, forceFile bool) (*Result, error) {
	if arg == "-" {
		images, err := parseReader(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if len(images) == 0 {
			return nil, fmt.Errorf("no image references found on stdin")
		}
		return &Result{Images: images, Source: SourceStdin}, nil
	}

	if forceFile || isExistingFile(arg) {
		return collectFromFile(arg)
	}

	if arg == "" {
		return nil, fmt.Errorf("no input supplied")
	}
	return &Result{Images: []string{arg}, Source: SourceImage}, nil
}

// CollectFromFile is the explicit-file equivalent of Collect; useful for
// tests and for callers that have already validated the path.
func CollectFromFile(path string) (*Result, error) {
	return collectFromFile(path)
}

func collectFromFile(path string) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()

	images, err := parseReader(f)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("no image references found in %q", path)
	}
	return &Result{Images: images, Source: SourceFile}, nil
}

func isExistingFile(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Mode().IsRegular()
}

// imageFieldRE matches a YAML `image:` or `Image:` field on its own line.
// Captures the value (quoted or unquoted). Tolerates leading whitespace and
// list-item dashes. Does NOT match `imagePullPolicy:` because the key is
// anchored to `image` followed by optional whitespace then `:`.
//
// Anchored to start-of-line only; the trailing character class stops at
// natural YAML terminators (whitespace, newline, quote).
var imageFieldRE = regexp.MustCompile(
	`(?mi)^[ \t]*(?:-[ \t]+)?"?image"?[ \t]*:[ \t]*["']?([^"'\s,}{\[\]]+)["']?`,
)

// parseReader extracts image references from r. The input is classified by
// its first non-blank byte and dispatched to exactly one parser:
//
//   - Starts with `{` or `[`: parsed as JSON; every `image` key's string
//     value across the entire document is collected.
//   - Contains `image:` (per line) or starts with `---`: treated as YAML;
//     the `image:` field regex runs over every line.
//   - Otherwise: treated as plain text; every non-blank, non-comment line
//     is checked with looksLikeImageRef.
//
// Lines starting with # are treated as comments in plain-text mode. Blank
// lines are ignored. Returns deduplicated, sorted output.
func parseReader(r io.Reader) ([]string, error) {
	// Buffer the input once. Kubernetes manifests and images.txt files are
	// tens of KB at most; the 4 MiB cap protects against pathological input.
	buf, err := io.ReadAll(io.LimitReader(r, 4*1024*1024))
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})

	switch detectMode(buf) {
	case modeJSON:
		if err := extractFromJSON(buf, seen); err != nil {
			return nil, err
		}
	case modeYAML:
		extractFromYAML(buf, seen)
	case modePlainText:
		if err := extractFromPlainText(buf, seen); err != nil {
			return nil, err
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

type inputMode int

const (
	modePlainText inputMode = iota
	modeYAML
	modeJSON
)

// detectMode classifies input by the first non-blank byte and a quick scan
// for YAML markers. Priority:
//
//  1. First non-blank byte is `{` or `[` → JSON
//  2. Input contains `image:` or `---` → YAML
//  3. Otherwise → plain text
//
// Mutually exclusive dispatch prevents the old greedy-fallback bug where
// YAML scalar lines (`apiVersion: apps/v1`) were scooped up as image refs.
func detectMode(buf []byte) inputMode {
	for _, b := range buf {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return modeJSON
		default:
			goto notJSON
		}
	}
notJSON:
	if yamlMarkerRE.Match(buf) {
		return modeYAML
	}
	return modePlainText
}

// yamlMarkerRE detects YAML by the presence of an `image:` key on its own
// line or a `---` document separator. An `image:` on some line is a very
// strong YAML signal that distinguishes it from plain text.
var yamlMarkerRE = regexp.MustCompile(`(?mi)^[ \t]*(?:-[ \t]+)?"?image"?[ \t]*:|^---`)

// extractFromJSON walks the parsed document recursively, collecting every
// string value under a key named "image" (case-insensitive).
func extractFromJSON(buf []byte, seen map[string]struct{}) error {
	var doc any
	if err := json.Unmarshal(buf, &doc); err != nil {
		return fmt.Errorf("parse JSON: %w", err)
	}
	walkJSON(doc, seen)
	return nil
}

func walkJSON(v any, seen map[string]struct{}) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if strings.EqualFold(k, "image") {
				if s, ok := val.(string); ok {
					s = strings.TrimSpace(s)
					if s != "" && looksLikeImageRef(s) {
						seen[s] = struct{}{}
					}
				}
			}
			walkJSON(val, seen)
		}
	case []any:
		for _, item := range x {
			walkJSON(item, seen)
		}
	}
}

// extractFromYAML runs the image-field regex over every line of the input.
// YAML's line-oriented structure makes this reliable: keys appear at the
// start of a line (after optional whitespace and list-item dashes), and
// values end at whitespace/newline/quote.
func extractFromYAML(buf []byte, seen map[string]struct{}) {
	for _, match := range imageFieldRE.FindAllSubmatch(buf, -1) {
		ref := strings.TrimSpace(string(match[1]))
		if ref != "" && looksLikeImageRef(ref) {
			seen[ref] = struct{}{}
		}
	}
}

// extractFromPlainText treats every non-blank, non-comment line as a
// candidate image reference.
func extractFromPlainText(buf []byte, seen map[string]struct{}) error {
	scanner := bufio.NewScanner(strings.NewReader(string(buf)))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if looksLikeImageRef(line) {
			seen[line] = struct{}{}
		}
	}
	return scanner.Err()
}

// looksLikeImageRef is a cheap pre-filter to reject obvious non-references
// before the parser package gets involved. We accept anything containing
// "/" or ":" — proper validation happens in the parser package, which gives
// the caller specific error categories.
//
// Rejects values like:
//   - "true" / "false" (YAML booleans)
//   - "Always" / "IfNotPresent" / "Never" (image pull policies — though our
//     regex above already excludes the imagePullPolicy field)
//   - "{}", "[]", "null"
func looksLikeImageRef(s string) bool {
	if s == "" {
		return false
	}
	if !strings.ContainsAny(s, "/:@") {
		return false
	}
	// Exclude obvious non-references. These show up in YAML/JSON contexts
	// where our regex might otherwise match.
	switch strings.ToLower(s) {
	case "true", "false", "null", "{}", "[]":
		return false
	case "always", "ifnotpresent", "never":
		return false
	}
	return true
}
