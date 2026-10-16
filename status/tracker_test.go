package status

import (
	"podsync"
	"testing"
)

func TestReportAndClear(t *testing.T) {
	tr := New()
	tr.Report("a", podsync.LevelWarning, "warn")
	tr.Report("b", podsync.LevelError, "boom")
	tr.Report("b", podsync.LevelError, "boom again")
	act := tr.Active()
	if len(act) != 2 || act[0].Key != "b" || act[0].Count != 2 || act[0].Message != "boom again" {
		t.Fatalf("active: %+v", act)
	}
	tr.Clear("a")
	tr.ClearPrefix("b")
	if len(tr.Active()) != 0 {
		t.Fatal("expected empty")
	}
}
