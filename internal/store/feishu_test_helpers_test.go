package store

import "testing"

func openSharedFeishuTestStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	first, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("open first shared Feishu store: %v", err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Errorf("close first shared Feishu store: %v", err)
		}
	})
	second, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("open second shared Feishu store: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close second shared Feishu store: %v", err)
		}
	})
	return first, second
}
