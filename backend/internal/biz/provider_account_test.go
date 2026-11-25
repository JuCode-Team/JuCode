package biz

import (
	"testing"

	"github.com/google/uuid"
)

func TestCatalogProviderDefaultsMapsDialects(t *testing.T) {
	cases := []struct {
		catalogID string
		ok        bool
		kind      string
		protocol  string
	}{
		{catalogID: "anthropic", ok: true, kind: "anthropic", protocol: NativeProtocolMessages},
		{catalogID: "deepseek", ok: true, kind: "openai_compatible", protocol: NativeProtocolChat},
		{catalogID: "openai", ok: true, kind: "openai_compatible", protocol: NativeProtocolResponses},
		// Subscription-only providers are not in the catalog at all.
		{catalogID: "openai-codex", ok: false},
		{catalogID: "definitely-not-real", ok: false},
	}
	for _, tc := range cases {
		baseURL, kind, protocols, ok := CatalogProviderDefaults(tc.catalogID)
		if ok != tc.ok {
			t.Fatalf("%s: ok = %v want %v", tc.catalogID, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if baseURL == "" {
			t.Fatalf("%s: missing base url", tc.catalogID)
		}
		if kind != tc.kind || len(protocols) != 1 || protocols[0] != tc.protocol {
			t.Fatalf("%s: got kind=%s protocols=%v", tc.catalogID, kind, protocols)
		}
	}
}

func TestProviderAccountStickyKeysPreferConversation(t *testing.T) {
	userID, groupID := uuid.New(), uuid.New()
	keys := providerAccountStickyKeys(userID, groupID, " conv-1 ")
	if len(keys) != 1 || keys[0] != "acct:sticky:conv:conv-1" {
		t.Fatalf("unexpected conversation keys: %#v", keys)
	}
	keys = providerAccountStickyKeys(userID, groupID, "")
	want := "acct:sticky:ug:" + userID.String() + ":" + groupID.String()
	if len(keys) != 1 || keys[0] != want {
		t.Fatalf("unexpected user/group keys: %#v want %s", keys, want)
	}
	if keys := providerAccountStickyKeys(uuid.Nil, uuid.Nil, ""); len(keys) != 0 {
		t.Fatalf("no scope should produce no keys: %#v", keys)
	}
}
