package edge

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/region"
)

func TestFileOutboxRoundTrip(t *testing.T) {
	o, err := NewFileOutbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	event := region.UsageEvent{EventID: uuid.New(), RequestID: "req-1", RegionID: "edge-1", UserID: uuid.New(), CostCNY: "0.12", OccurredAt: time.Now()}
	if err := o.Enqueue(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	got, err := o.Pending(context.Background(), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("pending = %#v, %v", got, err)
	}
	if got[0].EventID != event.EventID || got[0].CostCNY != event.CostCNY {
		t.Fatalf("event changed: %#v", got[0])
	}
	if err := o.Ack(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, err = o.Pending(context.Background(), 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("after ack = %#v, %v", got, err)
	}
}
