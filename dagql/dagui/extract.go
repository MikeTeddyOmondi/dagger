package dagui

import (
	"fmt"

	"github.com/dagger/dagger/dagql/call/callpbv1"
)

// missingCall records a call the walk could not resolve, together with the
// frame that referenced it.
//
// The referrer is the whole point. extractIntoDAG only ever visits digests
// something points at, so a gap is always reachable from some frame — and
// without naming that frame the eventual failure is a bare digest the reader
// has no way to look up, on a chain that can be a dozen frames deep.
type missingCall struct {
	digest string
	// via describes how the missing call was reached, e.g.
	//	receiver of "withWorkspace" (LLM)
	//	argument "object" of "withTools" (LLM)
	// Empty for the root of the walk, which nothing referenced.
	via string
}

func (m missingCall) String() string {
	if m.via == "" {
		return fmt.Sprintf("call %s", m.digest)
	}
	return fmt.Sprintf("call %s, referenced as %s", m.digest, m.via)
}

// extractIntoDAG recursively populates recipe.CallsByDigest from the call and
// its dependencies, and returns the references it could not resolve.
//
// Unresolvable references are reported rather than raised: a partial recipe is
// still worth rendering, and only callers that need a loadable ID care. Those
// callers must check, because the alternative is what this used to do —
// silently omit the frame and let call.ID.decode fail much later with
// `call digest %q not found`, wrapped once per frame it unwound through,
// naming the digest but never the frame that wanted it. This is the last place
// that still knows the context, so it is where the context gets recorded.
func extractIntoDAG(recipe *callpbv1.RecipeDAG, db *DB, callDigest string) []missingCall {
	x := &dagExtractor{recipe: recipe, db: db}
	x.extractCall(callDigest, "")
	return x.missing
}

type dagExtractor struct {
	recipe  *callpbv1.RecipeDAG
	db      *DB
	missing []missingCall
}

func (x *dagExtractor) extractCall(callDigest, via string) {
	if callDigest == "" {
		return
	}
	if _, exists := x.recipe.CallsByDigest[callDigest]; exists {
		return
	}

	call := x.db.Call(callDigest)
	if call == nil {
		x.missing = append(x.missing, missingCall{digest: callDigest, via: via})
		return
	}
	call = &callpbv1.Call{
		ReceiverDigest: call.ReceiverDigest,
		Type:           call.Type,
		Field:          call.Field,
		Args:           call.Args,
		Nth:            call.Nth,
		Module:         call.Module,
		Digest:         callDigest,
		View:           call.View,
	}
	x.recipe.CallsByDigest[callDigest] = call

	if call.ReceiverDigest != "" {
		x.extractCall(call.ReceiverDigest, "receiver of "+frameLabel(call))
	}
	for _, arg := range call.Args {
		if arg.Value != nil {
			x.extractLit(arg.Value, fmt.Sprintf("argument %q of %s", arg.GetName(), frameLabel(call)))
		}
	}
	if call.Module != nil && call.Module.CallDigest != "" {
		x.extractCall(call.Module.CallDigest, "module of "+frameLabel(call))
	}
}

// extractLit recursively extracts calls from literals, carrying the referring
// frame's label down so a call buried in a list or object argument still
// reports where it came from.
func (x *dagExtractor) extractLit(lit *callpbv1.Literal, via string) {
	switch v := lit.Value.(type) {
	case *callpbv1.Literal_CallDigest:
		x.extractCall(v.CallDigest, via)
	case *callpbv1.Literal_List:
		if v.List != nil {
			for _, val := range v.List.Values {
				x.extractLit(val, via)
			}
		}
	case *callpbv1.Literal_Object:
		if v.Object != nil {
			for _, val := range v.Object.Values {
				if val.Value != nil {
					x.extractLit(val.Value, via)
				}
			}
		}
	default:
		// Other literal types do not reference calls, so ignore.
	}
}

// frameLabel names a call the way a reader can find it in the trace: the field
// selected, and the type it returned.
func frameLabel(call *callpbv1.Call) string {
	field := call.GetField()
	if field == "" {
		field = "?"
	}
	if typ := call.GetType().GetNamedType(); typ != "" {
		return fmt.Sprintf("%q (%s)", field, typ)
	}
	return fmt.Sprintf("%q", field)
}
