package dagui

import (
	"context"
	"strings"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/dagger/dagger/dagql/call/callpbv1"
	"github.com/dagger/dagger/engine/telemetryattrs"
)

// The chain from the live failure this side channel exists for: focusing an
// agent needs llm.withSkills(directory: <dir>).agent(), where the withSkills
// frame is synthesized (LLM.recipeSelectors) and its directory argument is an
// ID literal whose own frame was never independently spanned. Only the agent
// frame gets a span; without the log channel the other two can never reach a
// client, and the chain is unrebuildable forever.
func callPayloadTestChain() (root *callpbv1.Call, unspanned []*callpbv1.Call) {
	dir := &callpbv1.Call{
		Digest: "xxh3:dir",
		Field:  "directory",
		Type:   &callpbv1.Type{NamedType: "Directory"},
		Args: []*callpbv1.Argument{{
			Name:  "path",
			Value: &callpbv1.Literal{Value: &callpbv1.Literal_String_{String_: "/skills"}},
		}},
	}
	withSkills := &callpbv1.Call{
		Digest: "xxh3:withSkills",
		Field:  "withSkills",
		Type:   &callpbv1.Type{NamedType: "LLM"},
		Args: []*callpbv1.Argument{{
			Name:  "directory",
			Value: &callpbv1.Literal{Value: &callpbv1.Literal_CallDigest{CallDigest: dir.Digest}},
		}},
	}
	agent := &callpbv1.Call{
		Digest:         "xxh3:agent",
		Field:          "agent",
		Type:           &callpbv1.Type{NamedType: "Agent"},
		ReceiverDigest: withSkills.Digest,
	}
	return agent, []*callpbv1.Call{withSkills, dir}
}

// newTestCallPayloadRecord builds the record the engine emits for one frame of
// a call's transitive closure.
func newTestCallPayloadRecord(t *testing.T, span SpanID, call *callpbv1.Call) sdklog.Record {
	t.Helper()
	payload, err := call.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", call.Digest, err)
	}
	return newTestLogRecord(trace.TraceID{1}, span.SpanID, "",
		otellog.String(telemetryattrs.DagCallPayloadDigestAttr, call.Digest),
		otellog.String(telemetryattrs.DagCallPayloadAttr, payload),
	)
}

func exportCallPayloads(t *testing.T, db *DB, span SpanID, calls ...*callpbv1.Call) {
	t.Helper()
	records := make([]sdklog.Record, 0, len(calls))
	for _, call := range calls {
		records = append(records, newTestCallPayloadRecord(t, span, call))
	}
	if err := db.LogExporter().Export(context.Background(), records); err != nil {
		t.Fatalf("export payload records: %v", err)
	}
}

// A payload that arrives ONLY over the log channel must resolve exactly like
// one that rode a span attribute: DB.Call is the single lookup both feed.
func TestIngestCallPayloadResolvesCallWithNoSpan(t *testing.T) {
	db := NewDB()
	_, unspanned := callPayloadTestChain()
	dir := unspanned[1]

	before := db.MutationCount()
	exportCallPayloads(t, db, spanID(1), dir)

	call := db.Call(dir.Digest)
	if call == nil {
		t.Fatal("payload arrived over the log channel but DB.Call cannot resolve it")
	}
	if call.Field != "directory" || call.GetType().GetNamedType() != "Directory" {
		t.Fatalf("decoded the wrong call: %+v", call)
	}
	// Memoized views (the agent roster among them) are derived from this
	// data, so a payload that leaves the mutation counter alone would be
	// invisible until some unrelated span happened to bump it.
	if db.MutationCount() == before {
		t.Error("ingesting a payload did not bump the mutation counter")
	}
}

// The record is call data, not output: it must be consumed before the log-text
// path, or every payload turns into a phantom log line on its span.
func TestIngestCallPayloadIsNotLogText(t *testing.T) {
	db := NewDB()
	_, unspanned := callPayloadTestChain()
	exportCallPayloads(t, db, spanID(1), unspanned...)

	if span := db.Spans.Map[spanID(1)]; span != nil && span.HasLogs {
		t.Error("payload record was treated as log text")
	}
	if got := len(db.PrimaryLogs); got != 0 {
		t.Errorf("payload record was buffered as a primary log: %d", got)
	}
}

// The acceptance criterion: a chain rebuilds through extractIntoDAG with no
// missing frames when only ONE of its frames ever got a span — and it does so
// whichever way the two pipelines happen to interleave. Spans and logs are
// separately batched, so neither order can be assumed.
func TestCallIDRebuildsFromLogPayloads(t *testing.T) {
	root, unspanned := callPayloadTestChain()
	rootPayload, err := root.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// Only the agent frame is spanned, exactly as the engine emits it today.
	spanned := []SpanSnapshot{{
		ID:          spanID(1),
		Name:        "LLM.agent",
		CallDigest:  root.Digest,
		CallPayload: rootPayload,
	}}

	for _, tc := range []struct {
		name         string
		payloadFirst bool
	}{
		{"payload before span", true},
		{"payload after span", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := NewDB()
			if tc.payloadFirst {
				exportCallPayloads(t, db, spanID(1), unspanned...)
				db.ImportSnapshots(spanned)
			} else {
				db.ImportSnapshots(spanned)
				exportCallPayloads(t, db, spanID(1), unspanned...)
			}

			span := db.Spans.Map[spanID(1)]
			if span == nil {
				t.Fatal("span not ingested")
			}
			id, err := span.CallID()
			if err != nil {
				t.Fatalf("chain not rebuildable: %v", err)
			}
			if got := id.Digest().String(); got != root.Digest {
				t.Errorf("rebuilt ID digest = %q, want %q", got, root.Digest)
			}
			// The frame behind the ID-literal argument is the one the span
			// channel structurally cannot deliver, so it is the one worth
			// asserting made it into the rebuilt chain.
			if display := id.Display(); !strings.Contains(display, "directory") {
				t.Errorf("rebuilt ID lost the ID-literal argument's frame: %s", display)
			}
		})
	}
}

// Control: the same chain WITHOUT the log channel must fail, and name the gap.
// Without this, the test above could pass for reasons unrelated to the payloads
// it exports.
func TestCallIDWithoutLogPayloadsStillReportsTheGap(t *testing.T) {
	root, _ := callPayloadTestChain()
	rootPayload, err := root.Encode()
	if err != nil {
		t.Fatal(err)
	}
	db := NewDB()
	db.ImportSnapshots([]SpanSnapshot{{
		ID:          spanID(1),
		Name:        "LLM.agent",
		CallDigest:  root.Digest,
		CallPayload: rootPayload,
	}})

	if _, err := db.Spans.Map[spanID(1)].CallID(); err == nil {
		t.Fatal("expected the unspanned receiver to be reported as missing")
	} else if !strings.Contains(err.Error(), "xxh3:withSkills") {
		t.Fatalf("gap report does not name the missing frame: %v", err)
	}
}
