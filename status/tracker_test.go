package status

import (
	"podsync"
	"testing"
)

func TestReportClearDismiss(t *testing.T) {
	tr := New()
	tr.Report("a", podsync.LevelWarning, "warn")
	tr.Report("b", podsync.LevelError, "boom")
	tr.Report("b", podsync.LevelError, "boom again")
	act := tr.Active()
	if len(act) != 2 || act[0].Key != "b" || act[0].Count != 2 || act[0].Message != "boom again" {
		t.Fatalf("active: %+v", act)
	}
	tr.Dismiss("b")
	if len(tr.Active()) != 1 {
		t.Fatal("dismissed issue should be hidden")
	}
	tr.Report("b", podsync.LevelError, "back")
	if len(tr.Active()) != 2 {
		t.Fatal("re-reported issue should reappear")
	}
	tr.Clear("a")
	tr.ClearPrefix("b")
	if len(tr.Active()) != 0 {
		t.Fatal("expected empty")
	}
}
