package agent

// SessionTelemetry is the per-session counter snapshot the eval
// harness records into run records — the stub-track and
// notebook-recall splits that a flat count can't express.
type SessionTelemetry struct {
	StubInvalidations int   `json:"invalidations"`
	StubResults       int   `json:"results"`
	StubSavedBytes    int64 `json:"saved_bytes"`
	BoundaryAdvances  int   `json:"boundary_advances"`
	ResultRecalls     int   `json:"result_recalls"`
	EntryRecalls      int   `json:"entry_recalls"`
	EmptyRecalls      int   `json:"empty_recalls"`
	CrossRecalls      int   `json:"cross_recalls"`
}

// SessionTelemetry returns the coordinator's per-session counters.
// Deliberately not on the Coordinator interface — the eval harness
// type-asserts for it so test stubs needn't implement it. Zero value
// when the agent is absent or the session has no accumulated counters.
func (c *coordinator) SessionTelemetry(sessionID string) SessionTelemetry {
	sa, ok := c.currentAgent.(*sessionAgent)
	if !ok || sa == nil {
		return SessionTelemetry{}
	}
	var t SessionTelemetry
	if s, ok := sa.stubStats.Get(sessionID); ok {
		t.StubInvalidations = s.Invalidations
		t.StubResults = s.Results
		t.StubSavedBytes = s.SavedBytes
		t.BoundaryAdvances = s.BoundaryAdvances
	}
	if n, ok := sa.nbStats.Get(sessionID); ok {
		t.ResultRecalls = n.ResultRecalls
		t.EntryRecalls = n.EntryRecalls
		t.EmptyRecalls = n.EmptyRecalls
		t.CrossRecalls = n.CrossRecalls
	}
	return t
}
