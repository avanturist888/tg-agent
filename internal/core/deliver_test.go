package core

import (
	"context"
	"testing"

	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

type fakeSender struct{ sent, edited, files int }

func (f *fakeSender) SendMessage(_ context.Context, _ tgc.Target, _ string, _ *int64, format string) (*omap.Map, error) {
	f.sent++
	return omap.New().Set("message_id", 3606036).Set("format", "rich_"+format), nil
}

func (f *fakeSender) EditMessage(_ context.Context, _ tgc.Target, id int, _, format string) (*omap.Map, error) {
	f.edited++
	return omap.New().Set("message_id", id).Set("edited", true).Set("format", format), nil
}

func (f *fakeSender) SendFiles(_ context.Context, _ tgc.Target, files []tgc.OutFile, _ *int64) ([]int, error) {
	f.files++
	ids := make([]int, len(files))
	for i := range ids {
		ids[i] = 500 + i
	}
	return ids, nil
}

// Ответ отправки — с настоящим message_id: без него tg_wait_approval отдаёт 0,
// а tg_edit_message не находит своё сообщение (так было 27.10–02.11).
func TestSendDraftKeepsMessageID(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		d    outbox.Draft
		want int64
	}{
		{"текст", outbox.Draft{Text: "привет", Fmt: "markdown"}, 3606036},
		{"текст и файл", outbox.Draft{Text: "привет", Files: []outbox.FileSnap{{Path: "a.txt"}}}, 3606036},
		{"только файлы", outbox.Draft{Files: []outbox.FileSnap{{Path: "a.txt"}, {Path: "b.txt"}}}, 500},
		{"правка", outbox.Draft{Text: "стало", EditOf: 77}, 77},
	}
	for _, c := range cases {
		r, err := sendDraft(ctx, &fakeSender{}, tgc.Target{}, &c.d)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		mid, _ := r.Get("message_id")
		if got := toInt64(mid); got != c.want {
			t.Errorf("%s: message_id %d, ждали %d", c.name, got, c.want)
		}
	}
}
