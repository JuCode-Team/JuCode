package idgen

import (
	"strings"
	"testing"
)

func TestAPIKeySecretShape(t *testing.T) {
	full, prefix, err := NewAPIKeySecret()
	if err != nil {
		t.Fatalf("NewAPIKeySecret: %v", err)
	}
	if !strings.HasPrefix(full, "sk-juc-") {
		t.Fatalf("full = %q, want sk-juc- prefix", full)
	}
	if !strings.HasPrefix(prefix, "sk-juc-") || len(prefix) != len("sk-juc-")+8 {
		t.Fatalf("prefix = %q, want sk-juc- + 8 chars", prefix)
	}
	if !strings.HasPrefix(full, prefix) {
		t.Fatalf("prefix %q is not a prefix of full %q", prefix, full)
	}
	// 32 bytes → 43 base64url chars; shell-safe means no '/' or '+'.
	tail := strings.TrimPrefix(full, "sk-juc-")
	if len(tail) != 43 {
		t.Fatalf("tail len = %d, want 43", len(tail))
	}
	if strings.ContainsAny(tail, "/+") {
		t.Fatalf("tail %q contains shell-unsafe chars", tail)
	}
}

func TestManagementSecretShape(t *testing.T) {
	full, prefix, err := NewManagementSecret()
	if err != nil {
		t.Fatalf("NewManagementSecret: %v", err)
	}
	if !strings.HasPrefix(full, "jcm-") || !strings.HasPrefix(full, prefix) {
		t.Fatalf("full=%q prefix=%q", full, prefix)
	}
}

func TestAPIKeyAndManagementSecretsDiffer(t *testing.T) {
	a, _, _ := NewAPIKeySecret()
	m, _, _ := NewManagementSecret()
	if a == m {
		t.Fatal("api key and management secret collided")
	}
	if strings.HasPrefix(m, "sk-juc-") || strings.HasPrefix(a, "jcm-") {
		t.Fatal("key kinds are confusable by prefix")
	}
}

func TestJTIShape(t *testing.T) {
	j, err := NewJTI()
	if err != nil {
		t.Fatalf("NewJTI: %v", err)
	}
	if len(j) != 32 { // 16 bytes hex
		t.Fatalf("jti len = %d, want 32", len(j))
	}
	for _, r := range j {
		if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') {
			t.Fatalf("jti %q has non-hex char %q", j, r)
		}
	}
}

func TestUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for range 100 {
		full, _, err := NewAPIKeySecret()
		if err != nil {
			t.Fatalf("NewAPIKeySecret: %v", err)
		}
		if _, dup := seen[full]; dup {
			t.Fatalf("duplicate key %q", full)
		}
		seen[full] = struct{}{}
		j, err := NewJTI()
		if err != nil {
			t.Fatalf("NewJTI: %v", err)
		}
		if _, dup := seen[j]; dup {
			t.Fatalf("duplicate jti %q", j)
		}
		seen[j] = struct{}{}
	}
}
