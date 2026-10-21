// Package cursorfixture stores recorded, redacted agent.v1.AgentService/Run
// streams as golden fixtures, so Cursor provider tests replay real protocol
// traffic and `go test` never talks to Cursor.
//
// # Format (cursor-run-fixture/v1)
//
// One JSON file per Run under testdata/runs/<name>.json:
//
//	{
//	  "format": "cursor-run-fixture/v1",
//	  "agent_version": "2026.10.19-6bbf2c4",   // agent CLI the client version header claimed
//	  "model": "composer-2.5",
//	  "source": "recorded",                    // or "reconstructed" (hand-built from notes)
//	  "description": "what the run exercises",
//	  "notes": "optional caveats",
//	  "frames": [
//	    {"dir": "client", "at_ms": 0,  "msg": {...AgentClientMessage as protojson...}},
//	    {"dir": "server", "at_ms": 98, "msg": {...AgentServerMessage as protojson...}}
//	  ],
//	  "end_stream": {"error": {"code": "canceled", "message": "..."}}  // optional Connect end-stream JSON
//	}
//
// Messages are protojson with proto field names, so fixtures stay readable and
// reviewable; the loader turns them back into cursorpb types or Connect-framed
// bytes. Every message passes through Redactor before it is stored: tokens,
// emails, home paths and the machine's user/host names are replaced. KV blob
// ids are content hashes and will not match redacted blob data.
//
// Recording: wrap the wire client so each sent/received message goes to a
// Recorder, then Fixture().Write(path). Review the diff before committing; the
// package tests fail if any committed fixture still matches a secret pattern.
package cursorfixture

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/cursorpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Format is the fixture format identifier.
const Format = "cursor-run-fixture/v1"

// Frame directions.
const (
	DirClient = "client" // proxy -> Cursor (AgentClientMessage)
	DirServer = "server" // Cursor -> proxy (AgentServerMessage)
)

// Fixture is one recorded Run.
type Fixture struct {
	Format       string          `json:"format"`
	AgentVersion string          `json:"agent_version"`
	Model        string          `json:"model"`
	Source       string          `json:"source"`
	Description  string          `json:"description"`
	Notes        string          `json:"notes,omitempty"`
	Frames       []Frame         `json:"frames"`
	EndStream    json.RawMessage `json:"end_stream,omitempty"`
}

// Frame is one message on the Run stream.
type Frame struct {
	Dir  string          `json:"dir"`
	AtMs int64           `json:"at_ms"`
	Msg  json.RawMessage `json:"msg"`
}

var (
	marshalOpts   = protojson.MarshalOptions{UseProtoNames: true}
	unmarshalOpts = protojson.UnmarshalOptions{}
)

// Load reads and validates a fixture; every frame must decode.
func Load(path string) (*Fixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := &Fixture{}
	if err = json.Unmarshal(raw, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Format != Format {
		return nil, fmt.Errorf("%s: format %q, want %q", path, f.Format, Format)
	}
	if _, err = f.Client(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err = f.Server(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Client returns the client frames in order.
func (f *Fixture) Client() ([]*cursorpb.AgentClientMessage, error) {
	var out []*cursorpb.AgentClientMessage
	for i, fr := range f.Frames {
		switch fr.Dir {
		case DirClient:
			m := &cursorpb.AgentClientMessage{}
			if err := unmarshalOpts.Unmarshal(fr.Msg, m); err != nil {
				return nil, fmt.Errorf("frame %d: %w", i, err)
			}
			out = append(out, m)
		case DirServer:
		default:
			return nil, fmt.Errorf("frame %d: bad dir %q", i, fr.Dir)
		}
	}
	return out, nil
}

// Server returns the server frames in order.
func (f *Fixture) Server() ([]*cursorpb.AgentServerMessage, error) {
	var out []*cursorpb.AgentServerMessage
	for i, fr := range f.Frames {
		if fr.Dir != DirServer {
			continue
		}
		m := &cursorpb.AgentServerMessage{}
		if err := unmarshalOpts.Unmarshal(fr.Msg, m); err != nil {
			return nil, fmt.Errorf("frame %d: %w", i, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// ServerConnectStream returns the server side as a Connect streaming body:
// each message framed as flags(0) + uint32 length + payload, then the
// end-stream frame (flags 0x02) carrying EndStream or "{}".
func (f *Fixture) ServerConnectStream() ([]byte, error) {
	msgs, err := f.Server()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for _, m := range msgs {
		payload, errMarshal := proto.Marshal(m)
		if errMarshal != nil {
			return nil, errMarshal
		}
		writeConnectFrame(&buf, 0, payload)
	}
	end := []byte("{}")
	if len(f.EndStream) > 0 {
		end = f.EndStream
	}
	writeConnectFrame(&buf, 0x02, end)
	return buf.Bytes(), nil
}

func writeConnectFrame(buf *bytes.Buffer, flags byte, payload []byte) {
	var hdr [5]byte
	hdr[0] = flags
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	buf.Write(hdr[:])
	buf.Write(payload)
}

// Write stores the fixture as indented JSON.
func (f *Fixture) Write(path string) error {
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// Recorder builds a fixture from live traffic, redacting every message.
type Recorder struct {
	// ElideBlobs replaces KV blob payloads (set_blob data, get_blob results)
	// with "[elided N bytes]". Blobs hold Cursor's own harness prompt and
	// rendered context, are large, and replay tests never read them.
	ElideBlobs bool

	fx    Fixture
	start time.Time
	now   func() time.Time
	red   *Redactor
}

// NewRecorder starts a recording. now may be nil (time.Now).
func NewRecorder(agentVersion, model, description string, red *Redactor, now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	if red == nil {
		red = NewRedactor(DefaultLiterals()...)
	}
	red.Allow(model)
	return &Recorder{
		fx:    Fixture{Format: Format, AgentVersion: agentVersion, Model: model, Source: "recorded", Description: description},
		start: now(),
		now:   now,
		red:   red,
	}
}

// Client records a message sent to Cursor.
func (r *Recorder) Client(m *cursorpb.AgentClientMessage) error { return r.add(DirClient, m) }

// Server records a message received from Cursor.
func (r *Recorder) Server(m *cursorpb.AgentServerMessage) error { return r.add(DirServer, m) }

// EndStream records the Connect end-stream JSON (already free of secrets).
func (r *Recorder) EndStream(raw []byte) {
	r.fx.EndStream = json.RawMessage(r.red.String(string(raw)))
}

func (r *Recorder) add(dir string, m proto.Message) error {
	c := proto.Clone(m)
	r.red.Message(c)
	if r.ElideBlobs {
		elideBlobs(c)
	}
	raw, err := marshalOpts.Marshal(c)
	if err != nil {
		return err
	}
	// protojson output whitespace is deliberately unstable; normalize it.
	var compact bytes.Buffer
	if err = json.Compact(&compact, raw); err != nil {
		return err
	}
	r.fx.Frames = append(r.fx.Frames, Frame{Dir: dir, AtMs: r.now().Sub(r.start).Milliseconds(), Msg: compact.Bytes()})
	return nil
}

func elideBlobs(m proto.Message) {
	elide := func(b []byte) []byte { return []byte(fmt.Sprintf("[elided %d bytes]", len(b))) }
	switch v := m.(type) {
	case *cursorpb.AgentServerMessage:
		if set := v.GetKvServerMessage().GetSetBlobArgs(); set != nil {
			set.BlobData = elide(set.BlobData)
		}
	case *cursorpb.AgentClientMessage:
		if get := v.GetKvClientMessage().GetGetBlobResult(); get != nil && get.BlobData != nil {
			get.BlobData = elide(get.BlobData)
		}
	}
}

// Fixture returns the recording so far.
func (r *Recorder) Fixture() *Fixture { return &r.fx }
