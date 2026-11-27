package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// Kind is the class of an argument's or flag's value. The registry checks
// every value against its kind before a command runs: before it resolves
// its caller, builds an Env, reads an @file, or creates a directory, so a
// hostile or hallucinated value fails with invalid_input and no effect.
type Kind string

const (
	KindHandle     Kind = "handle"        // an AMQ handle
	KindRole       Kind = "role"          // a role name, strict
	KindCrew       Kind = "crew"          // a crew name, strict
	KindTarget     Kind = "target"        // a handle or a role (summon)
	KindTopic      Kind = "topic"         // a skill topic
	KindSessionID  Kind = "session_id"    // session:<uuid>
	KindBlockID    Kind = "block_id"      // block:<uuid>
	KindMessageID  Kind = "message_id"    // an amq message id
	KindEventID    Kind = "event_id"      // a producer's event id
	KindThreadID   Kind = "thread_id"     // an amq thread id
	KindOpID       Kind = "op_id"         // an idempotency key, printable
	KindSingleLine Kind = "single_line"   // a subject, title, summary, name
	KindBody       Kind = "body"          // multi-line text, or @FILE, or -
	KindDir        Kind = "dir"           // an existing directory
	KindPath       Kind = "path"          // a path, which may no longer exist
	KindFile       Kind = "file"          // an existing regular file
	KindFileOrDash Kind = "file_or_stdin" // an existing regular file, or - for stdin
	KindOutDir     Kind = "out_dir"       // a directory to write, created if absent
	KindArtifact   Kind = "artifact"      // REL=@SRC, REL an artifact name
	KindRef        Kind = "ref"           // inbox:<id> or timer:<id>
	KindDuration   Kind = "duration"      // 90s, 2m, or seconds
	KindInt        Kind = "int"           // a bounded integer
	KindEnum       Kind = "enum"          // one of Enum
	KindHandles    Kind = "handles"       // comma-separated handles
	KindMessageIDs Kind = "message_ids"   // comma-separated message ids
	KindFields     Kind = "fields"        // comma-separated field names
	KindCursor     Kind = "cursor"        // an opaque page cursor
	KindInput      Kind = "input"         // --input: @FILE, -, or a JSON object
	KindJSON       Kind = "json"          // @FILE, -, or an inline JSON object
	KindCommand    Kind = "command"       // a command path or "errors" (schema)
	KindBool       Kind = "bool"
)

// BodyMax bounds a body value, inline or read from @FILE or stdin.
const BodyMax = 256 << 10

var (
	handleRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	nameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	uuidRe     = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
	sessionRe  = regexp.MustCompile(`^session:` + uuidRe + `$`)
	blockRe    = regexp.MustCompile(`^block:` + uuidRe + `$`)
	idRe       = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_.:-]{0,127}$`)
	threadRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}(/[a-z0-9][a-z0-9_.-]{0,127})?$`)
	fieldRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	cursorRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)
	artifactRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	commandRe  = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)
)

// invalid is a value that failed its kind.
type invalid struct {
	why string
}

func (e invalid) Error() string { return e.why }

func bad(format string, a ...any) error { return invalid{fmt.Sprintf(format, a...)} }

// q quotes a value for a message, every control character escaped, so an
// error never prints the bytes it refused.
func q(s string) string {
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return strconv.QuoteToASCII(s)
}

// cfBidi and zeroWidth are the format characters that hide or reorder text.
func cfBidi(r rune) bool    { return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 }
func zeroWidth(r rune) bool { return r >= 0x200b && r <= 0x200d || r == 0x2060 || r == 0xfeff }
func c1(r rune) bool        { return r >= 0x80 && r < 0xa0 }

// lineClean refuses what one line of text may not hold: C0 and C1 controls
// (newlines and ANSI escapes included), bidi controls, and zero-width
// characters; and invalid UTF-8.
func lineClean(s string) error {
	if err := core.LineClean(s); err != nil {
		return invalid{err.Error()}
	}
	return nil
}

// bodyClean refuses what a body may not hold: ANSI escapes and every other
// C0 control but tab and newline (a CR too), C1 controls, and bidi
// controls. Joiners and the LRM/RLM marks stay.
func bodyClean(s string) error {
	if !utf8.ValidString(s) {
		return bad("is not valid UTF-8")
	}
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n':
		case r < 0x20 || r == 0x7f || c1(r):
			return bad("holds a control character (%U)", r)
		case cfBidi(r):
			return bad("holds a bidi control (%U)", r)
		}
	}
	return nil
}

// CheckBody validates body text read from a file or stdin, as the body kind
// validates an inline one.
func CheckBody(s string) error {
	if len(s) > BodyMax {
		return bad("is %d bytes, over the %d-byte limit", len(s), BodyMax)
	}
	return bodyClean(s)
}

// check validates v against the value spec and returns it normalized (an
// absolute path for a path kind).
func (s Value) check(v string) (string, error) {
	switch s.Kind {
	case KindHandle:
		if !handleRe.MatchString(v) {
			return "", bad("%s is not a handle: want [a-z0-9][a-z0-9_-]{0,31}, such as claude-02", q(v))
		}
	case KindRole, KindCrew, KindTopic:
		if !nameRe.MatchString(v) {
			return "", bad("%s is not a %s name: want [a-z0-9][a-z0-9-]{0,31}, such as reviewer", q(v), s.Kind)
		}
	case KindTarget:
		if !handleRe.MatchString(v) {
			return "", bad("%s is neither a handle (claude-02) nor a role (reviewer)", q(v))
		}
	case KindSessionID:
		if !sessionRe.MatchString(v) {
			return "", bad("%s is not a Rex session id: want session:<uuid>, lowercase hex", q(v))
		}
	case KindBlockID:
		if !blockRe.MatchString(v) {
			return "", bad("%s is not a Rex block id: want block:<uuid>, lowercase hex", q(v))
		}
	case KindMessageID, KindEventID:
		if !idRe.MatchString(v) {
			return "", bad("%s is not an id: want 1-128 of [0-9A-Za-z_.:-], starting with a letter or digit", q(v))
		}
	case KindThreadID:
		if !threadRe.MatchString(v) {
			return "", bad("%s is not a thread id, such as p2p/alice__bob", q(v))
		}
	case KindOpID:
		if len(v) > 256 || strings.TrimSpace(v) == "" {
			return "", bad("must be 1-256 bytes of printable text")
		}
		if err := lineClean(v); err != nil {
			return "", err
		}
	case KindSingleLine:
		if len(v) > 4096 {
			return "", bad("is longer than 4096 bytes")
		}
		if err := lineClean(v); err != nil {
			return "", err
		}
	case KindBody:
		if v == "-" {
			return v, nil
		}
		if p, ok := strings.CutPrefix(v, "@"); ok {
			return atPath(p)
		}
		if err := CheckBody(v); err != nil {
			return "", err
		}
	case KindDir, KindFile, KindOutDir, KindPath:
		return checkPath(v, s.Kind)
	case KindFileOrDash:
		if v == "-" {
			return v, nil
		}
		return checkPath(v, KindFile)
	case KindArtifact:
		rel, src, ok := strings.Cut(v, "=@")
		if !ok {
			return "", bad("%s is not REL=@SRC", q(v))
		}
		if err := CheckArtifactName(rel); err != nil {
			return "", err
		}
		abs, err := checkPath(src, KindFile)
		if err != nil {
			return "", err
		}
		return rel + "=@" + abs, nil
	case KindRef:
		prefix, id, ok := strings.Cut(v, ":")
		if !ok || (prefix != "inbox" && prefix != "timer") || !idRe.MatchString(id) {
			return "", bad("%s is not inbox:<id> or timer:<id>", q(v))
		}
	case KindDuration:
		maxD := 24 * time.Hour
		if s.MaxDuration != "" {
			if parsed, err := time.ParseDuration(s.MaxDuration); err == nil && parsed > 0 {
				maxD = parsed
			}
		}
		d, err := ParseDurationBounded(v, maxD)
		if err != nil {
			return "", err
		}
		return d.String(), nil
	case KindInt:
		n, err := strconv.Atoi(v)
		if err != nil || n < s.Min || (s.Max != 0 && n > s.Max) {
			return "", bad("%s is not an integer from %d to %d", q(v), s.Min, s.Max)
		}
	case KindEnum:
		for _, e := range s.Enum {
			if v == e {
				return v, nil
			}
		}
		return "", bad("%s is not one of %s", q(v), strings.Join(s.Enum, ", "))
	case KindHandles, KindMessageIDs, KindFields:
		item := map[Kind]Kind{KindHandles: KindHandle, KindMessageIDs: KindMessageID, KindFields: KindFields}[s.Kind]
		var parts []string
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if item == KindFields {
				if !fieldRe.MatchString(p) {
					return "", bad("%s is not a field name", q(p))
				}
			} else if _, err := (Value{Kind: item}).check(p); err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		if len(parts) == 0 {
			return "", bad("names nothing")
		}
		if len(parts) > 100 {
			return "", bad("names more than 100")
		}
		return strings.Join(parts, ","), nil
	case KindCursor:
		if !cursorRe.MatchString(v) {
			return "", bad("%s is not a cursor this command returned", q(v))
		}
	case KindInput:
		if v == "-" || strings.HasPrefix(v, "{") {
			return v, nil
		}
		if p, ok := strings.CutPrefix(v, "@"); ok {
			return atPath(p)
		}
		return "", bad("is not @FILE, -, or a JSON object")
	case KindJSON:
		if v == "-" {
			return v, nil
		}
		if p, ok := strings.CutPrefix(v, "@"); ok {
			return atPath(p)
		}
		trimmed := strings.TrimSpace(v)
		if len(trimmed) > BodyMax {
			return "", bad("is %d bytes, over the %d-byte limit", len(trimmed), BodyMax)
		}
		if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
			return "", bad("is not @FILE, -, or a JSON object")
		}
		return trimmed, nil
	case KindCommand:
		if v != "errors" && !commandRe.MatchString(v) {
			return "", bad("%s is not a command word", q(v))
		}
	default:
		return "", bad("has no declared kind %q", s.Kind)
	}
	return v, nil
}

// atPath checks the file of an @FILE value and keeps the @, so the value
// still says "read this file" to whoever reads it after validation.
func atPath(p string) (string, error) {
	abs, err := checkPath(p, KindFile)
	if err != nil {
		return "", err
	}
	return "@" + abs, nil
}

// checkPath validates an input path: no control or format characters, made
// absolute and clean, and present as the kind says (an out_dir may be
// absent, but not a non-directory, and its parent must exist).
func checkPath(p string, kind Kind) (string, error) {
	if p == "" {
		return "", bad("names no path")
	}
	if err := lineClean(p); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", bad("%s cannot be made absolute: %v", q(p), err)
	}
	if kind == KindPath {
		return abs, nil
	}
	st, err := os.Stat(abs)
	switch {
	case kind == KindOutDir && os.IsNotExist(err):
		if pst, perr := os.Stat(filepath.Dir(abs)); perr != nil || !pst.IsDir() {
			return "", bad("%s: its parent directory does not exist", q(abs))
		}
		return abs, nil
	case err != nil:
		return "", bad("%s does not exist", q(abs))
	case (kind == KindDir || kind == KindOutDir) && !st.IsDir():
		return "", bad("%s is not a directory", q(abs))
	case kind == KindFile && !st.Mode().IsRegular():
		return "", bad("%s is not a regular file", q(abs))
	}
	return abs, nil
}

// CheckArtifactName validates an artifact's relative name: one or two
// segments of [a-z0-9][a-z0-9._-]*, ending in .md, never "..".
func CheckArtifactName(rel string) error {
	if strings.HasPrefix(rel, "/") || !strings.HasSuffix(rel, ".md") {
		return bad("artifact name %s must be relative and end in .md", q(rel))
	}
	segs := strings.Split(rel, "/")
	if len(segs) > 2 {
		return bad("artifact name %s has more than two segments", q(rel))
	}
	for _, s := range segs {
		if s == "." || s == ".." || strings.Contains(s, "..") || !artifactRe.MatchString(s) {
			return bad("artifact name %s: segment %s is not [a-z0-9][a-z0-9._-]*", q(rel), q(s))
		}
	}
	return nil
}

// ParseDuration reads a positive duration (90s, 2m) or bare seconds (60), up to 24h.
func ParseDuration(s string) (time.Duration, error) {
	return ParseDurationBounded(s, 24*time.Hour)
}

// ParseDurationBounded reads a positive duration (90s, 2m) or bare seconds (60),
// bounded above by maxD.
func ParseDurationBounded(s string, maxD time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		secs, perr := strconv.ParseFloat(s, 64)
		if perr != nil {
			return 0, bad("%s is neither a duration (90s, 2m) nor seconds", q(s))
		}
		d = time.Duration(secs * float64(time.Second))
	}
	if d <= 0 {
		return 0, bad("%s: a duration must be positive", q(s))
	}
	if d > maxD {
		return 0, bad("%s: a duration is at most %s", q(s), maxD)
	}
	return d, nil
}

// Snippet is s as one clean line of at most n runes: control, ANSI, bidi,
// and zero-width characters removed, whitespace runs folded to one space.
func Snippet(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	s = ansiRe.ReplaceAllString(s, "")
	var b strings.Builder
	space := false
	count := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f || c1(r) || cfBidi(r) || zeroWidth(r) || unicode.IsSpace(r) {
			if r == '\n' || r == '\t' || r == ' ' || unicode.IsSpace(r) {
				space = b.Len() > 0
			}
			continue
		}
		if space {
			if count >= n {
				break
			}
			b.WriteByte(' ')
			count++
			space = false
		}
		if count >= n {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

var ansiRe = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)
