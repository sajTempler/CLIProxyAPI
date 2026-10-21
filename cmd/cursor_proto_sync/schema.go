package main

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Label is a proto3 field label.
type Label int

const (
	LabelNone Label = iota
	LabelOptional
	LabelRepeated
)

// FieldType is exactly one of a scalar code, a message name or an enum name.
type FieldType struct {
	Scalar  int // protobuf ScalarType numbering (9 string, 8 bool, ...), 0 if not scalar
	Message string
	Enum    string
}

// Field is a message field. A map field has MapKey != 0 and Type is the value type.
type Field struct {
	Number int32
	Name   string
	Type   FieldType
	MapKey int
	Label  Label
	Oneof  string
}

// Message is a protobuf message with its fully qualified name.
type Message struct {
	FullName string
	File     string // bundle module path it came from
	Fields   []Field
}

// EnumValue is one enum constant.
type EnumValue struct {
	Number int32
	Name   string
}

// Enum is a protobuf enum with its fully qualified name.
type Enum struct {
	FullName string
	File     string
	Values   []EnumValue
}

// Method is one RPC.
type Method struct {
	Name                             string
	Input, Output                    string
	ClientStreaming, ServerStreaming bool
}

// Service is one RPC service.
type Service struct {
	FullName string
	Methods  []Method
}

// Schema is everything extracted from a bundle.
type Schema struct {
	Messages map[string]*Message
	Enums    map[string]*Enum
	Services map[string]*Service
	Warnings []string
}

// NewSchema returns an empty schema.
func NewSchema() *Schema {
	return &Schema{Messages: map[string]*Message{}, Enums: map[string]*Enum{}, Services: map[string]*Service{}}
}

var scalarNames = map[int]string{
	1: "double", 2: "float", 3: "int64", 4: "uint64", 5: "int32", 6: "fixed64", 7: "fixed32",
	8: "bool", 9: "string", 12: "bytes", 13: "uint32", 15: "sfixed32", 16: "sfixed64", 17: "sint32", 18: "sint64",
}

// TypeString renders a field type the way SNAPSHOT.txt and .proto files spell it.
func (t FieldType) TypeString() string {
	switch {
	case t.Message != "":
		return t.Message
	case t.Enum != "":
		return t.Enum
	default:
		if n, ok := scalarNames[t.Scalar]; ok {
			return n
		}
		return "scalar" + strconv.Itoa(t.Scalar)
	}
}

// Closure returns the sorted names of all messages and enums reachable from roots.
// A root is a message name or "pkg.Service/Method".
func (s *Schema) Closure(roots []string) (msgs, enums []string, err error) {
	seenM := map[string]bool{}
	seenE := map[string]bool{}
	var visit func(name string) error
	visit = func(name string) error {
		if seenM[name] {
			return nil
		}
		m, ok := s.Messages[name]
		if !ok {
			if isWellKnown(name) {
				return nil
			}
			return fmt.Errorf("message %s not found", name)
		}
		seenM[name] = true
		// Nested types live inside their parent, so the parent is always generated too.
		if parent := s.parentMessage(name); parent != "" {
			if err := visit(parent); err != nil {
				return err
			}
		}
		for _, f := range m.Fields {
			switch {
			case f.Type.Message != "":
				if err := visit(f.Type.Message); err != nil {
					return fmt.Errorf("%s.%s: %w", name, f.Name, err)
				}
			case f.Type.Enum != "":
				if isWellKnown(f.Type.Enum) {
					continue
				}
				if _, ok := s.Enums[f.Type.Enum]; !ok {
					return fmt.Errorf("%s.%s: enum %s not found", name, f.Name, f.Type.Enum)
				}
				seenE[f.Type.Enum] = true
				if parent := s.parentMessage(f.Type.Enum); parent != "" {
					if err := visit(parent); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	for _, r := range roots {
		if svc, method, ok := strings.Cut(r, "/"); ok {
			sv, found := s.Services[svc]
			if !found {
				return nil, nil, fmt.Errorf("service %s not found", svc)
			}
			var hit bool
			for _, m := range sv.Methods {
				if m.Name == method {
					hit = true
					if err := visit(m.Input); err != nil {
						return nil, nil, err
					}
					if err := visit(m.Output); err != nil {
						return nil, nil, err
					}
				}
			}
			if !hit {
				return nil, nil, fmt.Errorf("method %s not found", r)
			}
			continue
		}
		if err := visit(r); err != nil {
			return nil, nil, err
		}
	}
	for k := range seenM {
		msgs = append(msgs, k)
	}
	for k := range seenE {
		enums = append(enums, k)
	}
	sort.Strings(msgs)
	sort.Strings(enums)
	return msgs, enums, nil
}

// parentMessage returns the enclosing message of a nested type, or "".
func (s *Schema) parentMessage(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	if _, ok := s.Messages[name[:i]]; ok {
		return name[:i]
	}
	return ""
}

func isWellKnown(name string) bool {
	return strings.HasPrefix(name, "google.protobuf.")
}

// Snapshot is the committed view used by --check.
type Snapshot struct {
	AgentVersion string
	RPCs         map[string]string // "pkg.Service/Method" -> "<input> <output> <unary|server|client|bidi>"
	Messages     map[string][]SnapField
	Enums        map[string][]EnumValue
}

// SnapField is one field line in SNAPSHOT.txt.
type SnapField struct {
	Number int32
	Name   string
	Type   string // "string", "agent.v1.Foo", "map<string,agent.v1.Foo>"
	Label  string // "", "optional", "repeated"
	Oneof  string
}

func (f SnapField) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s %s", f.Number, f.Name, f.Type)
	if f.Label != "" {
		b.WriteString(" " + f.Label)
	}
	if f.Oneof != "" {
		b.WriteString(" oneof=" + f.Oneof)
	}
	return b.String()
}

// BuildSnapshot converts the selected part of a schema to a snapshot.
func BuildSnapshot(s *Schema, version string, msgs, enums []string) *Snapshot {
	snap := &Snapshot{AgentVersion: version, RPCs: map[string]string{}, Messages: map[string][]SnapField{}, Enums: map[string][]EnumValue{}}
	for _, name := range msgs {
		m := s.Messages[name]
		fields := make([]SnapField, 0, len(m.Fields))
		for _, f := range m.Fields {
			sf := SnapField{Number: f.Number, Name: f.Name, Type: f.Type.TypeString(), Oneof: f.Oneof}
			if f.MapKey != 0 {
				sf.Type = "map<" + FieldType{Scalar: f.MapKey}.TypeString() + "," + sf.Type + ">"
			}
			switch f.Label {
			case LabelOptional:
				sf.Label = "optional"
			case LabelRepeated:
				sf.Label = "repeated"
			}
			fields = append(fields, sf)
		}
		sort.Slice(fields, func(i, j int) bool { return fields[i].Number < fields[j].Number })
		snap.Messages[name] = fields
	}
	for _, name := range enums {
		vals := append([]EnumValue(nil), s.Enums[name].Values...)
		sort.Slice(vals, func(i, j int) bool { return vals[i].Number < vals[j].Number })
		snap.Enums[name] = vals
	}
	return snap
}

const snapshotHeader = "# Generated by cmd/cursor_proto_sync. Do not edit.\n# Format: \"rpc <service/method> <input> <output> <kind>\" lines, then \"message <name>\" / \"enum <name>\"\n# blocks of indented \"<no> <name> <type> [optional|repeated] [oneof=<name>]\" or \"<no> <NAME>\" lines.\n"

// RPCLines describes the root RPCs for the snapshot.
func RPCLines(s *Schema, roots []string) map[string]string {
	out := map[string]string{}
	for _, r := range roots {
		svc, method, ok := strings.Cut(r, "/")
		if !ok || s.Services[svc] == nil {
			continue
		}
		for _, m := range s.Services[svc].Methods {
			if m.Name != method {
				continue
			}
			kind := "unary"
			switch {
			case m.ClientStreaming && m.ServerStreaming:
				kind = "bidi"
			case m.ServerStreaming:
				kind = "server"
			case m.ClientStreaming:
				kind = "client"
			}
			out[r] = m.Input + " " + m.Output + " " + kind
		}
	}
	return out
}

// MissingUsed returns used messages or fields that this snapshot does not contain.
func (snap *Snapshot) MissingUsed(used map[string][]string) []string {
	var missing []string
	for _, msg := range sortedKeys(used) {
		fields, ok := snap.Messages[msg]
		if !ok {
			missing = append(missing, msg)
			continue
		}
		have := map[string]bool{}
		for _, f := range fields {
			have[f.Name] = true
		}
		for _, f := range used[msg] {
			if !have[f] {
				missing = append(missing, msg+"."+f)
			}
		}
	}
	return missing
}

// Write renders the snapshot deterministically.
func (snap *Snapshot) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	fmt.Fprint(bw, snapshotHeader)
	fmt.Fprintf(bw, "agent_version: %s\n", snap.AgentVersion)
	if len(snap.RPCs) > 0 {
		bw.WriteString("\n")
	}
	for _, r := range sortedKeys(snap.RPCs) {
		fmt.Fprintf(bw, "rpc %s %s\n", r, snap.RPCs[r])
	}
	names := sortedKeys(snap.Messages)
	for _, n := range names {
		fmt.Fprintf(bw, "\nmessage %s\n", n)
		for _, f := range snap.Messages[n] {
			fmt.Fprintf(bw, "  %s\n", f)
		}
	}
	for _, n := range sortedKeys(snap.Enums) {
		fmt.Fprintf(bw, "\nenum %s\n", n)
		for _, v := range snap.Enums[n] {
			fmt.Fprintf(bw, "  %d %s\n", v.Number, v.Name)
		}
	}
	return bw.Flush()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ParseSnapshot reads a snapshot written by Write.
func ParseSnapshot(r io.Reader) (*Snapshot, error) {
	snap := &Snapshot{RPCs: map[string]string{}, Messages: map[string][]SnapField{}, Enums: map[string][]EnumValue{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var curMsg, curEnum string
	line := 0
	for sc.Scan() {
		line++
		t := sc.Text()
		switch {
		case t == "" || strings.HasPrefix(t, "#"):
			continue
		case strings.HasPrefix(t, "agent_version: "):
			snap.AgentVersion = strings.TrimPrefix(t, "agent_version: ")
		case strings.HasPrefix(t, "rpc "):
			name, rest, _ := strings.Cut(strings.TrimPrefix(t, "rpc "), " ")
			snap.RPCs[name] = rest
		case strings.HasPrefix(t, "message "):
			curMsg, curEnum = strings.TrimPrefix(t, "message "), ""
			snap.Messages[curMsg] = []SnapField{}
		case strings.HasPrefix(t, "enum "):
			curMsg, curEnum = "", strings.TrimPrefix(t, "enum ")
			snap.Enums[curEnum] = []EnumValue{}
		case strings.HasPrefix(t, "  "):
			parts := strings.Fields(t)
			no, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, fmt.Errorf("snapshot line %d: bad number", line)
			}
			switch {
			case curMsg != "" && len(parts) >= 3:
				f := SnapField{Number: int32(no), Name: parts[1], Type: parts[2]}
				for _, p := range parts[3:] {
					if o, ok := strings.CutPrefix(p, "oneof="); ok {
						f.Oneof = o
					} else {
						f.Label = p
					}
				}
				snap.Messages[curMsg] = append(snap.Messages[curMsg], f)
			case curEnum != "" && len(parts) == 2:
				snap.Enums[curEnum] = append(snap.Enums[curEnum], EnumValue{Number: int32(no), Name: parts[1]})
			default:
				return nil, fmt.Errorf("snapshot line %d: unexpected %q", line, t)
			}
		default:
			return nil, fmt.Errorf("snapshot line %d: unexpected %q", line, t)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if snap.AgentVersion == "" {
		return nil, fmt.Errorf("snapshot has no agent_version")
	}
	return snap, nil
}

// Report is the result of comparing a fresh extraction against the snapshot.
type Report struct {
	OldVersion, NewVersion string
	Breaking               []string // changes in used messages that break the wire contract
	Changes                []string // other differences (added fields, unused messages, renames)
	NewExecTypes           []string // new ExecServerMessage variants (soft-denied, but worth a look)
}

// HasBreaking reports whether --check must fail.
func (r *Report) HasBreaking() bool { return len(r.Breaking) > 0 }

// Diff compares old (committed) and new (installed) snapshots. used maps a
// message to the fields the provider relies on (see main.go).
func Diff(old, cur *Snapshot, used map[string][]string) *Report {
	rep := &Report{OldVersion: old.AgentVersion, NewVersion: cur.AgentVersion}
	usedSet := map[string]bool{}
	usedField := map[string]bool{}
	for msg, fields := range used {
		usedSet[msg] = true
		for _, f := range fields {
			usedField[msg+"."+f] = true
		}
	}
	for _, r := range sortedKeys(old.RPCs) {
		switch now, ok := cur.RPCs[r]; {
		case !ok:
			rep.Breaking = append(rep.Breaking, fmt.Sprintf("rpc %s removed", r))
		case now != old.RPCs[r]:
			rep.Breaking = append(rep.Breaking, fmt.Sprintf("rpc %s changed: %s -> %s", r, old.RPCs[r], now))
		}
	}
	for _, name := range sortedKeys(old.Messages) {
		oldFields := old.Messages[name]
		newFields, ok := cur.Messages[name]
		if !ok {
			add(rep, usedSet[name], fmt.Sprintf("message %s removed", name))
			continue
		}
		byNo := map[int32]SnapField{}
		byName := map[string]SnapField{}
		for _, f := range newFields {
			byNo[f.Number] = f
			byName[f.Name] = f
		}
		oldNos := map[int32]bool{}
		for _, of := range oldFields {
			oldNos[of.Number] = true
			nf, ok := byNo[of.Number]
			switch {
			case !ok:
				if moved, ok := byName[of.Name]; ok {
					add(rep, usedSet[name], fmt.Sprintf("%s.%s renumbered %d -> %d", name, of.Name, of.Number, moved.Number))
				} else {
					add(rep, usedField[name+"."+of.Name], fmt.Sprintf("%s.%s (#%d) removed", name, of.Name, of.Number))
				}
			case nf.Type != of.Type:
				add(rep, usedSet[name], fmt.Sprintf("%s #%d %s: type %s -> %s", name, of.Number, of.Name, of.Type, nf.Type))
			case nf.Label != of.Label && (nf.Label == "repeated" || of.Label == "repeated"):
				add(rep, usedSet[name], fmt.Sprintf("%s #%d %s: label %q -> %q", name, of.Number, of.Name, of.Label, nf.Label))
			case nf.Name != of.Name:
				rep.Changes = append(rep.Changes, fmt.Sprintf("%s #%d renamed %s -> %s", name, of.Number, of.Name, nf.Name))
			case nf.Label != of.Label || nf.Oneof != of.Oneof:
				rep.Changes = append(rep.Changes, fmt.Sprintf("%s #%d %s: presence/oneof %q/%q -> %q/%q", name, of.Number, of.Name, of.Label, of.Oneof, nf.Label, nf.Oneof))
			}
		}
		for _, nf := range newFields {
			if oldNos[nf.Number] {
				continue
			}
			if name == execServerMessage && nf.Oneof != "" {
				rep.NewExecTypes = append(rep.NewExecTypes, fmt.Sprintf("%s (#%d, %s)", nf.Name, nf.Number, nf.Type))
				continue
			}
			if usedSet[name] {
				rep.Changes = append(rep.Changes, fmt.Sprintf("%s: new field %s", name, nf))
			}
		}
	}
	var added int
	for name := range cur.Messages {
		if _, ok := old.Messages[name]; !ok {
			added++
		}
	}
	if added > 0 {
		rep.Changes = append(rep.Changes, fmt.Sprintf("%d new messages in the generated closure", added))
	}
	for _, u := range sortedKeys(used) {
		if _, ok := old.Messages[u]; !ok {
			if _, ok := cur.Messages[u]; !ok {
				rep.Breaking = append(rep.Breaking, fmt.Sprintf("used message %s missing from both snapshots", u))
			}
		}
	}
	return rep
}

func add(rep *Report, breaking bool, msg string) {
	if breaking {
		rep.Breaking = append(rep.Breaking, msg)
		return
	}
	rep.Changes = append(rep.Changes, msg)
}

// Print writes a human-readable report.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "snapshot agent: %s\ninstalled agent: %s\n", r.OldVersion, r.NewVersion)
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(w, "%s (%d):\n", title, len(lines))
		for _, l := range lines {
			fmt.Fprintf(w, "  - %s\n", l)
		}
	}
	section("BREAKING changes in used messages", r.Breaking)
	section("New exec request types (will be soft-denied)", r.NewExecTypes)
	section("Other changes", r.Changes)
	if !r.HasBreaking() && len(r.NewExecTypes) == 0 && len(r.Changes) == 0 {
		fmt.Fprintln(w, "no protocol changes")
	}
}
