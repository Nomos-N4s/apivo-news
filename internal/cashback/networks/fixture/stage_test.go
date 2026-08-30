// The tests for stage.go: the four names, and the clock's one rule - it
// advances the stage a read actually finished, and nothing else.

package fixture

import (
	"sync"
	"testing"
)

func TestStageValidAndString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		stage     Stage
		wantValid bool
		wantName  string
	}{
		{stage: StageClick, wantValid: true, wantName: "click"},
		{stage: StagePending, wantValid: true, wantName: "pending"},
		{stage: StageApproved, wantValid: true, wantName: "approved"},
		{stage: StageReversed, wantValid: true, wantName: "reversed"},
		{stage: Stage(-1), wantValid: false, wantName: "stage(-1)"},
		{stage: Stage(stageCount), wantValid: false, wantName: "stage(4)"},
	}

	for _, tc := range tests {
		t.Run(tc.wantName, func(t *testing.T) {
			t.Parallel()
			if got := tc.stage.Valid(); got != tc.wantValid {
				t.Errorf("Stage(%d).Valid() = %t, want %t", int(tc.stage), got, tc.wantValid)
			}
			if got := tc.stage.String(); got != tc.wantName {
				t.Errorf("Stage(%d).String() = %q, want %q", int(tc.stage), got, tc.wantName)
			}
		})
	}
}

// TestStageCountFollowsTheConstants keeps the number the recording is checked
// against derived from the lifecycle rather than written beside the files.
// The two are one fact stated twice - the poll a name describes and the file
// that plays it - and recording_test.go is where they are held together.
func TestStageCountFollowsTheConstants(t *testing.T) {
	t.Parallel()

	if stageCount != int(StageReversed)+1 {
		t.Fatalf("stageCount = %d, want %d", stageCount, int(StageReversed)+1)
	}
	if !Stage(stageCount-1).Valid() || Stage(stageCount).Valid() {
		t.Errorf("stageCount = %d does not name the last valid stage", stageCount)
	}
}

func TestStageClockAdvancesOnACompletedRead(t *testing.T) {
	t.Parallel()

	var clock stageClock
	for want := StagePending; want <= StageReversed; want++ {
		clock.advanceFrom(want - 1)
		if got := clock.now(); got != want {
			t.Fatalf("after advancing from %s the clock reads %s, want %s", want-1, got, want)
		}
	}
}

// TestStageClockStopsAtTheLastObservation holds the end of the recording. A
// network goes on reporting a reversal rather than forgetting it, so a poller
// that keeps running must keep finding the final answer instead of falling
// off the end of the recording and panicking.
func TestStageClockStopsAtTheLastObservation(t *testing.T) {
	t.Parallel()

	clock := stageClock{at: StageReversed}
	for range 3 {
		clock.advanceFrom(StageReversed)
	}
	if got := clock.now(); got != StageReversed {
		t.Errorf("the clock reads %s after advancing past the last observation, want %s", got, StageReversed)
	}
}

// TestStageClockIgnoresAReaderOfAnOlderStage is the rule that makes
// resumability safe when two callers overlap. A read that started at the
// click poll and finished after another caller had already walked the
// lifecycle on must not drag the clock back to where its own answer came
// from: the second caller has seen the later observations, and moving the
// clock backwards would serve them again as if they were new.
func TestStageClockIgnoresAReaderOfAnOlderStage(t *testing.T) {
	t.Parallel()

	clock := stageClock{at: StageApproved}
	clock.advanceFrom(StageClick)
	if got := clock.now(); got != StageApproved {
		t.Errorf("a completed read of %s moved the clock to %s; it was already at %s, and only the stage a read actually finished may advance it",
			StageClick, got, StageApproved)
	}
}

// TestStageClockAdvancesOnceUnderConcurrentReads is the race detector's test
// as much as the logic's: two pollers finishing the same observation at the
// same moment must move the lifecycle on by one poll, not by two.
func TestStageClockAdvancesOnceUnderConcurrentReads(t *testing.T) {
	t.Parallel()

	const readers = 8
	var clock stageClock
	var wg sync.WaitGroup
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			clock.advanceFrom(StageClick)
		}()
	}
	wg.Wait()

	if got := clock.now(); got != StagePending {
		t.Errorf("%d concurrent reads of %s left the clock at %s, want %s", readers, StageClick, got, StagePending)
	}
}

func TestStageClockSetMovesToAnyRecordedStage(t *testing.T) {
	t.Parallel()

	var clock stageClock
	clock.set(StageApproved)
	if got := clock.now(); got != StageApproved {
		t.Errorf("clock.now() = %s after set(%s)", got, StageApproved)
	}
}
