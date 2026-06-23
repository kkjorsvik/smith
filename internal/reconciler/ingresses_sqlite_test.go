package reconciler

import (
	"path/filepath"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

func TestIngressStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	st, err := NewIngressStore(dbPath)
	if err != nil {
		t.Fatalf("NewIngressStore: %v", err)
	}

	if err := st.Add(types.Ingress{Host: "git.kkjorsvik.com", Service: "forgejo"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.Add(types.Ingress{Host: "ci.kkjorsvik.com", Service: "woodpecker"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Validation.
	if err := st.Add(types.Ingress{Host: "x.kkjorsvik.com"}); err == nil {
		t.Fatal("expected error for empty service")
	}
	if err := st.Add(types.Ingress{Service: "y"}); err == nil {
		t.Fatal("expected error for empty host")
	}

	// Update by host is idempotent on the key, changes the target.
	if err := st.Add(types.Ingress{Host: "git.kkjorsvik.com", Service: "forgejo-v2"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	list, err := st.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]string{}
	for _, ing := range list {
		got[ing.Host] = ing.Service
	}
	if len(got) != 2 || got["git.kkjorsvik.com"] != "forgejo-v2" || got["ci.kkjorsvik.com"] != "woodpecker" {
		t.Fatalf("list = %+v", got)
	}

	if err := st.Remove("ci.kkjorsvik.com"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	list, _ = st.List()
	if len(list) != 1 || list[0].Host != "git.kkjorsvik.com" {
		t.Fatalf("after remove, list = %+v", list)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Persistence across reopen.
	st2, err := NewIngressStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	list, _ = st2.List()
	if len(list) != 1 || list[0].Service != "forgejo-v2" {
		t.Fatalf("after reopen, list = %+v", list)
	}
}
