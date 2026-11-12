package tgc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tgerr"
)

// Отказ — только явный: «уже отправлено», ошибка сервера и незнакомое —
// не доказательство, что сообщения нет.
func TestRejected(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{tgerr.New(400, "PEER_ID_INVALID"), true},
		{tgerr.New(403, "CHAT_WRITE_FORBIDDEN"), true},
		{tgerr.New(420, "FLOOD_WAIT_30"), true},
		{fmt.Errorf("send: %w", tgerr.New(400, "MESSAGE_EMPTY")), true},
		{tgerr.New(400, "RANDOM_ID_DUPLICATE"), false},
		{tgerr.New(500, "INTERNAL"), false},
		{tgerr.New(-503, "Timeout"), false},
		{errors.New("i/o timeout"), false},
	} {
		if got := Rejected(c.err); got != c.want {
			t.Errorf("%v: %v", c.err, got)
		}
	}
}
