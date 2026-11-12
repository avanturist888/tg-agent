package core

// Опросы (tg_draft_poll, tg_poll_votes). Опрос — такой же черновик, как
// сообщение: та же карточка, подтверждение и автоотправка; отличается только
// тем, что уходит (outbox.Draft.Poll → tgc.SendPoll). Что в опросе и сколько
// голосов — видно в tg_read_chat (поле poll), кто за что — tg_poll_votes.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

// Пределы Telegram для опроса.
const (
	pollQuestionMax = 255
	pollOptionMax   = 100
	pollOptionsMax  = 12
)

// checkPoll — опрос, который Telegram примет; варианты и вопрос без пробелов по краям.
func checkPoll(p *outbox.PollSpec) error {
	p.Question = strings.TrimSpace(p.Question)
	if p.Question == "" {
		return &Bad{"Нужен вопрос опроса (question)."}
	}
	if n := len([]rune(p.Question)); n > pollQuestionMax {
		return &Bad{fmt.Sprintf("Вопрос — %d символов, Telegram принимает до %d.", n, pollQuestionMax)}
	}
	if len(p.Options) < 2 || len(p.Options) > pollOptionsMax {
		return &Bad{fmt.Sprintf("Вариантов — от 2 до %d, а их %d.", pollOptionsMax, len(p.Options))}
	}
	seen := map[string]bool{}
	for i, o := range p.Options {
		o = strings.TrimSpace(o)
		if o == "" {
			return &Bad{fmt.Sprintf("Вариант %d пустой.", i)}
		}
		if n := len([]rune(o)); n > pollOptionMax {
			return &Bad{fmt.Sprintf("Вариант %d — %d символов, Telegram принимает до %d.", i, n, pollOptionMax)}
		}
		if seen[strings.ToLower(o)] {
			return &Bad{fmt.Sprintf("Вариант «%s» повторяется.", o)}
		}
		seen[strings.ToLower(o)] = true
		p.Options[i] = o
	}
	p.Solution = strings.TrimSpace(p.Solution)
	if !p.Quiz {
		if len(p.Correct) > 0 || p.Solution != "" {
			return &Bad{"correct и solution — только у викторины (quiz=true)."}
		}
		return nil
	}
	if len(p.Correct) == 0 {
		return &Bad{"Викторине нужен верный ответ: correct — номера вариантов с 0."}
	}
	if len(p.Correct) > 1 && !p.Multiple {
		return &Bad{"Верных ответов несколько — тогда нужен multiple=true."}
	}
	for _, c := range p.Correct {
		if c < 0 || c >= len(p.Options) {
			return &Bad{fmt.Sprintf("correct: варианта %d нет, номера — с 0 до %d.", c, len(p.Options)-1)}
		}
	}
	slices.Sort(p.Correct)
	p.Correct = slices.Compact(p.Correct)
	if n := len([]rune(p.Solution)); n > 200 {
		return &Bad{fmt.Sprintf("Пояснение — %d символов, Telegram принимает до 200.", n)}
	}
	return nil
}

// DraftPoll — tg_draft_poll: черновик опроса. Как DraftMessage — ничего не
// отправляет само, в чате с автоотправкой ставит в очередь с окном на отмену.
func DraftPoll(ctx context.Context, s *config.Settings, chat string, p outbox.PollSpec, replyTo *int64, note string, topic int) (*omap.Map, error) {
	rule, err := sendable(s, chat)
	if err != nil {
		return nil, err
	}
	if n := netOf(s); n != nil {
		return nil, &Bad{"Опросы пока только в Telegram: в " + n.name() + " tg-agent их не создаёт."}
	}
	p.Options = slices.Clone(p.Options)
	if err := checkPoll(&p); err != nil {
		return nil, err
	}
	topic, topicName, err := draftTopic(ctx, s, rule, topic, replyTo)
	if err != nil {
		return nil, err
	}
	opts := outbox.CreateOpts{ID: outbox.NewID(), Chat: rule.Alias, ReplyTo: replyTo, TTLMin: s.DraftTTLMin,
		Note: note, Origin: draftOrigin(s), Topic: topic, TopicTitle: topicName, Poll: &p}
	auto := rule.AutoSend()
	if auto {
		at := time.Now().Add(time.Duration(s.AutoSendDelaySec) * time.Second)
		opts.SendAt = &at
	}
	d, err := outbox.New(s.OutboxPath()).Create(opts)
	if err != nil {
		return nil, err
	}
	kv := []any{"draft_id", d.ID, "chat", rule.Alias, "poll", p.Question, "options", len(p.Options),
		"anonymous", p.Anonymous, "quiz", p.Quiz}
	if topic != 0 {
		kv = append(kv, "topic", topic)
	}
	if auto {
		kv = append(kv, "auto_send_at", *d.SendAt)
	}
	logEvent(s, "draft_poll", kv...)
	out := omap.New().Set("draft_id", d.ID).Set("chat", s.Ref(rule.Alias))
	if topic != 0 {
		out.Set("topic", omap.New().Set("id", topic).Set("title", topicName))
	}
	out.Set("poll", d.Poll).Set("reply_to", d.ReplyTo).Set("expires_at", d.ExpiresAt)
	return finishDraft(ctx, s, rule, d, out, note, auto)
}

// PollVotes — tg_poll_votes: кто за какой вариант проголосовал.
func PollVotes(ctx context.Context, s *config.Settings, chat string, messageID, option, limit int) (*omap.Map, error) {
	rule, err := readable(s, chat)
	if err != nil {
		return nil, err
	}
	if !s.IsTelegram() {
		return nil, &Bad{"Опросы пока только в Telegram."}
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var out *omap.Map
	err = tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		out, err = c.PollVotes(ctx, t, messageID, option, limit)
		var np *tgc.NotPoll
		if errors.As(err, &np) {
			return &Bad{np.Error()}
		}
		switch tgc.RPCMessage(err) {
		case "":
		case "POLL_VOTE_REQUIRED":
			return &Bad{"Telegram показывает голоса, только когда владелец сам проголосовал в этом опросе."}
		default:
			return &Bad{"Telegram отказал: " + err.Error()}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	logEvent(s, "poll_votes", "chat", rule.Alias, "message_id", messageID)
	return omap.New().Set("chat", s.Ref(rule.Alias)).Set("message_id", messageID).Merge(out), nil
}

// pollOut — опрос черновика для tgc.
func pollOut(p *outbox.PollSpec) tgc.PollOut {
	return tgc.PollOut{Question: p.Question, Options: p.Options, Anonymous: p.Anonymous,
		Multiple: p.Multiple, Quiz: p.Quiz, Correct: p.Correct, Solution: p.Solution}
}

// DraftVote — tg_vote: голос владельца в опросе. Голос («приду», «беру») —
// обязательство от имени владельца, поэтому он идёт тем же путём, что
// сообщение: карточка или автоотправка с окном на отмену.
// options пусто и retract — снять голос.
func DraftVote(ctx context.Context, s *config.Settings, chat string, messageID int64, options []int, retract bool, note string) (*omap.Map, error) {
	rule, err := sendable(s, chat)
	if err != nil {
		return nil, err
	}
	if !s.IsTelegram() {
		return nil, &Bad{"Опросы пока только в Telegram."}
	}
	if retract {
		options = nil
	} else if len(options) == 0 {
		return nil, &Bad{"options — номера вариантов с 0 (или retract=true, чтобы снять голос)."}
	}
	options = slices.Clone(options)
	slices.Sort(options)
	options = slices.Compact(options)
	var info *omap.Map
	err = tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		_, info, err = c.PollOptions(ctx, t, int(messageID))
		var np *tgc.NotPoll
		if errors.As(err, &np) {
			return &Bad{np.Error()}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if closed, _ := info.Get("closed"); closed == true {
		return nil, &Bad{"Опрос закрыт — голосовать в нём нельзя."}
	}
	if multi, _ := info.Get("multiple"); multi != true && len(options) > 1 {
		return nil, &Bad{"В этом опросе можно выбрать только один вариант."}
	}
	q, _ := info.Get("question")
	all, _ := info.Get("options")
	opts, _ := all.([]*omap.Map)
	labels := make([]string, 0, len(options))
	for _, i := range options {
		if i < 0 || i >= len(opts) {
			return nil, &Bad{fmt.Sprintf("Варианта %d нет: номера — с 0 до %d.", i, len(opts)-1)}
		}
		text, _ := opts[i].Get("text")
		labels = append(labels, fmt.Sprint(text))
	}
	if options == nil {
		options = []int{}
	}
	v := &outbox.VoteSpec{MessageID: messageID, Options: options, Question: fmt.Sprint(q), Labels: labels}
	opts2 := outbox.CreateOpts{ID: outbox.NewID(), Chat: rule.Alias, TTLMin: s.DraftTTLMin, Note: note,
		Origin: draftOrigin(s), Vote: v}
	auto := rule.AutoSend()
	if auto {
		at := time.Now().Add(time.Duration(s.AutoSendDelaySec) * time.Second)
		opts2.SendAt = &at
	}
	d, err := outbox.New(s.OutboxPath()).Create(opts2)
	if err != nil {
		return nil, err
	}
	kv := []any{"draft_id", d.ID, "chat", rule.Alias, "message_id", messageID, "options", options}
	if auto {
		kv = append(kv, "auto_send_at", *d.SendAt)
	}
	logEvent(s, "draft_vote", kv...)
	out := omap.New().Set("draft_id", d.ID).Set("chat", s.Ref(rule.Alias)).Set("vote", d.Vote).
		Set("expires_at", d.ExpiresAt)
	return finishDraft(ctx, s, rule, d, out, note, auto)
}
