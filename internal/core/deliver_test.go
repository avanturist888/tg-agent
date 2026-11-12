package core

import (
	"context"
	"errors"
	"testing"

	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

type fakeSender struct {
	sent, edited, files int
	polls               []tgc.PollOut
	filesErr            error // SendFiles: ушло filesOK файлов, дальше эта ошибка
	filesOK             int
}

func (f *fakeSender) SendMessage(_ context.Context, _ tgc.Target, _ string, _ *int64, format string) (*omap.Map, error) {
	f.sent++
	return omap.New().Set("message_id", 3606036).Set("format", "rich_"+format), nil
}

func (f *fakeSender) EditMessage(_ context.Context, _ tgc.Target, id int, _, format string) (*omap.Map, error) {
	f.edited++
	return omap.New().Set("message_id", id).Set("edited", true).Set("format", format), nil
}

func (f *fakeSender) SendPoll(_ context.Context, _ tgc.Target, p tgc.PollOut, _ *int64) (*omap.Map, error) {
	f.polls = append(f.polls, p)
	return omap.New().Set("message_id", 900).Set("format", "poll"), nil
}

func (f *fakeSender) SendVote(_ context.Context, _ tgc.Target, msgID int, _ []int) (*omap.Map, error) {
	return omap.New().Set("message_id", msgID).Set("format", "vote"), nil
}

func (f *fakeSender) SendFiles(_ context.Context, _ tgc.Target, files []tgc.OutFile, _ *int64) ([]int, error) {
	f.files++
	ids := make([]int, len(files))
	for i := range ids {
		ids[i] = 500 + i
	}
	if f.filesErr != nil {
		return ids[:f.filesOK], f.filesErr
	}
	return ids, nil
}

// Текст или часть файлов ушли, дальше сбой: отказ окончательный — повтор
// задвоил бы ушедшее. Ничего не ушло — обычный сбой, его можно повторить.
func TestSendDraftPartialIsFinal(t *testing.T) {
	ctx := context.Background()
	files := []outbox.FileSnap{{Path: "a.txt", Name: "a.txt"}, {Path: "b.txt", Name: "b.txt"}}
	var d *Denied
	for _, c := range []struct {
		name    string
		draft   outbox.Draft
		ok      int
		partial bool
	}{
		{"текст ушёл", outbox.Draft{Text: "раз", Files: files}, 0, true},
		{"файл ушёл", outbox.Draft{Files: files}, 1, true},
		{"ничего не ушло", outbox.Draft{Files: files}, 0, false},
	} {
		_, err := sendDraft(ctx, &fakeSender{filesErr: errors.New("upload failed"), filesOK: c.ok}, tgc.Target{}, &c.draft)
		if err == nil || errors.As(err, &d) != c.partial {
			t.Errorf("%s: %v", c.name, err)
		}
	}
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
		{"голос", outbox.Draft{Vote: &outbox.VoteSpec{MessageID: 41, Options: []int{0}}}, 41},
		{"опрос", outbox.Draft{Poll: &outbox.PollSpec{Question: "Кто идёт?", Options: []string{"я", "не я"}}}, 900},
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
