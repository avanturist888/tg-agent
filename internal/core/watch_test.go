package core

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/t3"
)

type syncBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func watchEnv(t *testing.T) *config.Settings {
	t.Helper()
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	// настоящий T3 Code этой машины тесты не трогают
	oldWake := wakeT3
	wakeT3 = func(context.Context, *config.Settings, string, string) error { return t3.Unavailable }
	t.Cleanup(func() { wakeT3 = oldWake })
	s := &config.Settings{
		Chats:     map[string]config.ChatRule{"test": {Alias: "test", Peer: int64(-100123), Read: true}},
		ChatOrder: []string{"test"},
	}
	os.MkdirAll(s.FeedsDir(), 0o700)
	return s
}

func TestWatchSeesAppends(t *testing.T) {
	s := watchEnv(t)
	feed := FeedPath(s, "test")
	if _, err := appendNewer(feed, []int{10}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, s, []string{"test"}, 0, out) }()

	for i, id := range []int{11, 12, 13} {
		time.Sleep(300 * time.Millisecond)
		if _, err := appendNewer(feed, []int{id}); err != nil {
			t.Fatal(err)
		}
		want := "test " + []string{"11", "12", "13"}[i] + "\n"
		deadline := time.Now().Add(2 * time.Second)
		for !strings.Contains(out.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("watch не увидел %q за 2 с; вывод:\n%s", want, out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if strings.Contains(out.String(), "test 10") {
		t.Fatal("старое сообщение выдано как новое")
	}
	cancel()
	<-done
}

type closedWriter struct{ n int }

func (w *closedWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n > 1 { // приветствие прошло, дальше читателя нет
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestWatchExitsWithoutReader(t *testing.T) {
	s := watchEnv(t)
	feed := FeedPath(s, "test")
	done := make(chan error, 1)
	go func() { done <- Watch(context.Background(), s, []string{"test"}, 0, &closedWriter{}) }()
	time.Sleep(300 * time.Millisecond)
	appendNewer(feed, []int{5})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watch без читателя не завершился")
	}
}

func TestWatchAfterCatchesUp(t *testing.T) {
	s := watchEnv(t)
	feed := FeedPath(s, "test")
	if _, err := appendNewer(feed, []int{20, 21, 22}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, s, []string{"test"}, 20, out) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	got := out.String()
	if !strings.Contains(got, "test 21") || !strings.Contains(got, "test 22") || strings.Contains(got, "test 20") {
		t.Fatalf("после --after 20 ожидались 21 и 22:\n%s", got)
	}
}
