package idtui

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dagger/dagger/dagql/call/callpbv1"
	"github.com/dagger/dagger/dagql/dagui"
	"github.com/stretchr/testify/require"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/vito/tuist"
)

// The client half of roster focus (hack/designs/async-agents.md §5.1): who a
// submitted message goes to, what Ctrl-C preempts, and what a keypress does to
// the roster. The rule under test throughout is that all three follow FOCUS,
// never "whatever turn happens to be running".

// focusShellHandler is a ShellHandler that also implements the routing and
// focus contract the frontend probes for.
type focusShellHandler struct {
	stubShellHandler

	mu         sync.Mutex
	absorb     bool
	serial     bool
	submitted  []string
	interrupts int
	handled    []string
	target     string
	focused    []string
	focusErr   error
	queued     string
}

func (h *focusShellHandler) SubmitToTarget(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.absorb {
		return false
	}
	h.submitted = append(h.submitted, msg)
	return true
}

func (h *focusShellHandler) InterruptTarget() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.interrupts++
	return true
}

func (h *focusShellHandler) Serial(string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.serial
}

func (h *focusShellHandler) TargetAgentID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.target
}

func (h *focusShellHandler) FocusAgent(_ context.Context, agentID, _, encodedID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.focusErr != nil {
		return h.focusErr
	}
	if encodedID == "" {
		panic("focus without a rebuilt handle")
	}
	h.focused = append(h.focused, agentID)
	h.target = agentID
	return nil
}

func (h *focusShellHandler) Handle(_ context.Context, input string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handled = append(h.handled, input)
	return nil
}

func (h *focusShellHandler) QueueMessage(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queued = msg
}

func (h *focusShellHandler) DequeueMessage() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	msg := h.queued
	h.queued = ""
	return msg
}

func (h *focusShellHandler) snapshot() (submitted, handled, focused []string, interrupts int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.submitted...),
		append([]string(nil), h.handled...),
		append([]string(nil), h.focused...),
		h.interrupts
}

// focusTestFrontend brings up a headless shell frontend around handler.
func focusTestFrontend(t *testing.T, db *dagui.DB, handler *focusShellHandler) *frontendPretty {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	term := tuist.NewHeadlessTerminal(120, 10)
	fe := newWithTerminal(io.Discard, db, term)
	fe.setupTUI()
	fe.startShell(context.Background(), handler)
	fe.tui.Step()
	return fe
}

func pressEditlineKey(t *testing.T, fe *frontendPretty, key uv.Key) bool {
	t.Helper()
	return fe.interceptEditlineKey(tuist.Context{}, uv.KeyPressEvent(key))
}

// TestSubmitAsksTheTargetFirst covers the routing order: the focused
// conversation's own in-flight turn absorbs the message, and only when there
// is nothing to absorb it does the frontend open a new turn.
func TestSubmitAsksTheTargetFirst(t *testing.T) {
	handler := &focusShellHandler{absorb: true}
	fe := focusTestFrontend(t, dagui.NewDB(), handler)

	fe.textInput.SetValue("steer the focused agent")
	fe.handleInputComplete()

	submitted, handled, _, _ := handler.snapshot()
	require.Equal(t, []string{"steer the focused agent"}, submitted)
	require.Empty(t, handled, "an absorbed message must not open a turn")

	// With nothing to absorb it, the same message opens a turn instead.
	handler.mu.Lock()
	handler.absorb = false
	handler.mu.Unlock()

	fe.textInput.SetValue("open a new turn")
	fe.handleInputComplete()
	require.Eventually(t, func() bool {
		_, handled, _, _ := handler.snapshot()
		return len(handled) == 1 && handled[0] == "open a new turn"
	}, 5*time.Second, 10*time.Millisecond)
}

// TestMessageQueuesOnlyBehindASerialTurn: a prompt turn no longer blocks the
// client, so only the handler's single interpreter -- a shell command or a
// /command -- makes a submission wait. Before the split ANY running turn did,
// which with a roster meant you could not speak to a second agent at all.
func TestMessageQueuesOnlyBehindASerialTurn(t *testing.T) {
	handler := &focusShellHandler{serial: true}
	fe := focusTestFrontend(t, dagui.NewDB(), handler)

	// A serial turn is in flight.
	fe.serialRunning = true
	fe.textInput.SetValue("wait your turn")
	fe.handleInputComplete()

	_, handled, _, _ := handler.snapshot()
	require.Empty(t, handled)
	require.Equal(t, "wait your turn", fe.queuedMsgLabel.Message())

	// Nothing serial running: the message opens its own turn immediately,
	// even though another (prompt) turn may still be live.
	fe.serialRunning = false
	fe.turnsRunning = 1
	fe.textInput.SetValue("straight through")
	fe.handleInputComplete()
	require.Eventually(t, func() bool {
		_, handled, _, _ := handler.snapshot()
		return len(handled) == 1 && handled[0] == "straight through"
	}, 5*time.Second, 10*time.Millisecond)
}

// TestCtrlCPreemptsTheFocusedAgent: Ctrl-C is an explicit interrupt addressed
// at the focused agent's runtime, not a cancel re-pointed at whichever turn
// holds the client. A serial turn is the exception -- that turn IS the client.
func TestCtrlCPreemptsTheFocusedAgent(t *testing.T) {
	handler := &focusShellHandler{}
	fe := focusTestFrontend(t, dagui.NewDB(), handler)

	var canceled int
	fe.shellInterrupt = func(error) { canceled++ }

	pressEditlineKey(t, fe, uv.Key{Code: 'c', Mod: uv.ModCtrl})
	_, _, _, interrupts := handler.snapshot()
	require.Equal(t, 1, interrupts)
	require.Zero(t, canceled, "the focused agent is interrupted server-side")

	// A shell command owns the client, so Ctrl-C cancels it as before.
	fe.serialRunning = true
	pressEditlineKey(t, fe, uv.Key{Code: 'c', Mod: uv.ModCtrl})
	_, _, _, interrupts = handler.snapshot()
	require.Equal(t, 1, interrupts, "no agent is interrupted while a shell command runs")
	require.Equal(t, 1, canceled)
}

// rosterDB builds a trace with two agents, each with a loop span carrying its
// identity plus the (internal) call span whose payload a client rebuilds the
// agent's handle from.
func rosterDB(t *testing.T) *dagui.DB {
	t.Helper()
	db := dagui.NewDB()
	start := time.Unix(100, 0)
	traceID := prettyTestTraceID()

	for i, agent := range []struct {
		name   string
		id     string
		digest string
		span   byte
		call   byte
	}{
		{"chief", "agent-chief", "sha256:chief", 1, 2},
		{"scout", "agent-scout", "sha256:scout", 3, 4},
	} {
		db.Calls[agent.digest] = &callpbv1.Call{
			Digest: agent.digest,
			Field:  "agent",
			Type:   &callpbv1.Type{NamedType: "Agent"},
		}
		db.ImportSnapshots([]dagui.SpanSnapshot{
			{
				ID:        prettyTestSpanID(agent.span),
				TraceID:   traceID,
				Name:      "agent: " + agent.name,
				StartTime: start.Add(time.Duration(i) * time.Second),
				Agent:     true,
				AgentID:   agent.id,
				AgentName: agent.name,
				// The identity the loop span publishes, including the digest
				// of the call that produced the agent value.
				AgentCallDigest: agent.digest,
				AgentState:      "IDLE",
			},
			{
				ID:         prettyTestSpanID(agent.call),
				TraceID:    traceID,
				Name:       "agent(id:)",
				StartTime:  start.Add(time.Duration(i) * time.Second),
				CallDigest: agent.digest,
			},
		})
	}
	return db
}

// TestFocusKeyRetargetsAndKeepsDrafts covers the switcher: a numbered jump
// retargets the prompt through a handle rebuilt from the trace, the
// half-typed line is parked against the agent being left, and the
// last-focused toggle brings it back.
func TestFocusKeyRetargetsAndKeepsDrafts(t *testing.T) {
	handler := &focusShellHandler{target: "agent-chief"}
	fe := focusTestFrontend(t, rosterDB(t), handler)

	entries := fe.agentRosterEntries()
	require.Len(t, entries, 2)
	require.True(t, entries[0].Focused, "the session's own agent starts focused")
	require.False(t, entries[1].Focused)

	// Half a sentence to the chief, then jump to the scout.
	fe.textInput.SetValue("half a thought")
	require.True(t, pressEditlineKey(t, fe, uv.Key{Code: '2', Mod: uv.ModAlt}))
	require.Eventually(t, func() bool {
		_, _, focused, _ := handler.snapshot()
		return len(focused) == 1 && focused[0] == "agent-scout"
	}, 5*time.Second, 10*time.Millisecond)
	fe.tui.Step()

	require.Equal(t, "", fe.textInput.Value(), "the scout has no draft yet")
	entries = fe.agentRosterEntries()
	require.True(t, entries[1].Focused, "focus follows the handler's target")

	// Type at the scout, then toggle back to the chief: each draft returns to
	// the agent it was meant for.
	fe.textInput.SetValue("for the scout")
	require.True(t, pressEditlineKey(t, fe, uv.Key{Code: 'l', Mod: uv.ModAlt}))
	require.Eventually(t, func() bool {
		_, _, focused, _ := handler.snapshot()
		return len(focused) == 2 && focused[1] == "agent-chief"
	}, 5*time.Second, 10*time.Millisecond)
	fe.tui.Step()
	require.Equal(t, "half a thought", fe.textInput.Value())

	require.True(t, pressEditlineKey(t, fe, uv.Key{Code: '2', Mod: uv.ModAlt}))
	require.Eventually(t, func() bool {
		_, _, focused, _ := handler.snapshot()
		return len(focused) == 3
	}, 5*time.Second, 10*time.Millisecond)
	fe.tui.Step()
	require.Equal(t, "for the scout", fe.textInput.Value())
}

// TestUnaddressableAgentIsReadOnly: an agent whose handle cannot be rebuilt
// from this client's trace can be watched, not spoken to. The failure must be
// loud and the entry marked, rather than a focus that silently goes nowhere.
func TestUnaddressableAgentIsReadOnly(t *testing.T) {
	db := rosterDB(t)
	// Drop the scout's call payload: the frame never reached this client.
	delete(db.Calls, "sha256:scout")

	handler := &focusShellHandler{target: "agent-chief"}
	fe := focusTestFrontend(t, db, handler)

	require.True(t, pressEditlineKey(t, fe, uv.Key{Code: '2', Mod: uv.ModAlt}))
	_, _, focused, _ := handler.snapshot()
	require.Empty(t, focused, "focus must not move to an agent with no handle")
	require.Error(t, fe.promptErr)

	entries := fe.agentRosterEntries()
	require.True(t, entries[1].ReadOnly)
	require.True(t, entries[0].Focused, "focus stays where it was")

	// And the strip says so, rather than rendering a normal-looking entry.
	fe.agentRoster.Update()
	frame := strings.Join(fe.tui.Step(), "\n")
	require.Contains(t, frame, "scout·")
}
