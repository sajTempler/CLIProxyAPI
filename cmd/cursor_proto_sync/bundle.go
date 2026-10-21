package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The agent CLI is a webpack bundle. Generated protobuf-es messages carry a
// compact descriptor:
//
//	class X extends Base{... static $(){return["Name|1 field 9|2 other #0?|3 m 9,#1|4 o 8 kind",RefA,Mod.RefB]}}
//
// Field spec: "<no> <name> <type>[?|*] [oneof]". Type is a scalar code
// (protobuf ScalarType numbering), "#i" (index into the trailing refs), or
// "K,V" for a map. Enums are built by a helper call:
//
//	p=(0,l.QT)(n.C,d,"Name",[[0,"UNSPECIFIED"],[1,"X"]],1)
//
// where mode 1 means value names are prefixed with SCREAMING_CASE(Name)+"_".
// The package comes from the message's base class: static $p(){return d} with
// d="agent.v1.". Concatenated modules can hold several bases (packages).

var (
	reModuleStart = regexp.MustCompile(`[{,]"([\w@./:+\-=~#]{1,1000})"(?:\(([\w$]+(?:,[\w$]+)*)?\)|:\(([\w$]+(?:,[\w$]+)*)?\)=>|:function\(([\w$]+(?:,[\w$]+)*)?\))\{`)
	reExports     = regexp.MustCompile(`\b[\w$]+\.d\([\w$]+,\{`)
	reExportEntry = regexp.MustCompile(`([\w$]+|"[^"]+"):\(\)=>([\w$.]+)`)
	reImport      = regexp.MustCompile(`([\w$]+)=([\w$]+)\("([^"]+)"\)`)
	reImportComma = regexp.MustCompile(`([\w$]+)=\((?:[\w$]+\("[^"]+"\),)+[\w$]+\("([^"]+)"\)\)`)
	reBaseClass   = regexp.MustCompile(`class ([\w$]+) extends [\w$.]+\{static get runtime\(\)\{[^}]*\}static \$p\(\)\{return(?: ([\w$]+)|"([\w.]*)")\}`)
	reClassStart  = regexp.MustCompile(`class ([\w$]+) extends ([\w$.]+)`)
	reEnum        = regexp.MustCompile(`([\w$]+)=(?:\((?:[\w$.]+,)*)?\(0,(?:[\w$]+|[\w$]+\("[^"]+"\))\.[\w$]+\)\([\w$]+\.[\w$]+,([\w$]+|"[\w.]*"),"([\w.]+)",\[((?:\[-?\d+,"\w*"\],?)*)\](?:,([^)]*))?\)`)
	reEnumValue   = regexp.MustCompile(`\[(-?\d+),"(\w*)"\]`)
	reWKTMessage  = regexp.MustCompile(`([\w$]+)\.typeName="([\w.]+)"`)
	reWKTEnum     = regexp.MustCompile(`setEnumType\(([\w$]+),"([\w.]+)"`)
	reService     = regexp.MustCompile(`\{typeName:"([\w.]+)",methods:\{`)
	reMethod      = regexp.MustCompile(`([\w$]+):\{name:"(\w+)",I:([\w$.]+),O:([\w$.]+),kind:[\w$.]*?\.(\w+)\}`)
)

const compactMarker = `static $(){return["`

// jsModule is one webpack module with the symbols the resolver needs.
type jsModule struct {
	path     string
	body     string
	exports  map[string]string // export name -> local expression (ident or Mod.Export)
	imports  map[string]string // local ident -> module path
	bases    map[string]string // base class ident -> proto package with trailing dot
	messages map[string]*rawMessage
	enums    map[string]*Enum  // local ident -> enum
	wkt      map[string]string // local ident -> full name (well-known types from @bufbuild/protobuf)
	services []rawService
}

type rawMessage struct {
	local string
	pkg   string
	desc  string
	refs  []string
}

type rawService struct {
	name    string
	methods []rawMethod
}

type rawMethod struct {
	name, in, out, kind string
}

// LoadBundle scans every .js file in dir and returns the protobuf schema it embeds.
func LoadBundle(dir string) (*Schema, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read bundle dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	files := make(map[string]string, len(names))
	for _, n := range names {
		b, errRead := os.ReadFile(filepath.Join(dir, n))
		if errRead != nil {
			return nil, fmt.Errorf("read %s: %w", n, errRead)
		}
		files[n] = string(b)
	}
	return parseBundle(names, files)
}

func parseBundle(order []string, files map[string]string) (*Schema, error) {
	mods := map[string]*jsModule{}
	for _, name := range order {
		for _, m := range splitModules(files[name]) {
			if _, dup := mods[m.path]; dup {
				continue
			}
			mods[m.path] = m
		}
	}
	for _, m := range mods {
		parseModule(m)
	}
	return resolve(mods)
}

func splitModules(src string) []*jsModule {
	locs := reModuleStart.FindAllStringSubmatchIndex(src, -1)
	out := make([]*jsModule, 0, len(locs))
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out = append(out, &jsModule{path: src[loc[2]:loc[3]], body: src[loc[1]:end]})
	}
	return out
}

func parseModule(m *jsModule) {
	m.exports = map[string]string{}
	m.imports = map[string]string{}
	m.messages = map[string]*rawMessage{}
	m.enums = map[string]*Enum{}
	m.wkt = map[string]string{}
	body := m.body

	if loc := reExports.FindStringIndex(body); loc != nil {
		end := strings.IndexByte(body[loc[1]:], '}')
		if end >= 0 {
			for _, e := range reExportEntry.FindAllStringSubmatch(body[loc[1]:loc[1]+end], -1) {
				m.exports[strings.Trim(e[1], `"`)] = e[2]
			}
		}
	}
	for _, im := range reImport.FindAllStringSubmatch(body, -1) {
		if _, ok := m.imports[im[1]]; !ok {
			m.imports[im[1]] = im[3]
		}
	}
	for _, im := range reImportComma.FindAllStringSubmatch(body, -1) {
		if _, ok := m.imports[im[1]]; !ok {
			m.imports[im[1]] = im[2]
		}
	}
	m.bases = map[string]string{}
	for _, bm := range reBaseClass.FindAllStringSubmatch(body, -1) {
		if bm[3] != "" {
			m.bases[bm[1]] = bm[3]
		} else {
			m.bases[bm[1]] = lookupStringConst(body, bm[2])
		}
	}

	for off := 0; ; {
		i := strings.Index(body[off:], compactMarker)
		if i < 0 {
			break
		}
		start := off + i
		desc, refs, next := readCompact(body, start+len(compactMarker))
		off = next
		cls := reClassStart.FindAllStringSubmatch(body[:start], -1)
		if len(cls) == 0 {
			continue
		}
		local := cls[len(cls)-1][1]
		m.messages[local] = &rawMessage{local: local, pkg: m.bases[cls[len(cls)-1][2]], desc: desc, refs: refs}
	}

	for _, em := range reEnum.FindAllStringSubmatch(body, -1) {
		pkg := strings.Trim(em[2], `"`)
		if !strings.HasPrefix(em[2], `"`) {
			pkg = lookupStringConst(body, em[2])
		}
		name := em[3]
		mode := strings.TrimSpace(em[5])
		prefix := ""
		if mode == "1" {
			prefix = enumPrefix(name[strings.LastIndexByte(name, '.')+1:])
		}
		e := &Enum{FullName: pkg + name, File: m.path}
		for _, v := range reEnumValue.FindAllStringSubmatch(em[4], -1) {
			n, _ := strconv.Atoi(v[1])
			e.Values = append(e.Values, EnumValue{Number: int32(n), Name: prefix + v[2]})
		}
		m.enums[em[1]] = e
	}
	for _, w := range reWKTMessage.FindAllStringSubmatch(body, -1) {
		m.wkt[w[1]] = w[2]
	}
	for _, w := range reWKTEnum.FindAllStringSubmatch(body, -1) {
		m.wkt[w[1]] = w[2]
	}
	for _, loc := range reService.FindAllStringSubmatchIndex(body, -1) {
		svc := rawService{name: body[loc[2]:loc[3]]}
		rest := body[loc[1]:]
		// Methods end at the "}}" that closes the methods object.
		if end := strings.Index(rest, "}}}"); end >= 0 {
			rest = rest[:end+2]
		}
		for _, mm := range reMethod.FindAllStringSubmatch(rest, -1) {
			svc.methods = append(svc.methods, rawMethod{name: mm[2], in: mm[3], out: mm[4], kind: mm[5]})
		}
		m.services = append(m.services, svc)
	}
}

// readCompact reads `"desc",ref,ref]` starting just after the opening quote.
func readCompact(body string, pos int) (desc string, refs []string, next int) {
	var b strings.Builder
	i := pos
	for ; i < len(body); i++ {
		c := body[i]
		if c == '\\' && i+1 < len(body) {
			i++
			b.WriteByte(body[i])
			continue
		}
		if c == '"' {
			break
		}
		b.WriteByte(c)
	}
	desc = b.String()
	end := strings.IndexByte(body[i:], ']')
	if end < 0 {
		return desc, nil, len(body)
	}
	tail := body[i+1 : i+end]
	for _, r := range strings.Split(tail, ",") {
		if r = strings.TrimSpace(r); r != "" {
			refs = append(refs, r)
		}
	}
	return desc, refs, i + end
}

func lookupStringConst(body, ident string) string {
	re := regexp.MustCompile(`(?:^|[^\w$.])` + regexp.QuoteMeta(ident) + `="([\w.]*)"`)
	if m := re.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// enumPrefix mirrors the bundle's helper: "FooBar" -> "FOO_BAR_".
func enumPrefix(local string) string {
	if local == "" {
		return "_"
	}
	var b strings.Builder
	b.WriteByte(local[0])
	for _, r := range local[1:] {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String()) + "_"
}

// resolver turns module-local references into fully qualified type names.
type resolver struct {
	mods map[string]*jsModule
}

func (r *resolver) ref(m *jsModule, expr string, depth int) (string, bool, error) {
	if depth > 8 {
		return "", false, fmt.Errorf("reference chain too deep at %q in %s", expr, m.path)
	}
	if dot := strings.IndexByte(expr, '.'); dot >= 0 {
		alias, exp := expr[:dot], expr[dot+1:]
		path, ok := m.imports[alias]
		if !ok {
			return "", false, fmt.Errorf("unknown import %q in %s", alias, m.path)
		}
		t, ok := r.mods[path]
		if !ok {
			return "", false, fmt.Errorf("module %q not found (from %s)", path, m.path)
		}
		local, ok := t.exports[exp]
		if !ok {
			return "", false, fmt.Errorf("export %q not found in %s", exp, path)
		}
		return r.ref(t, local, depth+1)
	}
	if msg, ok := m.messages[expr]; ok {
		return msg.pkg + messageName(msg.desc), true, nil
	}
	if e, ok := m.enums[expr]; ok {
		return e.FullName, false, nil
	}
	if name, ok := m.wkt[expr]; ok {
		return name, !strings.HasSuffix(name, "NullValue"), nil
	}
	return "", false, fmt.Errorf("unresolved local %q in %s", expr, m.path)
}

func messageName(desc string) string {
	if i := strings.IndexByte(desc, '|'); i >= 0 {
		return desc[:i]
	}
	return desc
}

func resolve(mods map[string]*jsModule) (*Schema, error) {
	r := &resolver{mods: mods}
	s := NewSchema()
	paths := make([]string, 0, len(mods))
	for p := range mods {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var errs []string
	for _, p := range paths {
		m := mods[p]
		for _, e := range m.enums {
			s.Enums[e.FullName] = e
		}
		for _, raw := range m.messages {
			if raw.pkg == "" {
				errs = append(errs, fmt.Sprintf("no package for %s in %s", messageName(raw.desc), m.path))
				continue
			}
			msg, err := r.message(m, raw)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			s.Messages[msg.FullName] = msg
		}
		for _, svc := range m.services {
			out := &Service{FullName: svc.name}
			for _, mm := range svc.methods {
				in, _, errIn := r.ref(m, mm.in, 0)
				outName, _, errOut := r.ref(m, mm.out, 0)
				if errIn != nil || errOut != nil {
					errs = append(errs, fmt.Sprintf("service %s/%s: %v %v", svc.name, mm.name, errIn, errOut))
					continue
				}
				out.Methods = append(out.Methods, Method{
					Name: mm.name, Input: in, Output: outName,
					ClientStreaming: mm.kind == "BiDiStreaming" || mm.kind == "ClientStreaming",
					ServerStreaming: mm.kind == "BiDiStreaming" || mm.kind == "ServerStreaming",
				})
			}
			if prev, ok := s.Services[svc.name]; !ok || len(prev.Methods) < len(out.Methods) {
				s.Services[svc.name] = out
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		s.Warnings = errs
	}
	if len(s.Messages) == 0 {
		return nil, fmt.Errorf("no compact protobuf descriptors found")
	}
	return s, nil
}

func (r *resolver) message(m *jsModule, raw *rawMessage) (*Message, error) {
	parts := strings.Split(raw.desc, "|")
	msg := &Message{FullName: raw.pkg + parts[0], File: m.path}
	typeRef := func(t string) (FieldType, error) {
		if !strings.HasPrefix(t, "#") {
			n, err := strconv.Atoi(t)
			if err != nil {
				return FieldType{}, fmt.Errorf("bad scalar %q", t)
			}
			return FieldType{Scalar: n}, nil
		}
		idx, err := strconv.Atoi(t[1:])
		if err != nil || idx < 0 || idx >= len(raw.refs) {
			return FieldType{}, fmt.Errorf("bad ref %q", t)
		}
		name, isMsg, errRef := r.ref(m, raw.refs[idx], 0)
		if errRef != nil {
			return FieldType{}, errRef
		}
		if isMsg {
			return FieldType{Message: name}, nil
		}
		return FieldType{Enum: name}, nil
	}
	for _, spec := range parts[1:] {
		f := strings.Split(spec, " ")
		if len(f) < 3 {
			return nil, fmt.Errorf("%s: bad field spec %q", msg.FullName, spec)
		}
		no, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("%s: bad field number %q", msg.FullName, spec)
		}
		fd := Field{Number: int32(no), Name: f[1]}
		typ := f[2]
		mod := ""
		if len(f) > 3 {
			mod = f[3]
		}
		if last := typ[len(typ)-1]; last == '?' || last == '*' {
			mod = string(last)
			typ = typ[:len(typ)-1]
		}
		switch mod {
		case "?":
			fd.Label = LabelOptional
		case "*":
			fd.Label = LabelRepeated
		case "":
		default:
			fd.Oneof = mod
		}
		if k, v, isMap := strings.Cut(typ, ","); isMap {
			key, errK := strconv.Atoi(k)
			if errK != nil {
				return nil, fmt.Errorf("%s: bad map key %q", msg.FullName, spec)
			}
			val, errV := typeRef(v)
			if errV != nil {
				return nil, fmt.Errorf("%s.%s: %w", msg.FullName, fd.Name, errV)
			}
			fd.MapKey = key
			fd.Type = val
		} else {
			t, errT := typeRef(typ)
			if errT != nil {
				return nil, fmt.Errorf("%s.%s: %w", msg.FullName, fd.Name, errT)
			}
			fd.Type = t
		}
		msg.Fields = append(msg.Fields, fd)
	}
	return msg, nil
}
