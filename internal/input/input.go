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

// imageFieldRE matches `image:` and `Image:` fields in YAML/JSON. Captures
// the value (quoted or unquoted, single or double quotes). Tolerates leading
// whitespace and list-item dashes. Does NOT match `imagePullPolicy:` because
// the key is anchored to `image` followed by optional whitespace then `:`,
// and `imagePullPolicy` is a different key.
//
// Key anchoring uses `\bimage\b` semantics via explicit character class
// boundaries: the key is `image` or `"image"` with nothing before it except
// whitespace, a dash (YAML list item), or a quote. This prevents matching
// keys like `imagePullPolicy` whose prefix happens to contain "image".
var imageFieldRE = regexp.MustCompile(
	`(?mi)^[ \t]*(?:-[ \t]+)?"?image"?[ \t]*:[ \t]*["']?([^"'\s,}{\[\]]+)["']?[ \t]*$`,
)

// structuredInputRE detects whether an input looks structured (YAML/JSON).
// If any line matches an `image:` field or begins with `---` / `{` / `[`,
// we treat the whole input as structured and ONLY extract from `image:`
// fields. Otherwise we treat it as line-delimited plain text.
var structuredInputRE = regexp.MustCompile(
	`(?mi)^[ \t]*(?:[-"]?image"?[ \t]*:|---|\{|\[)`,
)

// parseReader extracts image references from r. The input is classified as
// either structured (YAML/JSON) or plain-text, and only one extraction path
// runs. This avoids false positives where structured-file keys like
// `apiVersion: apps/v1` would be misread as image references in a greedy
// fallback path.
//
//   - Structured input: only lines matching `image:` (case-insensitive, with
//     optional YAML list-item dash and quotes) are consumed.
//   - Plain text: every non-blank, non-comment line that looks like an image
//     reference is consumed.
//
// Lines starting with # are treated as comments and skipped. Blank lines are
// ignored. Returns deduplicated, sorted output.
func parseReader(r io.Reader) ([]string, error) {
	// Buffer the entire input so we can make a mode decision before parsing.
	// Inputs are kubernetes manifests or images.txt lists — tens of KB at
	// most — so full buffering is fine. The 4 MiB cap protects against a
	// pathological caller piping gigabytes.
	buf, err := io.ReadAll(io.LimitReader(r, 4*1024*1024))
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	structured := structuredInputRE.Match(buf)

	if structured {
		for _, match := range imageFieldRE.FindAllStringSubmatch(string(buf), -1) {
			ref := strings.TrimSpace(match[1])
			if ref != "" && looksLikeImageRef(ref) {
				seen[ref] = struct{}{}
			}
		}
	} else {
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
		if err := scanner.Err(); err != nil {
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
