package game

import (
	"testing"
	"time"
)

func TestSamplePipsFiltersAndCaps(t *testing.T) {
	armedAt := time.Now()
	mk := func(tag string, pos bool, ms int) ClickEvent {
		return ClickEvent{Tag: tag, HasPos: pos, X: 1, Y: 2, At: armedAt.Add(time.Duration(ms) * time.Millisecond)}
	}

	// No positioned clicks ⇒ no pips (older clients send no x/y).
	if got := samplePips([]ClickEvent{mk("a", false, 5)}, armedAt, 4); got != nil {
		t.Fatalf("want nil for no-position clicks, got %v", got)
	}
	// k<=0 disables sampling.
	if got := samplePips([]ClickEvent{mk("a", true, 5)}, armedAt, 0); got != nil {
		t.Fatalf("want nil for k=0, got %v", got)
	}

	// Under the cap: every positioned click survives (the unpositioned one is dropped),
	// in order, with t_arm measured from the arm.
	in := []ClickEvent{mk("a", true, 10), mk("b", false, 20), mk("c", true, 30)}
	got := samplePips(in, armedAt, 4)
	if len(got) != 2 {
		t.Fatalf("want 2 positioned pips, got %d", len(got))
	}
	if got[0].Tag != "a" || got[0].TArmMs != 10 || got[1].Tag != "c" || got[1].TArmMs != 30 {
		t.Fatalf("unexpected pips: %+v", got)
	}

	// Over the cap: sampled down to exactly k.
	many := make([]ClickEvent, 20)
	for i := range many {
		many[i] = mk("x", true, i)
	}
	if got := samplePips(many, armedAt, 8); len(got) != 8 {
		t.Fatalf("want 8 sampled, got %d", len(got))
	}
}

func TestSamplePipsClampsTArm(t *testing.T) {
	armedAt := time.Now()
	neg := ClickEvent{Tag: "a", HasPos: true, At: armedAt.Add(-5 * time.Millisecond)}
	big := ClickEvent{Tag: "b", HasPos: true, At: armedAt.Add(100 * time.Second)}
	got := samplePips([]ClickEvent{neg, big}, armedAt, 4)
	if got[0].TArmMs != 0 {
		t.Errorf("negative offset: want t_arm 0, got %d", got[0].TArmMs)
	}
	if got[1].TArmMs != 65535 {
		t.Errorf("overflow offset: want t_arm 65535, got %d", got[1].TArmMs)
	}
}
