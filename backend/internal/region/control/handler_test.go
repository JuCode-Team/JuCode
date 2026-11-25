package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/region"
)

type fakeSnapshots struct{}

func (fakeSnapshots) UpstreamSnapshot(context.Context) ([]region.ProviderSnapshot, error) {
	return nil, nil
}

func (fakeSnapshots) AccountSnapshot(context.Context, string) (*region.AccountSnapshot, error) {
	return nil, region.ErrNotFound
}

type fakeLedger struct {
	grantErr     error
	releasedFrom string
	applied      int
}

func (f *fakeLedger) GrantLease(context.Context, region.CreditLeaseRequest) (*region.CreditLease, error) {
	return nil, f.grantErr
}

func (f *fakeLedger) ReleaseLease(_ context.Context, regionID string, _ uuid.UUID) error {
	f.releasedFrom = regionID
	return nil
}

func (f *fakeLedger) ApplyUsage(_ context.Context, events []region.UsageEvent) error {
	f.applied += len(events)
	return nil
}

const (
	tokenA = "token-a-0123456789abcdef0123456789abcdef"
	tokenB = "token-b-0123456789abcdef0123456789abcdef"
)

func newTestControl(t *testing.T, ledger Ledger) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(TokenAuth{tokenA: "edge-a", tokenB: "edge-b"}, fakeSnapshots{}, ledger).Mount(r.Group(region.APIPrefix))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

func apiError(t *testing.T, err error) *region.Error {
	t.Helper()
	var e *region.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v (%T); want *region.Error", err, err)
	}
	return e
}

// Every refusal crosses the wire as a status plus a code, and the client
// turns the code back into the sentinel the control plane used.
func TestHandlerErrorCodes(t *testing.T) {
	ledger := &fakeLedger{grantErr: region.ErrLeaseInsufficient}
	url := newTestControl(t, ledger)
	ctx := context.Background()
	req := region.CreditLeaseRequest{UserID: uuid.New(), RegionID: "edge-a", RequestID: "r1", RequestedCNY: "1"}

	_, err := region.NewClient(url, "wrong", nil).GrantLease(ctx, req)
	if e := apiError(t, err); e.Status != http.StatusUnauthorized || e.Code != region.CodeUnauthorized {
		t.Fatalf("bad token = %#v", e)
	}

	a := region.NewClient(url, tokenA, nil)
	_, err = a.GrantLease(ctx, req)
	if e := apiError(t, err); e.Status != http.StatusPaymentRequired || e.Code != region.CodeInsufficientBalance {
		t.Fatalf("insufficient = %#v", e)
	}
	if !errors.Is(err, region.ErrLeaseInsufficient) {
		t.Fatalf("errors.Is(%v, ErrLeaseInsufficient) = false", err)
	}

	ledger.grantErr = errors.New("db exploded with secret detail")
	_, err = a.GrantLease(ctx, req)
	if e := apiError(t, err); e.Status != http.StatusInternalServerError || e.Code != region.CodeInternal || e.Message != "internal error" {
		t.Fatalf("internal = %#v", e)
	}

	if _, err := a.FetchAccount(ctx, "00000000000000000000000000000000000000000000000000000000000000aa"); !errors.Is(err, region.ErrNotFound) {
		t.Fatalf("unknown key = %v; want ErrNotFound", err)
	}
}

// A token fixes the caller's region: edge-a cannot lease or report usage as
// edge-b, and a release is checked against the caller's region.
func TestHandlerBindsTokenToRegion(t *testing.T) {
	ledger := &fakeLedger{}
	url := newTestControl(t, ledger)
	ctx := context.Background()
	a := region.NewClient(url, tokenA, nil)

	_, err := a.GrantLease(ctx, region.CreditLeaseRequest{UserID: uuid.New(), RegionID: "edge-b", RequestID: "r1", RequestedCNY: "1"})
	if !errors.Is(err, region.ErrRegionMismatch) {
		t.Fatalf("lease for another region = %v; want ErrRegionMismatch", err)
	}
	err = a.PushUsage(ctx, []region.UsageEvent{{EventID: uuid.New(), RegionID: "edge-b", UserID: uuid.New(), CostCNY: "0"}})
	if !errors.Is(err, region.ErrRegionMismatch) || ledger.applied != 0 {
		t.Fatalf("usage for another region = %v, applied %d", err, ledger.applied)
	}
	if err := a.PushUsage(ctx, []region.UsageEvent{{EventID: uuid.New(), RegionID: "edge-a", UserID: uuid.New(), CostCNY: "0"}}); err != nil || ledger.applied != 1 {
		t.Fatalf("own usage = %v, applied %d", err, ledger.applied)
	}
	if err := a.ReleaseLease(ctx, uuid.New()); err != nil || ledger.releasedFrom != "edge-a" {
		t.Fatalf("release = %v from %q", err, ledger.releasedFrom)
	}
}
