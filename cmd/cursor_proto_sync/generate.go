package main

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	gengo "google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/pluginpb"
)

const goPackage = "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb;cursorpb"

// wellKnownFiles maps google.protobuf types to the files that define them.
var wellKnownFiles = map[string]protoreflect.FileDescriptor{
	"google.protobuf.Struct":    structpb.File_google_protobuf_struct_proto,
	"google.protobuf.Value":     structpb.File_google_protobuf_struct_proto,
	"google.protobuf.ListValue": structpb.File_google_protobuf_struct_proto,
	"google.protobuf.NullValue": structpb.File_google_protobuf_struct_proto,
	"google.protobuf.Timestamp": timestamppb.File_google_protobuf_timestamp_proto,
	"google.protobuf.Duration":  durationpb.File_google_protobuf_duration_proto,
	"google.protobuf.Empty":     emptypb.File_google_protobuf_empty_proto,
	"google.protobuf.Any":       anypb.File_google_protobuf_any_proto,
}

// Generate writes .proto files, .pb.go files and SNAPSHOT.txt into out.
func Generate(s *Schema, snap *Snapshot, msgs, enums []string, out string) error {
	files, err := BuildFiles(s, msgs, enums)
	if err != nil {
		return err
	}
	goFiles, err := generateGo(files)
	if err != nil {
		return err
	}
	if err = cleanGenerated(out); err != nil {
		return err
	}
	for _, f := range files {
		p := filepath.Join(out, "proto", filepath.FromSlash(f.GetName()))
		if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err = os.WriteFile(p, []byte(PrintProto(f)), 0o644); err != nil {
			return err
		}
	}
	for name, content := range goFiles {
		if err = os.WriteFile(filepath.Join(out, name), content, 0o644); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	if err = snap.Write(&buf); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "SNAPSHOT.txt"), buf.Bytes(), 0o644)
}

// cleanGenerated removes previously generated output, keeping hand-written files.
func cleanGenerated(out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(out, "proto")); err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(out, "*.pb.go"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err = os.Remove(m); err != nil {
			return err
		}
	}
	return nil
}

// protoFileFor names the .proto file for a type: <pkg path>/<module base>.proto.
func protoFileFor(fullName, module string) string {
	pkg := packageOf(fullName)
	base := path.Base(module)
	base = strings.TrimSuffix(base, ".js")
	base = strings.TrimSuffix(base, "_pb")
	return strings.ReplaceAll(pkg, ".", "/") + "/" + base + ".proto"
}

// packageOf returns the proto package of a fully qualified name ("agent.v1.Foo.Bar" -> "agent.v1").
func packageOf(fullName string) string {
	parts := strings.Split(fullName, ".")
	for i, p := range parts {
		if p != "" && unicode.IsUpper(rune(p[0])) {
			return strings.Join(parts[:i], ".")
		}
	}
	return ""
}

// BuildFiles turns the selected schema subset into validated file descriptors,
// sorted so that dependencies come first.
func BuildFiles(s *Schema, msgs, enums []string) ([]*descriptorpb.FileDescriptorProto, error) {
	type fileBuild struct {
		fd       *descriptorpb.FileDescriptorProto
		messages map[string]*descriptorpb.DescriptorProto
		deps     map[string]bool
	}
	files := map[string]*fileBuild{}
	fileOfType := map[string]string{}
	topLevel := func(full string) string {
		// Walk up to the outermost message for nested types.
		for {
			parent := s.parentMessage(full)
			if parent == "" {
				return full
			}
			full = parent
		}
	}
	getFile := func(full string) *fileBuild {
		top := topLevel(full)
		module := ""
		if m, ok := s.Messages[top]; ok {
			module = m.File
		} else if e, ok := s.Enums[top]; ok {
			module = e.File
		}
		name := protoFileFor(top, module)
		fb, ok := files[name]
		if !ok {
			fb = &fileBuild{
				fd: &descriptorpb.FileDescriptorProto{
					Name:    proto.String(name),
					Package: proto.String(packageOf(top)),
					Syntax:  proto.String("proto3"),
					Options: &descriptorpb.FileOptions{GoPackage: proto.String(goPackage)},
				},
				messages: map[string]*descriptorpb.DescriptorProto{},
				deps:     map[string]bool{},
			}
			files[name] = fb
		}
		fileOfType[full] = name
		return fb
	}

	// Messages first, sorted so parents exist before nested children.
	sortedMsgs := append([]string(nil), msgs...)
	sort.Slice(sortedMsgs, func(i, j int) bool {
		di, dj := strings.Count(sortedMsgs[i], "."), strings.Count(sortedMsgs[j], ".")
		if di != dj {
			return di < dj
		}
		return sortedMsgs[i] < sortedMsgs[j]
	})
	for _, name := range sortedMsgs {
		fb := getFile(name)
		dp := &descriptorpb.DescriptorProto{Name: proto.String(name[strings.LastIndexByte(name, '.')+1:])}
		fb.messages[name] = dp
		if parent := s.parentMessage(name); parent != "" {
			pd := fb.messages[parent]
			pd.NestedType = append(pd.NestedType, dp)
		} else {
			fb.fd.MessageType = append(fb.fd.MessageType, dp)
		}
	}
	for _, name := range enums {
		fb := getFile(name)
		e := s.Enums[name]
		ed := &descriptorpb.EnumDescriptorProto{Name: proto.String(name[strings.LastIndexByte(name, '.')+1:])}
		for _, v := range e.Values {
			ed.Value = append(ed.Value, &descriptorpb.EnumValueDescriptorProto{Name: proto.String(v.Name), Number: proto.Int32(v.Number)})
		}
		if len(e.Values) == 0 || e.Values[0].Number != 0 {
			return nil, fmt.Errorf("enum %s: first value must be 0 in proto3", name)
		}
		if parent := s.parentMessage(name); parent != "" {
			pd := fb.messages[parent]
			pd.EnumType = append(pd.EnumType, ed)
		} else {
			fb.fd.EnumType = append(fb.fd.EnumType, ed)
		}
	}

	wktDeps := map[string]protoreflect.FileDescriptor{}
	typeRef := func(fb *fileBuild, ft FieldType) (descriptorpb.FieldDescriptorProto_Type, string, error) {
		var full string
		var kind descriptorpb.FieldDescriptorProto_Type
		switch {
		case ft.Message != "":
			full, kind = ft.Message, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		case ft.Enum != "":
			full, kind = ft.Enum, descriptorpb.FieldDescriptorProto_TYPE_ENUM
		default:
			if _, ok := scalarNames[ft.Scalar]; !ok {
				return 0, "", fmt.Errorf("unknown scalar type %d", ft.Scalar)
			}
			return descriptorpb.FieldDescriptorProto_Type(ft.Scalar), "", nil
		}
		if wk, ok := wellKnownFiles[full]; ok {
			wktDeps[wk.Path()] = wk
			fb.deps[wk.Path()] = true
		} else if dep, ok := fileOfType[full]; ok {
			if dep != fb.fd.GetName() {
				fb.deps[dep] = true
			}
		} else {
			return 0, "", fmt.Errorf("type %s is not generated", full)
		}
		return kind, "." + full, nil
	}

	for _, name := range sortedMsgs {
		m := s.Messages[name]
		fb := files[fileOfType[name]]
		dp := fb.messages[name]
		oneofIdx := map[string]int32{}
		var synthetic []string
		for _, f := range m.Fields {
			fd := &descriptorpb.FieldDescriptorProto{
				Name:     proto.String(f.Name),
				Number:   proto.Int32(f.Number),
				JsonName: proto.String(jsonName(f.Name)),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			}
			kind, typeName, err := typeRef(fb, f.Type)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", name, f.Name, err)
			}
			if f.MapKey != 0 {
				entry := &descriptorpb.DescriptorProto{
					Name:    proto.String(mapEntryName(f.Name)),
					Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
					Field: []*descriptorpb.FieldDescriptorProto{
						{Name: proto.String("key"), Number: proto.Int32(1), JsonName: proto.String("key"), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_Type(f.MapKey).Enum()},
						{Name: proto.String("value"), Number: proto.Int32(2), JsonName: proto.String("value"), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: kind.Enum()},
					},
				}
				if typeName != "" {
					entry.Field[1].TypeName = proto.String(typeName)
				}
				dp.NestedType = append(dp.NestedType, entry)
				fd.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
				fd.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
				fd.TypeName = proto.String("." + name + "." + entry.GetName())
				dp.Field = append(dp.Field, fd)
				continue
			}
			fd.Type = kind.Enum()
			if typeName != "" {
				fd.TypeName = proto.String(typeName)
			}
			switch {
			case f.Label == LabelRepeated:
				fd.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
			case f.Oneof != "":
				idx, ok := oneofIdx[f.Oneof]
				if !ok {
					idx = int32(len(dp.OneofDecl))
					oneofIdx[f.Oneof] = idx
					dp.OneofDecl = append(dp.OneofDecl, &descriptorpb.OneofDescriptorProto{Name: proto.String(f.Oneof)})
				}
				fd.OneofIndex = proto.Int32(idx)
			case f.Label == LabelOptional:
				fd.Proto3Optional = proto.Bool(true)
				synthetic = append(synthetic, f.Name)
			}
			dp.Field = append(dp.Field, fd)
		}
		// Synthetic oneofs for proto3 optional fields come after real oneofs, as protoc emits them.
		for _, fname := range synthetic {
			idx := int32(len(dp.OneofDecl))
			dp.OneofDecl = append(dp.OneofDecl, &descriptorpb.OneofDescriptorProto{Name: proto.String("_" + fname)})
			for _, fd := range dp.Field {
				if fd.GetName() == fname {
					fd.OneofIndex = proto.Int32(idx)
				}
			}
		}
	}

	names := sortedKeys(files)
	byName := map[string]*descriptorpb.FileDescriptorProto{}
	for _, n := range names {
		fb := files[n]
		fb.fd.Dependency = sortedKeys(fb.deps)
		byName[n] = fb.fd
	}
	for p, wk := range wktDeps {
		byName[p] = protodesc.ToFileDescriptorProto(wk)
	}

	// Topological order: dependencies before dependents.
	var ordered []*descriptorpb.FileDescriptorProto
	state := map[string]int{}
	var visit func(n string, stack []string) error
	visit = func(n string, stack []string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("import cycle: %s -> %s", strings.Join(stack, " -> "), n)
		case 2:
			return nil
		}
		state[n] = 1
		for _, d := range byName[n].GetDependency() {
			if err := visit(d, append(stack, n)); err != nil {
				return err
			}
		}
		state[n] = 2
		ordered = append(ordered, byName[n])
		return nil
	}
	all := sortedKeys(byName)
	for _, n := range all {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	if _, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: ordered}); err != nil {
		return nil, fmt.Errorf("validate descriptors: %w", err)
	}
	return ordered, nil
}

// generateGo runs protoc-gen-go in-process and returns file name -> content.
func generateGo(files []*descriptorpb.FileDescriptorProto) (map[string][]byte, error) {
	req := &pluginpb.CodeGeneratorRequest{ProtoFile: files}
	for _, f := range files {
		if !strings.HasPrefix(f.GetName(), "google/") {
			req.FileToGenerate = append(req.FileToGenerate, f.GetName())
		}
	}
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		return nil, err
	}
	for _, f := range plugin.Files {
		if !f.Generate {
			continue
		}
		// Flat names in one Go package: agent/v1/exec.proto -> agent_v1_exec.pb.go.
		f.GeneratedFilenamePrefix = strings.ReplaceAll(strings.TrimSuffix(f.Desc.Path(), ".proto"), "/", "_")
		gengo.GenerateFile(plugin, f)
	}
	resp := plugin.Response()
	if resp.Error != nil {
		return nil, fmt.Errorf("protoc-gen-go: %s", resp.GetError())
	}
	out := map[string][]byte{}
	for _, f := range resp.File {
		out[f.GetName()] = []byte(f.GetContent())
	}
	return out, nil
}

func jsonName(name string) string {
	var b strings.Builder
	upper := false
	for _, r := range name {
		if r == '_' {
			upper = true
			continue
		}
		if upper {
			b.WriteRune(unicode.ToUpper(r))
			upper = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func mapEntryName(field string) string {
	j := jsonName(field)
	if j == "" {
		return "Entry"
	}
	return strings.ToUpper(j[:1]) + j[1:] + "Entry"
}
