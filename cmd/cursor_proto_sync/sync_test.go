package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBundle mimics the agent bundle layout: a compact runtime module, two
// proto modules in different packages, a well-known type and a connect service.
const fakeBundle = `(()=>{var __webpack_modules__={` +
	`"../wkt/struct_pb.js"(t,e,r){r.d(e,{V:()=>a});class a extends i.Q{}a.typeName="google.protobuf.Value"},` +
	`"../proto/dist/generated/agent/v1/b_pb.js"(t,e,r){"use strict";r.d(e,{Q:()=>p,M:()=>g});` +
	`var n=r("../proto3.js"),l=r("../proto/dist/runtime/compact.js"),w=r("../wkt/struct_pb.js");const d="agent.v1.";` +
	`class m extends l.HL{static get runtime(){return(0,l.kU)(this,"runtime",n.C)}static $p(){return d}}` +
	`var p=(0,l.QT)(n.C,d,"ThingKind",[[0,"UNSPECIFIED"],[1,"BIG"]],1);` +
	`class g extends m{static $(){return["Thing|1 name 9|2 kind #0|3 tags 9*|4 note 9?|5 attrs 9,#1|6 text 9 body|7 nested #2 body",p,w.V,h]}}` +
	`class h extends m{static $(){return["Thing.Inner|1 ok 8"]}}` +
	`},` +
	`"../proto/dist/generated/agent/v1/a_pb.js"(t,e,r){"use strict";r.d(e,{R:()=>q,S:()=>s});` +
	`var n=r("../proto3.js"),l=r("../proto/dist/runtime/compact.js"),c=(r("../x.js"),r("../proto/dist/generated/agent/v1/b_pb.js"));const d="agent.v1.";` +
	`class m extends l.HL{static get runtime(){return(0,l.kU)(this,"runtime",n.C)}static $p(){return d}}` +
	`class q extends m{static $(){return["Req|1 thing #0|2 count 13",c.M]}}` +
	`class s extends m{static $(){return["Resp"]}}` +
	`const D="other.v1.";class N extends l.HL{static get runtime(){return(0,l.kU)(this,"runtime",n.C)}static $p(){return D}}` +
	`class o extends N{static $(){return["Elsewhere|1 x 5"]}}` +
	`},` +
	`"./src/client.ts"(t,e,r){var _=r("../proto/dist/generated/agent/v1/a_pb.js");` +
	`const b={typeName:"agent.v1.FakeService",methods:{call:{name:"Call",I:_.R,O:_.S,kind:u.I.BiDiStreaming}}}` +
	`}}})();`

func loadFake(t *testing.T) *Schema {
	t.Helper()
	s, err := parseBundle([]string{"index.js"}, map[string]string{"index.js": fakeBundle})
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}
	if len(s.Warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", s.Warnings)
	}
	return s
}

func TestParseBundleResolvesTypes(t *testing.T) {
	s := loadFake(t)
	thing := s.Messages["agent.v1.Thing"]
	if thing == nil {
		t.Fatalf("Thing missing; have %v", sortedKeys(s.Messages))
	}
	want := []string{
		"1 name string",
		"2 kind agent.v1.ThingKind",
		"3 tags string repeated",
		"4 note string optional",
		"5 attrs map<string,google.protobuf.Value>",
		"6 text string oneof=body",
		"7 nested agent.v1.Thing.Inner oneof=body",
	}
	snap := BuildSnapshot(s, "v", []string{"agent.v1.Thing"}, nil)
	var got []string
	for _, f := range snap.Messages["agent.v1.Thing"] {
		got = append(got, f.String())
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("fields:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if e := s.Enums["agent.v1.ThingKind"]; e == nil || e.Values[1].Name != "THING_KIND_BIG" {
		t.Fatalf("enum prefix not applied: %+v", e)
	}
	if s.Messages["other.v1.Elsewhere"] == nil {
		t.Fatal("second package in a concatenated module not resolved")
	}
	if req := s.Messages["agent.v1.Req"]; req == nil || req.Fields[0].Type.Message != "agent.v1.Thing" {
		t.Fatalf("cross-module ref via comma import not resolved: %+v", req)
	}
	svc := s.Services["agent.v1.FakeService"]
	if svc == nil || svc.Methods[0].Input != "agent.v1.Req" || !svc.Methods[0].ClientStreaming {
		t.Fatalf("service not resolved: %+v", svc)
	}
}

func TestClosureAndGenerate(t *testing.T) {
	s := loadFake(t)
	msgs, enums, err := s.Closure([]string{"agent.v1.FakeService/Call"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(msgs, ",") != "agent.v1.Req,agent.v1.Resp,agent.v1.Thing,agent.v1.Thing.Inner" {
		t.Fatalf("closure = %v", msgs)
	}
	if strings.Join(enums, ",") != "agent.v1.ThingKind" {
		t.Fatalf("enums = %v", enums)
	}
	files, err := BuildFiles(s, msgs, enums)
	if err != nil {
		t.Fatalf("BuildFiles: %v", err)
	}
	goFiles, err := generateGo(files)
	if err != nil {
		t.Fatalf("generateGo: %v", err)
	}
	if _, ok := goFiles["agent_v1_b.pb.go"]; !ok {
		t.Fatalf("generated files: %v", sortedKeys(goFiles))
	}
	var protoText string
	for _, f := range files {
		if f.GetName() == "agent/v1/b.proto" {
			protoText = PrintProto(f)
		}
	}
	for _, want := range []string{"map<string, .google.protobuf.Value> attrs = 5;", "optional string note = 4;", "oneof body {", "message Inner {"} {
		if !strings.Contains(protoText, want) {
			t.Errorf("proto text missing %q:\n%s", want, protoText)
		}
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := loadFake(t)
	msgs, enums, _ := s.Closure([]string{"agent.v1.FakeService/Call"})
	snap := BuildSnapshot(s, "2026.01.01-abc", msgs, enums)
	snap.RPCs = RPCLines(s, []string{"agent.v1.FakeService/Call"})
	var buf bytes.Buffer
	if err := snap.Write(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := ParseSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err = back.Write(&again); err != nil {
		t.Fatal(err)
	}
	if buf.String() != again.String() {
		t.Fatalf("round trip differs:\n%s\n---\n%s", buf.String(), again.String())
	}
	if back.RPCs["agent.v1.FakeService/Call"] != "agent.v1.Req agent.v1.Resp bidi" {
		t.Fatalf("rpc line = %q", back.RPCs["agent.v1.FakeService/Call"])
	}
}

func snapOf(fields map[string][]SnapField) *Snapshot {
	return &Snapshot{AgentVersion: "v", RPCs: map[string]string{}, Messages: fields, Enums: map[string][]EnumValue{}}
}

func TestDiffClassification(t *testing.T) {
	old := snapOf(map[string][]SnapField{
		"agent.v1.Used": {
			{Number: 1, Name: "kept", Type: "string"},
			{Number: 2, Name: "listed_gone", Type: "string"},
			{Number: 3, Name: "unlisted_gone", Type: "string"},
			{Number: 4, Name: "retyped", Type: "string"},
			{Number: 5, Name: "moved", Type: "string"},
		},
		execServerMessage: {{Number: 1, Name: "id", Type: "uint32"}},
		"agent.v1.Unused": {{Number: 1, Name: "x", Type: "string"}},
	})
	old.RPCs["agent.v1.S/Run"] = "agent.v1.A agent.v1.B bidi"
	cur := snapOf(map[string][]SnapField{
		"agent.v1.Used": {
			{Number: 1, Name: "kept", Type: "string"},
			{Number: 4, Name: "retyped", Type: "bytes"},
			{Number: 9, Name: "moved", Type: "string"},
			{Number: 10, Name: "fresh", Type: "bool"},
		},
		execServerMessage: {{Number: 1, Name: "id", Type: "uint32"}, {Number: 2, Name: "teleport_args", Type: "agent.v1.TeleportArgs", Oneof: "message"}},
		"agent.v1.Unused": {{Number: 1, Name: "x", Type: "int32"}},
	})
	cur.RPCs["agent.v1.S/Run"] = "agent.v1.A agent.v1.C bidi"
	used := map[string][]string{"agent.v1.Used": {"kept", "listed_gone"}, execServerMessage: {"id"}}

	rep := Diff(old, cur, used)
	wantBreaking := []string{
		"rpc agent.v1.S/Run changed: agent.v1.A agent.v1.B bidi -> agent.v1.A agent.v1.C bidi",
		"agent.v1.Used.listed_gone (#2) removed",
		"agent.v1.Used #4 retyped: type string -> bytes",
		"agent.v1.Used.moved renumbered 5 -> 9",
	}
	if strings.Join(rep.Breaking, "\n") != strings.Join(wantBreaking, "\n") {
		t.Fatalf("breaking:\n%s\nwant:\n%s", strings.Join(rep.Breaking, "\n"), strings.Join(wantBreaking, "\n"))
	}
	if !rep.HasBreaking() {
		t.Fatal("HasBreaking = false")
	}
	if len(rep.NewExecTypes) != 1 || !strings.HasPrefix(rep.NewExecTypes[0], "teleport_args (#2") {
		t.Fatalf("new exec types = %v", rep.NewExecTypes)
	}
	changes := strings.Join(rep.Changes, "\n")
	for _, want := range []string{"agent.v1.Used.unlisted_gone (#3) removed", "agent.v1.Unused #1 x: type string -> int32", "agent.v1.Used: new field 10 fresh bool"} {
		if !strings.Contains(changes, want) {
			t.Errorf("changes missing %q:\n%s", want, changes)
		}
	}

	if rep = Diff(old, old, used); rep.HasBreaking() || len(rep.Changes) != 0 || len(rep.NewExecTypes) != 0 {
		t.Fatalf("identical snapshots reported changes: %+v", rep)
	}
}

// The used list must only name messages and fields that exist in the committed snapshot.
func TestUsedListMatchesCommittedSnapshot(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", defaultOut, "SNAPSHOT.txt"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ParseSnapshot(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if missing := snap.MissingUsed(used); len(missing) > 0 {
		t.Fatalf("used list names unknown messages/fields: %v", missing)
	}
	for _, r := range roots {
		if snap.RPCs[r] == "" {
			t.Errorf("root rpc %s missing from snapshot", r)
		}
	}
}

func TestEnumPrefix(t *testing.T) {
	for in, want := range map[string]string{"ThingKind": "THING_KIND_", "MCPState": "M_C_P_STATE_", "A": "A_", "Http2Config": "HTTP2_CONFIG_"} {
		if got := enumPrefix(in); got != want {
			t.Errorf("enumPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
