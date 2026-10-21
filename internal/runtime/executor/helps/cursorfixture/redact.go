package cursorfixture

import (
	"os"
	"os/user"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Placeholders written in place of redacted values.
const (
	RedactedSecret = "REDACTED"
	RedactedEmail  = "user@example.com"
	RedactedUser   = "user"
)

type pattern struct {
	name string
	re   *regexp.Regexp
	repl string
}

// Order matters: tokens before emails/paths so a JWT inside a URL is caught whole.
var patterns = []pattern{
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), RedactedSecret},
	{"bearer", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`), "Bearer " + RedactedSecret},
	{"api-key", regexp.MustCompile(`\b(?:sk|key|crsr|user)_[A-Za-z0-9]{24,}\b|\bsk-[A-Za-z0-9_-]{20,}`), RedactedSecret},
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`), RedactedEmail},
	{"home-path", regexp.MustCompile(`/(Users|home)/[^/\s"'\\]+`), "/$1/" + RedactedUser},
	{"windows-home-path", regexp.MustCompile(`(?i)\b([A-Z]):\\Users\\[^\\\s"']+`), `$1:\Users\` + RedactedUser},
}

// sensitiveField reports whether a field's whole value is secret regardless of content.
func sensitiveField(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"token", "secret", "password", "api_key", "credential", "authorization", "cookie"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// Redactor strips tokens, emails, home paths and caller-supplied literals
// (user name, host name) from strings and protobuf messages.
type Redactor struct {
	literals []string
}

// NewRedactor returns a redactor that also replaces each non-trivial literal with "user".
func NewRedactor(literals ...string) *Redactor {
	r := &Redactor{}
	for _, l := range literals {
		if l = strings.TrimSpace(l); len(l) >= 3 {
			r.literals = append(r.literals, l)
		}
	}
	// Longest first, so "jdoe-mbp" is replaced whole before "jdoe".
	sort.Slice(r.literals, func(i, j int) bool { return len(r.literals[i]) > len(r.literals[j]) })
	return r
}

// DefaultLiterals returns machine identity strings that must never reach a fixture:
// the login name, the home directory's base name and the short host name.
func DefaultLiterals() []string {
	var out []string
	if u, err := user.Current(); err == nil {
		out = append(out, u.Username)
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, home[strings.LastIndexAny(home, `/\`)+1:])
	}
	if host, err := os.Hostname(); err == nil {
		out = append(out, strings.SplitN(host, ".", 2)[0])
	}
	return out
}

// String redacts one string.
func (r *Redactor) String(s string) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	for _, l := range r.literals {
		s = replaceFold(s, l, RedactedUser)
	}
	return s
}

func replaceFold(s, old, repl string) string {
	lowerS, lowerOld := strings.ToLower(s), strings.ToLower(old)
	if !strings.Contains(lowerS, lowerOld) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(lowerS, lowerOld)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(repl)
		s, lowerS = s[i+len(old):], lowerS[i+len(old):]
	}
}

// Message redacts a protobuf message in place: every string (including
// google.protobuf.Value strings and map values), UTF-8 bytes fields such as
// KV blobs, and whole values of fields named like credentials.
// Blob ids are content hashes and will no longer match redacted blob data.
func (r *Redactor) Message(m proto.Message) {
	if m == nil {
		return
	}
	r.walk(m.ProtoReflect())
}

func (r *Redactor) walk(m protoreflect.Message) {
	// Singular replacements are applied after Range; mutating during Range is not allowed.
	sets := map[protoreflect.FieldDescriptor]protoreflect.Value{}
	defer func() {
		for fd, nv := range sets {
			m.Set(fd, nv)
		}
	}()
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			mp := v.Map()
			type update struct {
				k protoreflect.MapKey
				v protoreflect.Value
			}
			var updates []update
			mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if nv, changed := r.value(fd.MapValue(), mv, sensitiveField(string(fd.Name()))); changed {
					updates = append(updates, update{k, nv})
				}
				return true
			})
			for _, u := range updates {
				mp.Set(u.k, u.v)
			}
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				if nv, changed := r.value(fd, l.Get(i), sensitiveField(string(fd.Name()))); changed {
					l.Set(i, nv)
				}
			}
		default:
			if nv, changed := r.value(fd, v, sensitiveField(string(fd.Name()))); changed {
				sets[fd] = nv
			}
		}
		return true
	})
}

func (r *Redactor) value(fd protoreflect.FieldDescriptor, v protoreflect.Value, sensitive bool) (protoreflect.Value, bool) {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		r.walk(v.Message())
		return v, false
	case protoreflect.StringKind:
		s := v.String()
		if sensitive && s != "" {
			return protoreflect.ValueOfString(RedactedSecret), true
		}
		if red := r.String(s); red != s {
			return protoreflect.ValueOfString(red), true
		}
	case protoreflect.BytesKind:
		b := v.Bytes()
		if sensitive && len(b) > 0 {
			return protoreflect.ValueOfBytes([]byte(RedactedSecret)), true
		}
		if utf8.Valid(b) {
			if red := r.String(string(b)); red != string(b) {
				return protoreflect.ValueOfBytes([]byte(red)), true
			}
		}
	}
	return v, false
}

// Scan returns the names of secret patterns and literals found in data.
// Fixture tests use it as a guard: committed fixtures must scan clean.
func Scan(data []byte, literals ...string) []string {
	var hits []string
	s := string(data)
	for _, p := range patterns {
		for _, m := range p.re.FindAllString(s, -1) {
			if !isPlaceholder(m) {
				hits = append(hits, p.name+": "+m)
				break
			}
		}
	}
	lower := strings.ToLower(s)
	for _, l := range literals {
		if l = strings.TrimSpace(l); len(l) >= 3 && strings.Contains(lower, strings.ToLower(l)) {
			hits = append(hits, "literal: "+l)
		}
	}
	return hits
}

func isPlaceholder(s string) bool {
	return s == RedactedEmail || strings.HasSuffix(s, "/"+RedactedUser) || strings.HasSuffix(s, `\`+RedactedUser) || strings.HasSuffix(s, RedactedSecret)
}
