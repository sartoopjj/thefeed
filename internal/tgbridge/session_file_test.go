package tgbridge

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// A read during a save gets the old session or the new one, never part of one.
func TestSessionSaveIsAllOrNothing(t *testing.T) {
	f := SessionFile{Path: filepath.Join(t.TempDir(), "session.json")}
	a, b := bytes.Repeat([]byte("a"), 64<<10), bytes.Repeat([]byte("b"), 64<<10)
	if err := f.StoreSession(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			data := a
			if i%2 == 1 {
				data = b
			}
			if err := f.StoreSession(context.Background(), data); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		got, err := f.LoadSession(context.Background())
		if err != nil || !(bytes.Equal(got, a) || bytes.Equal(got, b)) {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d bytes mid-save (err %v)", len(got), err)
		}
	}
	close(stop)
	wg.Wait()
}
