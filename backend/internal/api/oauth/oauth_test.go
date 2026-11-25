package oauth

import "testing"

var testClients = Clients{
	IDs:             []string{"jucode-cli", "jucode-desktop", "jucode-web"},
	WebClientID:     "jucode-web",
	WebRedirectURIs: []string{"https://app.example.com/auth/callback"},
}

func TestClientsValid(t *testing.T) {
	for id, want := range map[string]bool{
		"jucode-cli": true, "jucode-desktop": true, "jucode-web": true,
		"jucode-mobile": false, "": false,
	} {
		if got := testClients.valid(id); got != want {
			t.Errorf("valid(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestAllowedRedirectWeb(t *testing.T) {
	for _, tc := range []struct {
		redirect string
		want     bool
	}{
		{"https://app.example.com/auth/callback", true},
		{"http://127.0.0.1:5173/auth/callback", true}, // loopback: local development
		{"https://app.example.com/auth/callback?x=1", false},
		{"https://app.example.com/remote", false},
		{"https://app.example.com.evil.com/auth/callback", false},
		{"jucode://oauth/callback", false},
	} {
		if got := testClients.allowedRedirect("jucode-web", tc.redirect); got != tc.want {
			t.Fatalf("allowedRedirect(jucode-web, %q) = %v, want %v", tc.redirect, got, tc.want)
		}
	}
	// Another client cannot use the web app's callback.
	if testClients.allowedRedirect("jucode-cli", "https://app.example.com/auth/callback") {
		t.Fatal("jucode-cli accepted the web callback")
	}
	if !testClients.allowedRedirect("jucode-cli", "http://127.0.0.1:8912/cb") {
		t.Fatal("jucode-cli rejected a loopback callback")
	}
	// Without configured callbacks only loopback is allowed.
	noWeb := testClients
	noWeb.WebRedirectURIs = nil
	if noWeb.allowedRedirect("jucode-web", "https://app.example.com/auth/callback") {
		t.Fatal("an unconfigured web callback was accepted")
	}
}
