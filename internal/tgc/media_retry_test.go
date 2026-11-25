package tgc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"syscall"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type fakeCloseInvoker struct {
	dc      int
	closed  int
	onClose func()
}

func (f *fakeCloseInvoker) Invoke(context.Context, bin.Encoder, bin.Decoder) error {
	return nil
}

func (f *fakeCloseInvoker) Close() error {
	f.closed++
	if f.onClose != nil {
		f.onClose()
	}
	return nil
}

func stubMediaDownloadHooks(
	t *testing.T,
	streamFn func(ctx context.Context, api *tg.Client, loc tg.InputFileLocationClass, w io.Writer) error,
	openFn func(ctx context.Context, client *telegram.Client, dcID int) (telegram.CloseInvoker, error),
) {
	t.Helper()
	origStream := streamDownload
	origOpen := openFreshDC
	streamDownload = streamFn
	openFreshDC = openFn
	t.Cleanup(func() {
		streamDownload = origStream
		openFreshDC = origOpen
	})
}

func mediaTestConn(msg *tg.Message) *Conn {
	c := &Conn{
		Client: &telegram.Client{},
		Peers:  &PeerCache{data: newPeerData()},
	}
	c.API = tg.NewClient(historySinceInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		if _, ok := in.(*tg.MessagesGetMessagesRequest); !ok {
			return fmt.Errorf("unexpected RPC %T", in)
		}
		out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{
			Messages: []tg.MessageClass{msg},
			Users:    []tg.UserClass{&tg.User{ID: 42}},
		}
		return nil
	}))
	return c
}

func TestIsMediaConnBroken(t *testing.T) {
	wsasendErr := errors.New("write tcp 192.0.2.1:54321->149.154.167.41:443: wsasend: An established connection was aborted by the software in your host machine.")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"wrapped context canceled", fmt.Errorf("download: %w", context.Canceled), false},
		{"context deadline", context.DeadlineExceeded, false},
		{"net error wrapping deadline", &net.OpError{Op: "read", Net: "tcp", Err: context.DeadlineExceeded}, false},
		{"rpc file reference expired", tgerr.New(400, "FILE_REFERENCE_EXPIRED"), false},
		{"wrapped rpc error", fmt.Errorf("upload.getFile: %w", tgerr.New(303, "FILE_MIGRATE_4")), false},
		{"rpc error with reset text", tgerr.New(500, "CONNECTION_RESET"), false},
		{"generic error", errors.New("some application error"), false},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"syscall econnreset", fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{"syscall econnaborted", fmt.Errorf("write: %w", syscall.ECONNABORTED), true},
		{"syscall epipe", fmt.Errorf("write: %w", syscall.EPIPE), true},
		{"net error", &net.OpError{Op: "write", Net: "tcp", Err: errors.New("socket closed")}, true},
		{"wsasend abort text", wsasendErr, true},
		{"wsarecv text", errors.New("read tcp: wsarecv: forcible close"), true},
		{"connection aborted text", errors.New("tcp: Connection Aborted"), true},
		{"connection reset text", errors.New("read: connection reset by peer"), true},
		{"broken pipe text", errors.New("write: broken pipe"), true},
		{"engine forcibly closed", fmt.Errorf("engine forcibly closed: %w", context.Canceled), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMediaConnBroken(tc.err); got != tc.want {
				t.Fatalf("isMediaConnBroken(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMediaDownloadFirstAttemptSuccess(t *testing.T) {
	freshOpened := 0
	stubMediaDownloadHooks(t,
		func(_ context.Context, _ *tg.Client, _ tg.InputFileLocationClass, w io.Writer) error {
			_, err := w.Write([]byte("ok-first-try"))
			return err
		},
		func(_ context.Context, _ *telegram.Client, _ int) (telegram.CloseInvoker, error) {
			freshOpened++
			return &fakeCloseInvoker{}, nil
		},
	)

	c := &Conn{Client: &telegram.Client{}, API: tg.NewClient(&fakeCloseInvoker{})}
	var buf bytes.Buffer
	resetCalls := 0
	err := c.download(context.Background(), &tg.InputDocumentFileLocation{ID: 1}, 2, &buf, func() error {
		resetCalls++
		buf.Reset()
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if freshOpened != 0 {
		t.Fatalf("openFreshDC called %d times, want 0", freshOpened)
	}
	if resetCalls != 0 {
		t.Fatalf("reset called %d times, want 0", resetCalls)
	}
	if buf.String() != "ok-first-try" {
		t.Fatalf("got %q, want %q", buf.String(), "ok-first-try")
	}
}

func TestMediaDownloadRetryFetchImageBufferOnWsasend(t *testing.T) {
	wsasendErr := errors.New("write tcp 192.0.2.1:54321->149.154.167.41:443: wsasend: An established connection was aborted by the software in your host machine.")
	msg := &tg.Message{
		ID:     10,
		PeerID: &tg.PeerUser{UserID: 42},
		Date:   100,
		Media: &tg.MessageMediaPhoto{
			Photo: &tg.Photo{
				ID:            1001,
				AccessHash:    2002,
				FileReference: []byte{1, 2, 3},
				DCID:          4,
				Sizes: []tg.PhotoSizeClass{
					&tg.PhotoSize{Type: "x", W: 100, H: 100, Size: 16},
				},
			},
		},
	}
	c := mediaTestConn(msg)

	var (
		attempts int
		openedDC []int
		inv      *fakeCloseInvoker
	)
	stubMediaDownloadHooks(t,
		func(_ context.Context, api *tg.Client, _ tg.InputFileLocationClass, w io.Writer) error {
			attempts++
			if attempts == 1 {
				if api != c.API {
					t.Fatal("first attempt did not use c.API")
				}
				_, _ = w.Write([]byte("partial-garbage-before-abort"))
				return wsasendErr
			}
			if api == c.API {
				t.Fatal("retry attempt reused dead c.API instead of fresh DC client")
			}
			_, err := w.Write([]byte("clean-jpeg-bytes"))
			return err
		},
		func(_ context.Context, _ *telegram.Client, dcID int) (telegram.CloseInvoker, error) {
			openedDC = append(openedDC, dcID)
			inv = &fakeCloseInvoker{dc: dcID}
			return inv, nil
		},
	)

	target := Target{Kind: "user", ID: 42, Input: &tg.InputPeerUser{UserID: 42}}
	img, err := c.FetchImage(context.Background(), target, 10)
	if err != nil {
		t.Fatalf("FetchImage failed: %v", err)
	}
	if string(img.Data) != "clean-jpeg-bytes" {
		t.Fatalf("img.Data = %q, want %q", string(img.Data), "clean-jpeg-bytes")
	}
	if !reflect.DeepEqual(openedDC, []int{4}) {
		t.Fatalf("openedDC = %v, want [4]", openedDC)
	}
	if inv == nil || inv.closed != 1 {
		t.Fatalf("fresh DC invoker closed = %v, want 1", inv)
	}
}

func TestMediaDownloadRetryDownloadFileOnWsasend(t *testing.T) {
	wsasendErr := errors.New("write tcp 192.0.2.1:54321->149.154.167.41:443: wsasend: An established connection was aborted by the software in your host machine.")
	msg := &tg.Message{
		ID:     11,
		PeerID: &tg.PeerUser{UserID: 42},
		Date:   100,
		Media: &tg.MessageMediaDocument{
			Document: &tg.Document{
				ID:            3003,
				AccessHash:    4004,
				FileReference: []byte{4, 5, 6},
				DCID:          2,
				Size:          9,
				MimeType:      "text/plain",
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeFilename{FileName: "report.txt"},
				},
			},
		},
	}
	c := mediaTestConn(msg)

	var (
		attempts int
		openedDC []int
		inv      *fakeCloseInvoker
	)
	stubMediaDownloadHooks(t,
		func(_ context.Context, _ *tg.Client, _ tg.InputFileLocationClass, w io.Writer) error {
			attempts++
			if attempts == 1 {
				// Intentionally longer than final payload to verify both Truncate(0) and Seek(0, SeekStart).
				_, _ = w.Write([]byte("partial-garbage-much-longer-than-final-payload-0123456789"))
				return wsasendErr
			}
			_, err := w.Write([]byte("clean-doc"))
			return err
		},
		func(_ context.Context, _ *telegram.Client, dcID int) (telegram.CloseInvoker, error) {
			openedDC = append(openedDC, dcID)
			inv = &fakeCloseInvoker{dc: dcID}
			return inv, nil
		},
	)

	target := Target{Kind: "user", ID: 42, Input: &tg.InputPeerUser{UserID: 42}}
	res, err := c.DownloadFile(context.Background(), target, 11, t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != "clean-doc" {
		t.Fatalf("file content = %q, want %q", string(got), "clean-doc")
	}
	if res.Size != int64(len("clean-doc")) {
		t.Fatalf("res.Size = %d, want %d", res.Size, len("clean-doc"))
	}
	if !reflect.DeepEqual(openedDC, []int{2}) {
		t.Fatalf("openedDC = %v, want [2]", openedDC)
	}
	if inv == nil || inv.closed != 1 {
		t.Fatalf("fresh DC invoker closed = %v, want 1", inv)
	}
}

func TestMediaDownloadRetryFileMigrateOneHop(t *testing.T) {
	t.Run("migrates once and closes both invokers", func(t *testing.T) {
		var (
			attempts int
			openedDC []int
			invokers []*fakeCloseInvoker
			events   []string
		)
		stubMediaDownloadHooks(t,
			func(_ context.Context, _ *tg.Client, _ tg.InputFileLocationClass, w io.Writer) error {
				attempts++
				switch attempts {
				case 1:
					_, _ = w.Write([]byte("stale-partial"))
					return io.EOF
				case 2:
					_, _ = w.Write([]byte("wrong-dc-partial"))
					return fmt.Errorf("upload.getFile: %w", tgerr.New(303, "FILE_MIGRATE_5"))
				case 3:
					_, err := w.Write([]byte("dc5-clean-payload"))
					return err
				default:
					t.Fatalf("unexpected attempt %d", attempts)
					return nil
				}
			},
			func(_ context.Context, _ *telegram.Client, dcID int) (telegram.CloseInvoker, error) {
				openedDC = append(openedDC, dcID)
				events = append(events, fmt.Sprintf("open:%d", dcID))
				inv := &fakeCloseInvoker{
					dc: dcID,
					onClose: func() {
						events = append(events, fmt.Sprintf("close:%d", dcID))
					},
				}
				invokers = append(invokers, inv)
				return inv, nil
			},
		)

		c := &Conn{Client: &telegram.Client{}, API: tg.NewClient(&fakeCloseInvoker{})}
		var buf bytes.Buffer
		resetCalls := 0
		err := c.download(context.Background(), &tg.InputDocumentFileLocation{ID: 7}, 2, &buf, func() error {
			resetCalls++
			buf.Reset()
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if buf.String() != "dc5-clean-payload" {
			t.Fatalf("got %q, want %q", buf.String(), "dc5-clean-payload")
		}
		if !reflect.DeepEqual(openedDC, []int{2, 5}) {
			t.Fatalf("openedDC = %v, want [2 5]", openedDC)
		}
		if resetCalls != 2 {
			t.Fatalf("resetCalls = %d, want 2", resetCalls)
		}
		if !reflect.DeepEqual(events, []string{"open:2", "close:2", "open:5", "close:5"}) {
			t.Fatalf("lifecycle events = %v, want [open:2 close:2 open:5 close:5]", events)
		}
		for i, inv := range invokers {
			if inv.closed != 1 {
				t.Fatalf("invoker[%d] closed = %d, want 1", i, inv.closed)
			}
		}
	})

	t.Run("stops after one FILE_MIGRATE hop", func(t *testing.T) {
		var (
			attempts int
			openedDC []int
			invokers []*fakeCloseInvoker
		)
		secondMigrate := tgerr.New(303, "FILE_MIGRATE_4")
		stubMediaDownloadHooks(t,
			func(_ context.Context, _ *tg.Client, _ tg.InputFileLocationClass, _ io.Writer) error {
				attempts++
				switch attempts {
				case 1:
					return io.ErrUnexpectedEOF
				case 2:
					return tgerr.New(303, "FILE_MIGRATE_5")
				case 3:
					return secondMigrate
				default:
					t.Fatalf("unexpected attempt %d", attempts)
					return nil
				}
			},
			func(_ context.Context, _ *telegram.Client, dcID int) (telegram.CloseInvoker, error) {
				openedDC = append(openedDC, dcID)
				inv := &fakeCloseInvoker{dc: dcID}
				invokers = append(invokers, inv)
				return inv, nil
			},
		)

		c := &Conn{Client: &telegram.Client{}, API: tg.NewClient(&fakeCloseInvoker{})}
		var buf bytes.Buffer
		err := c.download(context.Background(), &tg.InputDocumentFileLocation{ID: 8}, 2, &buf, func() error {
			buf.Reset()
			return nil
		})
		if !errors.Is(err, secondMigrate) {
			t.Fatalf("err = %v, want %v", err, secondMigrate)
		}
		if !reflect.DeepEqual(openedDC, []int{2, 5}) {
			t.Fatalf("openedDC = %v, want [2 5]", openedDC)
		}
		for i, inv := range invokers {
			if inv.closed != 1 {
				t.Fatalf("invoker[%d] closed = %d, want 1", i, inv.closed)
			}
		}
	})

	t.Run("no hop when FILE_MIGRATE points to same DC", func(t *testing.T) {
		var (
			attempts int
			openedDC []int
			inv      *fakeCloseInvoker
		)
		sameDC := tgerr.New(303, "FILE_MIGRATE_2")
		stubMediaDownloadHooks(t,
			func(_ context.Context, _ *tg.Client, _ tg.InputFileLocationClass, _ io.Writer) error {
				attempts++
				if attempts == 1 {
					return io.EOF
				}
				return sameDC
			},
			func(_ context.Context, _ *telegram.Client, dcID int) (telegram.CloseInvoker, error) {
				openedDC = append(openedDC, dcID)
				inv = &fakeCloseInvoker{dc: dcID}
				return inv, nil
			},
		)

		c := &Conn{Client: &telegram.Client{}, API: tg.NewClient(&fakeCloseInvoker{})}
		var buf bytes.Buffer
		err := c.download(context.Background(), &tg.InputDocumentFileLocation{ID: 9}, 2, &buf, func() error {
			buf.Reset()
			return nil
		})
		if !errors.Is(err, sameDC) {
			t.Fatalf("err = %v, want %v", err, sameDC)
		}
		if !reflect.DeepEqual(openedDC, []int{2}) {
			t.Fatalf("openedDC = %v, want [2]", openedDC)
		}
		if inv == nil || inv.closed != 1 {
			t.Fatalf("invoker closed = %v, want 1", inv)
		}
	})
}

func TestMediaDownloadNoRetryOnRPCOrCanceled(t *testing.T) {
	cases := []struct {
		name      string
		cancelCtx bool
		err       error
	}{
		{"FILE_REFERENCE_EXPIRED", false, tgerr.New(400, "FILE_REFERENCE_EXPIRED")},
		{"wrapped RPC error", false, fmt.Errorf("download: %w", tgerr.New(400, "FILE_REFERENCE_EXPIRED"))},
		{"context.Canceled error", false, context.Canceled},
		{"context.DeadlineExceeded error", false, context.DeadlineExceeded},
		{"transport error with canceled ctx", true, io.EOF},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			freshOpened := 0
			stubMediaDownloadHooks(t,
				func(_ context.Context, _ *tg.Client, _ tg.InputFileLocationClass, _ io.Writer) error {
					return tc.err
				},
				func(_ context.Context, _ *telegram.Client, _ int) (telegram.CloseInvoker, error) {
					freshOpened++
					return &fakeCloseInvoker{}, nil
				},
			)

			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancelCtx {
				cancel()
			} else {
				defer cancel()
			}

			c := &Conn{Client: &telegram.Client{}, API: tg.NewClient(&fakeCloseInvoker{})}
			var buf bytes.Buffer
			err := c.download(ctx, &tg.InputDocumentFileLocation{ID: 12}, 2, &buf, func() error {
				buf.Reset()
				return nil
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if freshOpened != 0 {
				t.Fatalf("openFreshDC called %d times, want 0", freshOpened)
			}
		})
	}
}

func TestMediaDownloadNoRetryWhenDCZeroOrClientNil(t *testing.T) {
	transportErr := errors.New("write tcp 192.0.2.1:54321->149.154.167.41:443: wsasend: An established connection was aborted by the software in your host machine.")

	for _, tc := range []struct {
		name   string
		client *telegram.Client
		dcID   int
	}{
		{"dcID zero", &telegram.Client{}, 0},
		{"client nil", nil, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freshOpened := 0
			stubMediaDownloadHooks(t,
				func(_ context.Context, _ *tg.Client, _ tg.InputFileLocationClass, _ io.Writer) error {
					return transportErr
				},
				func(_ context.Context, _ *telegram.Client, _ int) (telegram.CloseInvoker, error) {
					freshOpened++
					return &fakeCloseInvoker{}, nil
				},
			)

			c := &Conn{Client: tc.client, API: tg.NewClient(&fakeCloseInvoker{})}
			var buf bytes.Buffer
			err := c.download(context.Background(), &tg.InputDocumentFileLocation{ID: 13}, tc.dcID, &buf, func() error {
				buf.Reset()
				return nil
			})
			if !errors.Is(err, transportErr) {
				t.Fatalf("err = %v, want %v", err, transportErr)
			}
			if freshOpened != 0 {
				t.Fatalf("openFreshDC called %d times, want 0", freshOpened)
			}
		})
	}
}
