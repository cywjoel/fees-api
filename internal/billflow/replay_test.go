package billflow_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/worker"

	"fees-api/internal/billflow"
)

// TestReplayCommittedHistories replays real workflow histories against the
// current workflow code.
//
// This is the guard that matters most for a workflow with a month-long lifetime.
// Bills stay open across deploys, so new code will routinely be asked to resume
// executions that older code started. Temporal reconstructs a running workflow by
// replaying its history through the current code, and if that code now issues a
// different sequence of commands - an activity moved, a timer added, a branch
// reordered - replay diverges and the execution is stuck with a
// non-determinism error.
//
// The failure would otherwise appear at deploy time, on live bills, rather than
// in CI. These fixtures were exported from real executions with:
//
//	temporal workflow show --workflow-id <id> --output json
//
// Refresh them deliberately when the workflow's structure is meant to change;
// never to make a red test go green.
func TestReplayCommittedHistories(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("finding fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no history fixtures committed under testdata/; the replay guard would silently check nothing")
	}

	for _, path := range fixtures {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(billflow.BillWorkflow)

			if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, path); err != nil {
				t.Fatalf("replaying %s against the current workflow failed: %v\n\n"+
					"This means the workflow's command sequence changed. Any bill still "+
					"open would fail to resume after a deploy. Either restore the previous "+
					"sequence, or make the change behind a workflow.GetVersion branch.", path, err)
			}
		})
	}
}

// The fixtures must actually exercise the workflow, not merely parse. A history
// with no activities or no updates would replay trivially and prove nothing.
func TestFixturesCoverTheWorkflowsBehaviour(t *testing.T) {
	fixtures, _ := filepath.Glob(filepath.Join("testdata", "*.json"))

	for _, path := range fixtures {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			counts := eventCounts(t, path)

			for _, required := range []string{
				"EVENT_TYPE_WORKFLOW_EXECUTION_STARTED",
				"EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_ACCEPTED",
				"EVENT_TYPE_ACTIVITY_TASK_COMPLETED",
				"EVENT_TYPE_TIMER_STARTED",
				"EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED",
			} {
				if counts[required] == 0 {
					t.Errorf("fixture has no %s; it does not exercise the workflow meaningfully", required)
				}
			}

			// Persist, emit, finalize.
			if got, want := counts["EVENT_TYPE_ACTIVITY_TASK_COMPLETED"], 3; got != want {
				t.Errorf("completed activities = %d, want %d", got, want)
			}
		})
	}
}

// A rejected update never enters workflow history. The bill closed by request was
// driven through six update requests - three additions, one identical retry, one
// conflicting reuse of an item id, and the close - and only five were accepted.
//
// That absence is the design working: business refusals belong in the validator,
// where they cost nothing and leave no permanent record, rather than in the
// handler, where every refusal would be written into the bill's history forever.
func TestRejectedUpdatesLeaveNoTraceInHistory(t *testing.T) {
	path := filepath.Join("testdata", "bill_closed_by_request.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not present: %v", err)
	}

	counts := eventCounts(t, path)

	if got, want := counts["EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_ACCEPTED"], 5; got != want {
		t.Errorf("accepted updates = %d, want %d (3 additions, 1 identical retry, 1 close)", got, want)
	}
	if got := counts["EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_REJECTED"]; got != 0 {
		t.Errorf("rejected updates recorded in history = %d, want 0: a validator rejection should leave no trace", got)
	}
}

func eventCounts(t *testing.T, path string) map[string]int {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var doc struct {
		Events []struct {
			EventType string `json:"eventType"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}

	counts := make(map[string]int)
	for _, e := range doc.Events {
		counts[e.EventType]++
	}
	return counts
}
