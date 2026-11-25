package trustedproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchParsesCIDRList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# comment\n\n173.245.48.0/20\n103.21.244.0/22\n  2400:cb00::/32  \nnot-a-cidr\n1.2.3.4\n")
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), []string{srv.URL})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := []string{"173.245.48.0/20", "103.21.244.0/22", "2400:cb00::/32", "1.2.3.4"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFetchPartialFailure(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "10.0.0.0/8\n")
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	got, err := Fetch(context.Background(), []string{ok.URL, bad.URL})
	if err == nil {
		t.Fatal("expected error for the failing URL")
	}
	if len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Fatalf("partial success lost: got %v", got)
	}
}

func TestFetchEmpty(t *testing.T) {
	got, err := Fetch(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("Fetch(nil) = %v, %v", got, err)
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"10.0.0.0/8", "1.2.3.4", "::1", "2400:cb00::/32"} {
		if !valid(s) {
			t.Fatalf("valid(%q) = false", s)
		}
	}
	for _, s := range []string{"", "nope", "999.1.1.1", "10.0.0.0/33"} {
		if valid(s) {
			t.Fatalf("valid(%q) = true", s)
		}
	}
}

func TestCombineDedupes(t *testing.T) {
	got := Combine(
		[]string{"10.0.0.0/8", "  1.2.3.4 ", ""},
		[]string{"1.2.3.4", "192.168.0.0/16"},
		nil,
	)
	want := []string{"10.0.0.0/8", "1.2.3.4", "192.168.0.0/16"}
	if len(got) != len(want) {
		t.Fatalf("Combine = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Combine[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
