package tgc

// Опросы: что агент видит при чтении (PollInfo), отправка (SendPoll) и кто
// за что проголосовал (PollVotes; только у неанонимных опросов).
// Варианты нумеруются с 0 — так же их указывают при создании (correct).

import (
	"bytes"
	"context"
	"fmt"

	"github.com/gotd/td/tg"

	"tgagent/internal/omap"
)

// PollOut — опрос к отправке.
type PollOut struct {
	Question  string
	Options   []string
	Anonymous bool
	Multiple  bool
	Quiz      bool
	Correct   []int // у викторины — номера верных вариантов (с 0)
	Solution  string
}

// PollOf — опрос сообщения (nil — это не опрос).
func PollOf(m *tg.Message) *tg.MessageMediaPoll {
	p, _ := m.Media.(*tg.MessageMediaPoll)
	return p
}

// PollInfo — опрос для агента: вопрос, варианты с голосами, настройки.
func PollInfo(m *tg.Message) *omap.Map {
	mp := PollOf(m)
	if mp == nil {
		return nil
	}
	p, r := mp.Poll, mp.Results
	results := map[string]tg.PollAnswerVoters{}
	for _, v := range r.Results {
		results[string(v.Option)] = v
	}
	_, hasCounts := r.GetResults()
	options := make([]*omap.Map, 0, len(p.Answers))
	for i, a := range p.Answers {
		pa, ok := a.(*tg.PollAnswer)
		if !ok {
			continue
		}
		o := omap.New().Set("index", i).Set("text", pa.Text.Text)
		if v, ok := results[string(pa.Option)]; ok {
			o.Set("voters", v.Voters)
			if v.Chosen {
				o.Set("chosen", true) // голос владельца
			}
			if v.Correct {
				o.Set("correct", true)
			}
		} else if hasCounts {
			o.Set("voters", 0)
		}
		options = append(options, o)
	}
	out := omap.New().Set("question", p.Question.Text).Set("options", options).
		Set("anonymous", !p.PublicVoters).Set("multiple", p.MultipleChoice).Set("quiz", p.Quiz).
		Set("closed", p.Closed)
	if total, ok := r.GetTotalVoters(); ok {
		out.Set("total_voters", total)
	} else {
		out.Set("total_voters", nil) // результаты скрыты, пока владелец не проголосует
	}
	if d, ok := p.GetCloseDate(); ok && d != 0 {
		out.Set("close_date", isoTime(d))
	}
	if s, ok := r.GetSolution(); ok && s != "" {
		out.Set("solution", s)
	}
	return out
}

// SendPoll — отправить опрос от аккаунта владельца.
func (c *Conn) SendPoll(ctx context.Context, t Target, p PollOut, reply *int64) (*omap.Map, error) {
	answers := make([]tg.PollAnswerClass, len(p.Options))
	for i, o := range p.Options {
		answers[i] = &tg.InputPollAnswer{Text: tg.TextWithEntities{Text: o, Entities: []tg.MessageEntityClass{}}}
	}
	poll := tg.Poll{
		Question:       tg.TextWithEntities{Text: p.Question, Entities: []tg.MessageEntityClass{}},
		Answers:        answers,
		PublicVoters:   !p.Anonymous,
		MultipleChoice: p.Multiple,
		Quiz:           p.Quiz,
	}
	media := &tg.InputMediaPoll{Poll: poll}
	if p.Quiz {
		media.SetCorrectAnswers(p.Correct)
		if p.Solution != "" {
			media.SetSolution(p.Solution)
			media.SetSolutionEntities([]tg.MessageEntityClass{})
		}
	}
	req := &tg.MessagesSendMediaRequest{Peer: t.Input, Media: media, RandomID: randomID()}
	if r := replyTo(reply, t.Topic); r != nil {
		req.ReplyTo = r
	}
	res, err := c.API.MessagesSendMedia(ctx, req)
	if err != nil {
		return nil, err
	}
	ids, date := sent(res, []int64{req.RandomID})
	if len(ids) == 0 {
		return nil, fmt.Errorf("Telegram не вернул id отправленного опроса")
	}
	out := omap.New().Set("message_id", ids[0])
	if date != 0 {
		out.Set("date", isoTime(date))
	}
	return out.Set("format", "poll"), nil
}

// PollVotes — кто как проголосовал. option < 0 — за все варианты.
// Telegram отдаёт это только для неанонимных опросов.
func (c *Conn) PollVotes(ctx context.Context, t Target, msgID, option, limit int) (*omap.Map, error) {
	opts, info, err := c.PollOptions(ctx, t, msgID)
	if err != nil {
		return nil, err
	}
	if anon, _ := info.Get("anonymous"); anon == true {
		return nil, &NotPoll{ID: msgID, Anonymous: true}
	}
	indexOf := func(o []byte) int {
		for i, x := range opts {
			if bytes.Equal(x, o) {
				return i
			}
		}
		return -1
	}
	req := &tg.MessagesGetPollVotesRequest{Peer: t.Input, ID: msgID, Limit: min(limit, 50)}
	if option >= 0 {
		if option >= len(opts) {
			return nil, fmt.Errorf("в опросе %d вариантов (с 0), варианта %d нет", len(opts), option)
		}
		req.SetOption(opts[option])
	}
	b := &Batch{Users: map[int64]*tg.User{}, Chats: map[int64]tg.ChatClass{}}
	var votes []*omap.Map
	count := 0
	for len(votes) < limit {
		req.Limit = min(50, limit-len(votes))
		res, err := c.API.MessagesGetPollVotes(ctx, req)
		if err != nil {
			return nil, err
		}
		count = res.Count
		c.Peers.Remember(res.Users, res.Chats)
		for _, u := range res.Users {
			if user, ok := u.(*tg.User); ok {
				b.Users[user.ID] = user
			}
		}
		for _, ch := range res.Chats {
			b.Chats[ch.GetID()] = ch
		}
		for _, v := range res.Votes {
			var (
				peer    tg.PeerClass
				date    int
				options []int
			)
			switch x := v.(type) {
			case *tg.MessagePeerVote:
				peer, date, options = x.Peer, x.Date, []int{indexOf(x.Option)}
			case *tg.MessagePeerVoteMultiple:
				peer, date = x.Peer, x.Date
				for _, o := range x.Options {
					options = append(options, indexOf(o))
				}
			case *tg.MessagePeerVoteInputOption:
				peer, date = x.Peer, x.Date // свой вариант ответа
			default:
				continue
			}
			id, kind := peerID(peer)
			who := person(id, b, kind)
			if who == nil {
				who = omap.New().Set("id", id).Set("name", "?")
			}
			votes = append(votes, omap.New().Set("from", who).Set("options", options).Set("date", isoTime(date)))
		}
		next, ok := res.GetNextOffset()
		if !ok || next == "" || len(res.Votes) == 0 {
			break
		}
		req.SetOffset(next)
	}
	c.Peers.Save()
	if votes == nil {
		votes = []*omap.Map{}
	}
	return omap.New().Set("poll", info).Set("count", count).Set("votes", votes), nil
}

// NotPoll — сообщение не опрос (или опрос анонимный).
type NotPoll struct {
	ID        int
	Anonymous bool // опрос, но анонимный: кто голосовал, не узнать
}

func (e *NotPoll) Error() string {
	if e.Anonymous {
		return fmt.Sprintf("Опрос %d анонимный — кто как голосовал, Telegram не показывает никому; "+
			"сколько голосов за каждый вариант — в tg_read_chat (poll.options[].voters).", e.ID)
	}
	return fmt.Sprintf("Сообщение %d — не опрос.", e.ID)
}

func peerID(p tg.PeerClass) (int64, string) {
	switch v := p.(type) {
	case *tg.PeerUser:
		return v.UserID, "user"
	case *tg.PeerChat:
		return v.ChatID, "chat"
	case *tg.PeerChannel:
		return v.ChannelID, "channel"
	}
	return 0, ""
}

// PollOptions — опрос сообщения msgID: варианты (байты option) и сам
// опрос для агента. Ошибка NotPoll — это не опрос.
func (c *Conn) PollOptions(ctx context.Context, t Target, msgID int) ([][]byte, *omap.Map, error) {
	m, _, err := c.Message(ctx, t, msgID)
	if err != nil {
		return nil, nil, err
	}
	msg, ok := m.(*tg.Message)
	if !ok || msg == nil {
		return nil, nil, fmt.Errorf("сообщения %d нет", msgID)
	}
	mp := PollOf(msg)
	if mp == nil {
		return nil, nil, &NotPoll{ID: msgID}
	}
	var opts [][]byte
	for _, a := range mp.Poll.Answers {
		if pa, ok := a.(*tg.PollAnswer); ok {
			opts = append(opts, pa.Option)
		}
	}
	return opts, PollInfo(msg), nil
}

// SendVote — голос владельца в опросе (options пусто — снять голос).
// Возвращает опрос после голосования.
func (c *Conn) SendVote(ctx context.Context, t Target, msgID int, options []int) (*omap.Map, error) {
	opts, _, err := c.PollOptions(ctx, t, msgID)
	if err != nil {
		return nil, err
	}
	chosen := make([][]byte, 0, len(options))
	for _, i := range options {
		if i < 0 || i >= len(opts) {
			return nil, fmt.Errorf("в опросе %d вариантов (с 0), варианта %d нет", len(opts), i)
		}
		chosen = append(chosen, opts[i])
	}
	if _, err := c.API.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{Peer: t.Input, MsgID: msgID, Options: chosen}); err != nil {
		return nil, err
	}
	_, poll, err := c.PollOptions(ctx, t, msgID)
	if err != nil {
		poll = nil // голос ушёл; опрос просто не перечитали
	}
	return omap.New().Set("message_id", msgID).Set("voted", options).Set("poll", poll).Set("format", "vote"), nil
}
