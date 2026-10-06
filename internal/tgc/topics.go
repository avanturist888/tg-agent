package tgc

// Темы форумов (супергруппа с флагом forum).
//
// Тема — это id её служебного сообщения «тема создана»; у «General» id = 1.
// Сообщение в теме приходит с reply_to_msg_id = id темы и флагом
// forum_topic, а ответ внутри темы — ещё и с reply_to_top_id = id темы
// (reply_to_msg_id тогда — то, на что ответили). Без флага сообщение форума
// лежит в General. Отправка в тему — reply_to_msg_id = id темы; ответ внутри
// темы — reply_to_msg_id = сообщение и top_msg_id = тема (Target.Topic).

import (
	"context"
	"sync"
	"time"

	"github.com/gotd/td/tg"
)

// GeneralTopic — id темы «General».
const GeneralTopic = 1

// Topic — тема форума.
type Topic struct {
	ID         int
	Title      string
	Closed     bool
	Pinned     bool
	Hidden     bool
	Unread     int
	TopMessage int
}

const topicTTL = 10 * time.Minute

var topicCache = struct {
	sync.Mutex
	forum  map[int64]cached[bool]
	titles map[int64]map[int]cached[string]
}{forum: map[int64]cached[bool]{}, titles: map[int64]map[int]cached[string]{}}

type cached[T any] struct {
	v  T
	at time.Time
}

func (c cached[T]) fresh() bool { return time.Since(c.at) < topicTTL }

func replyHeader(m tg.MessageClass) (*tg.MessageReplyHeader, bool) {
	var r tg.MessageReplyHeaderClass
	switch v := m.(type) {
	case *tg.Message:
		r = v.ReplyTo
	case *tg.MessageService:
		r = v.ReplyTo
	}
	h, ok := r.(*tg.MessageReplyHeader)
	return h, ok
}

// TopicOf — тема сообщения форума (0 — чат не форум).
func TopicOf(m tg.MessageClass, forum bool) int {
	if !forum {
		return 0
	}
	if svc, ok := m.(*tg.MessageService); ok {
		if _, ok := svc.Action.(*tg.MessageActionTopicCreate); ok {
			return svc.ID
		}
	}
	if h, ok := replyHeader(m); ok && h.ForumTopic {
		if top, ok := h.GetReplyToTopID(); ok && top != 0 {
			return top
		}
		if id, ok := h.GetReplyToMsgID(); ok && id != 0 {
			return id
		}
	}
	return GeneralTopic
}

// ReplyID — на какое сообщение это ответ (0 — не ответ). В теме форума
// reply_to_msg_id без reply_to_top_id указывает на саму тему — это не ответ.
func ReplyID(m tg.MessageClass) int {
	h, ok := replyHeader(m)
	if !ok {
		return 0
	}
	id, ok := h.GetReplyToMsgID()
	if !ok {
		return 0
	}
	if h.ForumTopic {
		if top, ok := h.GetReplyToTopID(); !ok || top == 0 {
			return 0
		}
	}
	return id
}

func rememberForum(chats []tg.ChatClass) {
	topicCache.Lock()
	defer topicCache.Unlock()
	for _, ch := range chats {
		if c, ok := ch.(*tg.Channel); ok {
			topicCache.forum[c.ID] = cached[bool]{c.Forum, time.Now()}
		}
	}
}

// IsForum — чат с темами.
func (c *Conn) IsForum(ctx context.Context, t Target) (bool, error) {
	if t.Kind != "channel" {
		return false, nil
	}
	topicCache.Lock()
	f, ok := topicCache.forum[t.ID]
	topicCache.Unlock()
	if ok && f.fresh() {
		return f.v, nil
	}
	in, ok := t.Input.(*tg.InputPeerChannel)
	if !ok {
		return false, nil
	}
	res, err := c.API.ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: in.ChannelID, AccessHash: in.AccessHash}})
	if err != nil {
		return false, err
	}
	chats := res.GetChats()
	c.Peers.Remember(nil, chats)
	rememberForum(chats)
	for _, ch := range chats {
		if v, ok := ch.(*tg.Channel); ok && v.ID == t.ID {
			return v.Forum, nil
		}
	}
	return false, nil
}

func rememberTopics(chatID int64, topics []tg.ForumTopicClass) {
	topicCache.Lock()
	defer topicCache.Unlock()
	m := topicCache.titles[chatID]
	if m == nil {
		m = map[int]cached[string]{}
		topicCache.titles[chatID] = m
	}
	for _, tc := range topics {
		if t, ok := tc.(*tg.ForumTopic); ok {
			m[t.ID] = cached[string]{t.Title, time.Now()}
		}
	}
}

// TopicTitles — названия тем по id (удалённых и несуществующих в ответе нет).
func (c *Conn) TopicTitles(ctx context.Context, t Target, ids []int) (map[int]string, error) {
	out := map[int]string{}
	var missing []int
	topicCache.Lock()
	for _, id := range ids {
		if e, ok := topicCache.titles[t.ID][id]; ok && e.fresh() {
			out[id] = e.v
		} else if !containsInt(missing, id) {
			missing = append(missing, id)
		}
	}
	topicCache.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	res, err := c.API.MessagesGetForumTopicsByID(ctx, &tg.MessagesGetForumTopicsByIDRequest{Peer: t.Input, Topics: missing})
	if err != nil {
		return nil, err
	}
	c.Peers.Remember(res.Users, res.Chats)
	rememberTopics(t.ID, res.Topics)
	for _, tc := range res.Topics {
		if v, ok := tc.(*tg.ForumTopic); ok {
			out[v.ID] = v.Title
		}
	}
	return out, nil
}

// Topics — темы форума, сначала с недавней активностью.
func (c *Conn) Topics(ctx context.Context, t Target, limit int) ([]Topic, error) {
	var out []Topic
	req := &tg.MessagesGetForumTopicsRequest{Peer: t.Input}
	for len(out) < limit {
		req.Limit = min(100, limit-len(out))
		res, err := c.API.MessagesGetForumTopics(ctx, req)
		if err != nil {
			return nil, err
		}
		c.Peers.Remember(res.Users, res.Chats)
		rememberTopics(t.ID, res.Topics)
		dates := map[int]int{}
		for _, m := range res.Messages {
			dates[m.GetID()] = messageDate(m)
		}
		got := 0
		for _, tc := range res.Topics {
			v, ok := tc.(*tg.ForumTopic)
			if !ok {
				continue
			}
			got++
			out = append(out, Topic{ID: v.ID, Title: v.Title, Closed: v.Closed, Pinned: v.Pinned, Hidden: v.Hidden,
				Unread: v.UnreadCount, TopMessage: v.TopMessage})
			req.OffsetTopic, req.OffsetID, req.OffsetDate = v.ID, v.TopMessage, dates[v.TopMessage]
		}
		if got == 0 || len(res.Topics) < req.Limit || len(out) >= res.Count {
			break
		}
	}
	c.Peers.Save()
	return out, nil
}

// FillTopics — у форума: отметить выдачу и подтянуть названия тем её сообщений.
func (c *Conn) FillTopics(ctx context.Context, t Target, b *Batch) error {
	if t.Kind != "channel" || len(b.Messages) == 0 {
		return nil
	}
	forum := false
	if ch, ok := b.Chats[t.ID].(*tg.Channel); ok {
		forum = ch.Forum
	} else {
		var err error
		if forum, err = c.IsForum(ctx, t); err != nil {
			return err
		}
	}
	b.Forum = forum
	if !forum {
		return nil
	}
	var ids []int
	for _, m := range b.Messages {
		if id := TopicOf(m, true); !containsInt(ids, id) {
			ids = append(ids, id)
		}
	}
	titles, err := c.TopicTitles(ctx, t, ids)
	if err != nil {
		return err
	}
	b.Topics = titles
	return nil
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
