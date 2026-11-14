package tgc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"tgagent/internal/omap"
)

// Pure serialization: no API, files, profiles, services, or runtime fixtures.
func serializedTodo(t *testing.T, m tg.MessageClass) map[string]any {
	t.Helper()
	c := &Conn{Peers: &PeerCache{data: newPeerData()}}
	raw, err := omap.Marshal(c.Serialize(m, &Batch{}))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTodoSnapshotSerialization(t *testing.T) {
	media := &tg.MessageMediaToDo{Todo: tg.TodoList{
		Title:           tg.TextWithEntities{Text: "План", Entities: []tg.MessageEntityClass{&tg.MessageEntityBold{Length: 4}}},
		OthersCanAppend: true,
		List: []tg.TodoItem{
			{ID: 2, Title: tg.TextWithEntities{Text: "Проверить"}},
			{ID: 9, Title: tg.TextWithEntities{Text: "🧪 ссылка", Entities: []tg.MessageEntityClass{&tg.MessageEntityTextURL{Offset: 3, Length: 6, URL: "https://example.test/task"}}}},
		},
	}}
	media.SetCompletions([]tg.TodoCompletion{{ID: 9, CompletedBy: &tg.PeerUser{UserID: 5}, Date: 1_800_000_000}})
	m := &tg.Message{ID: 42, Media: media, PeerID: &tg.PeerUser{UserID: 7}}
	out := serializedTodo(t, m)
	if out["media"] != "todo" || !strings.Contains(out["text"].(string), "[x] #9 🧪 ссылка") {
		t.Fatalf("unreadable checklist: %+v", out)
	}
	list := out["todo"].(map[string]any)
	if list["title"] != "План" || list["others_can_append"] != true || list["others_can_complete"] != false {
		t.Fatalf("list metadata: %+v", list)
	}
	items := list["items"].([]any)
	open, done := items[0].(map[string]any), items[1].(map[string]any)
	if open["id"] != float64(2) || open["completed"] != false || done["id"] != float64(9) || done["completed"] != true || done["completed_by_id"] != float64(5) || done["completed_at"] != isoTime(1_800_000_000) {
		t.Fatalf("item IDs/state: %+v", items)
	}
	if _, exists := open["completed_at"]; exists {
		t.Fatal("open item has completion time")
	}
	entity := done["entities"].([]any)[0].(map[string]any)
	if entity["url"] != "https://example.test/task" || entity["offset"] != float64(3) || entity["length"] != float64(6) {
		t.Fatalf("lost hidden URL or UTF-16 offset: %+v", entity)
	}
	m.Message = "caption"
	if got := serializedTodo(t, m)["text"]; got != "caption" {
		t.Fatalf("caption replaced: %v", got)
	}
}

func TestTodoAppendSerialization(t *testing.T) {
	m := &tg.MessageService{ID: 43, PeerID: &tg.PeerUser{UserID: 7}, ReplyTo: header(42, 0, false),
		Action: &tg.MessageActionTodoAppendTasks{List: []tg.TodoItem{{ID: 12, Title: tg.TextWithEntities{Text: "Новая задача"}}}}}
	out := serializedTodo(t, m)
	if out["service_action"] != "MessageActionTodoAppendTasks" || out["reply_to"] != float64(42) || !strings.Contains(out["text"].(string), "Новая задача") {
		t.Fatalf("append envelope: %+v", out)
	}
	action := out["todo_action"].(map[string]any)
	item := action["items"].([]any)[0].(map[string]any)
	if action["type"] != "append" || item["id"] != float64(12) || item["text"] != "Новая задача" {
		t.Fatalf("append content: %+v", action)
	}
	if _, exists := item["completed"]; exists {
		t.Fatal("append delta invents current completion state")
	}
}

func TestTodoCompletionsSerialization(t *testing.T) {
	m := &tg.MessageService{ID: 44, PeerID: &tg.PeerUser{UserID: 7}, ReplyTo: header(42, 0, false),
		Action: &tg.MessageActionTodoCompletions{Completed: []int{12, 2}, Incompleted: []int{9}}}
	out := serializedTodo(t, m)
	action := out["todo_action"].(map[string]any)
	if out["reply_to"] != float64(42) || action["type"] != "completion" || out["text"] == "" {
		t.Fatalf("completion envelope: %+v", out)
	}
	if got := action["completed_ids"].([]any); len(got) != 2 || got[0] != float64(12) || got[1] != float64(2) {
		t.Fatalf("completed IDs: %v", got)
	}
	if got := action["incompleted_ids"].([]any); len(got) != 1 || got[0] != float64(9) {
		t.Fatalf("reopened IDs: %v", got)
	}
	m.Action = &tg.MessageActionTodoCompletions{}
	action = serializedTodo(t, m)["todo_action"].(map[string]any)
	for _, key := range []string{"completed_ids", "incompleted_ids"} {
		if v, ok := action[key].([]any); !ok || len(v) != 0 {
			t.Fatalf("empty delta must be []: %+v", action)
		}
	}
}

func TestTodoEmptyAndUnrelatedMessages(t *testing.T) {
	out := serializedTodo(t, &tg.Message{ID: 42, PeerID: &tg.PeerUser{UserID: 7}, Media: &tg.MessageMediaToDo{}})
	if v, ok := out["todo"].(map[string]any)["items"].([]any); !ok || len(v) != 0 {
		t.Fatalf("empty checklist: %+v", out)
	}
	for _, m := range []tg.MessageClass{&tg.Message{ID: 1, Message: "ordinary"}, &tg.MessageService{ID: 2, Action: &tg.MessageActionTopicCreate{Title: "topic"}}} {
		out := serializedTodo(t, m)
		if _, exists := out["todo"]; exists {
			t.Fatal("todo added to unrelated message")
		}
		if _, exists := out["todo_action"]; exists {
			t.Fatal("delta added to unrelated message")
		}
		if out["text"] == "" {
			t.Fatalf("existing text lost: %+v", out)
		}
	}
}

func TestTodoEntityPayloads(t *testing.T) {
	media := &tg.MessageMediaToDo{Todo: tg.TodoList{Title: tg.TextWithEntities{
		Text: "Дата и правка",
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityFormattedDate{Offset: 0, Length: 4, Date: 1_800_000_000,
				Relative: true, ShortTime: true, LongDate: true, DayOfWeek: true},
			&tg.MessageEntityDiffReplace{Offset: 7, Length: 6, OldText: "прежнее"},
			&tg.MessageEntityBlockquote{Length: 13, Collapsed: true},
		},
	}}}
	out := serializedTodo(t, &tg.Message{ID: 45, Media: media})
	entities := out["todo"].(map[string]any)["title_entities"].([]any)
	date := entities[0].(map[string]any)
	if date["date"] != float64(1_800_000_000) {
		t.Fatalf("formatted date lost timestamp: %+v", date)
	}
	for key, want := range map[string]bool{"relative": true, "short_time": true, "long_time": false, "short_date": false, "long_date": true, "day_of_week": true} {
		if date[key] != want {
			t.Fatalf("date rendering flag %s: %+v", key, date)
		}
	}
	if entities[1].(map[string]any)["old_text"] != "прежнее" || entities[2].(map[string]any)["collapsed"] != true {
		t.Fatalf("entity payloads lost: %+v", entities)
	}
}
