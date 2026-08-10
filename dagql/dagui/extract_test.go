package dagui

import (
	"strings"
	"testing"

	"github.com/dagger/dagger/dagql/call/callpbv1"
)

// A chain is only rebuildable if EVERY frame it references has a call payload
// in this client's DB, including the frames buried inside ID-literal
// arguments. When one is absent the walk used to drop it silently and let
// call.ID.decode fail later with a bare "call digest %q not found", wrapped
// once per frame it unwound through -- a wall of identical wrappers naming a
// digest, but never the frame that referenced it, on chains that in practice
// run a dozen frames deep.
func TestExtractIntoDAGReportsMissingFrames(t *testing.T) {
	// llm(...).withTools(object: <missing>.staff()).agent(...)
	//
	// The gap sits behind an argument rather than the receiver spine, which is
	// the case the bare-digest error was least able to explain.
	db := NewDB()
	db.Calls = map[string]*callpbv1.Call{
		"xxh3:root": {
			Digest:         "xxh3:root",
			Field:          "agent",
			Type:           &callpbv1.Type{NamedType: "Agent"},
			ReceiverDigest: "xxh3:withTools",
		},
		"xxh3:withTools": {
			Digest: "xxh3:withTools",
			Field:  "withTools",
			Type:   &callpbv1.Type{NamedType: "LLM"},
			Args: []*callpbv1.Argument{{
				Name: "object",
				Value: &callpbv1.Literal{
					Value: &callpbv1.Literal_CallDigest{CallDigest: "xxh3:staff"},
				},
			}},
		},
		"xxh3:staff": {
			Digest: "xxh3:staff",
			Field:  "staff",
			Type:   &callpbv1.Type{NamedType: "Staff"},
			// Its receiver's span never reached this client.
			ReceiverDigest: "xxh3:gone",
		},
	}

	recipe := &callpbv1.RecipeDAG{
		RootDigest:    "xxh3:root",
		CallsByDigest: map[string]*callpbv1.Call{},
	}
	missing := extractIntoDAG(recipe, db, "xxh3:root")

	if len(missing) != 1 {
		t.Fatalf("expected exactly the one unresolvable reference, got %v", missing)
	}
	if missing[0].digest != "xxh3:gone" {
		t.Errorf("missing call digest = %q, want xxh3:gone", missing[0].digest)
	}
	// The referrer is the whole value of the report: it says which frame to go
	// looking for, rather than leaving the reader with a digest and no handle.
	if want := `receiver of "staff" (Staff)`; missing[0].via != want {
		t.Errorf("missing call via = %q, want %q", missing[0].via, want)
	}
	if msg := missing[0].String(); !strings.Contains(msg, "xxh3:gone") ||
		!strings.Contains(msg, `"staff"`) {
		t.Errorf("rendered message names neither the digest nor the frame: %q", msg)
	}

	// Everything reachable is still collected: a gap degrades the result, it
	// does not abandon the walk.
	for _, dgst := range []string{"xxh3:root", "xxh3:withTools", "xxh3:staff"} {
		if _, ok := recipe.CallsByDigest[dgst]; !ok {
			t.Errorf("resolvable call %s missing from the recipe", dgst)
		}
	}
}

// The ordinary case must stay silent: a complete chain reports nothing, so
// callers can treat a non-empty result as a real failure.
func TestExtractIntoDAGCompleteChain(t *testing.T) {
	db := NewDB()
	db.Calls = map[string]*callpbv1.Call{
		"xxh3:root": {
			Digest:         "xxh3:root",
			Field:          "agent",
			Type:           &callpbv1.Type{NamedType: "Agent"},
			ReceiverDigest: "xxh3:llm",
		},
		"xxh3:llm": {
			Digest: "xxh3:llm",
			Field:  "llm",
			Type:   &callpbv1.Type{NamedType: "LLM"},
		},
	}

	recipe := &callpbv1.RecipeDAG{
		RootDigest:    "xxh3:root",
		CallsByDigest: map[string]*callpbv1.Call{},
	}
	if missing := extractIntoDAG(recipe, db, "xxh3:root"); len(missing) != 0 {
		t.Fatalf("complete chain reported missing frames: %v", missing)
	}
	if len(recipe.CallsByDigest) != 2 {
		t.Fatalf("expected both frames in the recipe, got %d", len(recipe.CallsByDigest))
	}
}
