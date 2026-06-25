package game

import (
	"testing"
	"time"
)

func TestMsSinceClamps(t *testing.T) {
	armedAt := time.Now()
	if got := msSince(armedAt, armedAt.Add(-5*time.Millisecond)); got != 0 {
		t.Errorf("negative offset: want 0, got %d", got)
	}
	if got := msSince(armedAt, armedAt.Add(100*time.Second)); got != 65535 {
		t.Errorf("overflow offset: want 65535, got %d", got)
	}
	if got := msSince(armedAt, armedAt.Add(42*time.Millisecond)); got != 42 {
		t.Errorf("normal offset: want 42, got %d", got)
	}
}

// takePending drains the board mutations accumulated since the last call, carrying the
// claimer tag and the replacement, so each tick ships only the new ones.
func TestBoardTakePending(t *testing.T) {
	b := newBoard(5, time.Now())
	seq := uint64(1)
	b.mint = func() Button {
		seq++
		return b.register(seq, int16(seq), int16(seq))
	}
	live := b.mint()
	b.offer(ClickEvent{Tag: "aaaa", Nonce: live.Nonce, At: time.Now()})

	got := b.takePending()
	if len(got) != 1 || got[0].ClaimerTag != "aaaa" || got[0].Spawn == nil {
		t.Fatalf("unexpected pending: %+v", got)
	}
	if b.takePending() != nil {
		t.Fatal("pending should be drained after takePending")
	}
}
