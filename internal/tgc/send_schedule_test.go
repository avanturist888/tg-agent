package tgc

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// Ответ отложенной отправки, как у Telegram: Updates с UpdateMessageID
// (id очереди, привязан к RandomID) и UpdateNewScheduledMessage.
func TestSendMessageScheduleDate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
	}{
		{"сразу", 0},
		{"отложенно", time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			var request *tg.MessagesSendMessageRequest
			c := &Conn{}
			c.API = tg.NewClient(historySinceInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				req, ok := in.(*tg.MessagesSendMessageRequest)
				if !ok {
					t.Fatalf("unexpected RPC %T", in)
				}
				request = req
				out.(*tg.UpdatesBox).Updates = &tg.Updates{Updates: []tg.UpdateClass{
					&tg.UpdateMessageID{RandomID: req.RandomID, ID: 7},
					&tg.UpdateNewScheduledMessage{Message: &tg.Message{ID: 8}},
				}}
				return nil
			}))
			r, err := c.SendMessage(context.Background(), Target{ScheduleIn: tc.delay}, "текст", nil, "plain")
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := r.Get("message_id"); got != 7 {
				t.Fatalf("message_id = %v, ждали 7 (id очереди из UpdateMessageID)", got)
			}
			date, ok := request.GetScheduleDate()
			if tc.delay == 0 {
				if ok {
					t.Fatalf("schedule_date установлен без ScheduleIn: %d", date)
				}
				return
			}
			if !ok {
				t.Fatal("schedule_date не установлен")
			}
			at := time.Unix(int64(date), 0)
			if at.Before(now.Add(55*time.Second)) || at.After(now.Add(70*time.Second)) {
				t.Fatalf("schedule_date = %v, now = %v", at, now)
			}
		})
	}
}

// Ответ без UpdateMessageID (только UpdateNewScheduledMessage) — id достаётся
// запасным путём: сообщение в очереди, черновик не остаётся «могло уйти».
func TestSendMessageScheduleFallbackID(t *testing.T) {
	c := &Conn{}
	c.API = tg.NewClient(historySinceInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		out.(*tg.UpdatesBox).Updates = &tg.Updates{Updates: []tg.UpdateClass{
			&tg.UpdateNewScheduledMessage{Message: &tg.Message{ID: 42}},
		}}
		return nil
	}))
	r, err := c.SendMessage(context.Background(), Target{ScheduleIn: time.Minute}, "текст", nil, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get("message_id"); got != 42 {
		t.Fatalf("message_id = %v, ждали 42", got)
	}
}
