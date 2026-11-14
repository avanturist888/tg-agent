package tgc

import (
	"fmt"
	"strings"

	"github.com/gotd/td/tg"

	"tgagent/internal/omap"
)

// setTodo preserves the checklist snapshot and its service-message deltas.
// A delta does not imply the current state of its parent list: read reply_to.
func setTodo(out *omap.Map, message tg.MessageClass) {
	var preview string
	switch m := message.(type) {
	case *tg.Message:
		media, ok := m.Media.(*tg.MessageMediaToDo)
		if !ok {
			return
		}
		completed := make(map[int]tg.TodoCompletion, len(media.Completions))
		for _, c := range media.Completions {
			completed[c.ID] = c
		}
		items := make([]*omap.Map, 0, len(media.Todo.List))
		lines := []string{media.Todo.Title.Text}
		for _, item := range media.Todo.List {
			c, done := completed[item.ID]
			row := todoItem(item).Set("completed", done)
			mark := " "
			if done {
				mark = "x"
				row.Set("completed_at", isoTime(c.Date))
				if c.CompletedBy != nil {
					row.Set("completed_by_id", MarkedPeerID(c.CompletedBy))
				}
			}
			items = append(items, row)
			lines = append(lines, fmt.Sprintf("[%s] #%d %s", mark, item.ID, item.Title.Text))
		}
		info := omap.New().Set("title", media.Todo.Title.Text).Set("items", items).
			Set("others_can_append", media.Todo.OthersCanAppend).
			Set("others_can_complete", media.Todo.OthersCanComplete)
		if len(media.Todo.Title.Entities) > 0 {
			info.Set("title_entities", todoEntities(media.Todo.Title.Entities))
		}
		out.Set("todo", info)
		preview = strings.Join(lines, "\n")
	case *tg.MessageService:
		switch a := m.Action.(type) {
		case *tg.MessageActionTodoAppendTasks:
			items := make([]*omap.Map, 0, len(a.List))
			lines := []string{"Добавлены пункты списка:"}
			for _, item := range a.List {
				items = append(items, todoItem(item))
				lines = append(lines, fmt.Sprintf("#%d %s", item.ID, item.Title.Text))
			}
			out.Set("todo_action", omap.New().Set("type", "append").Set("items", items))
			preview = strings.Join(lines, "\n")
		case *tg.MessageActionTodoCompletions:
			// Empty vectors must remain JSON arrays, not unknown/null state.
			completed := append([]int{}, a.Completed...)
			incompleted := append([]int{}, a.Incompleted...)
			out.Set("todo_action", omap.New().Set("type", "completion").
				Set("completed_ids", completed).Set("incompleted_ids", incompleted))
			preview = fmt.Sprintf("Пункты выполнены: %v; возвращены в невыполненные: %v", completed, incompleted)
		default:
			return
		}
	default:
		return
	}
	if text, _ := out.Get("text"); text == "" {
		out.Set("text", preview)
	}
}

func todoItem(item tg.TodoItem) *omap.Map {
	row := omap.New().Set("id", item.ID).Set("text", item.Title.Text)
	if len(item.Title.Entities) > 0 {
		row.Set("entities", todoEntities(item.Title.Entities))
	}
	return row
}

// Keep entity offsets in Telegram's UTF-16 units. In particular a text-link's
// URL may be absent from the visible title, yet necessary to understand a task.
func todoEntities(entities []tg.MessageEntityClass) []*omap.Map {
	rows := make([]*omap.Map, 0, len(entities))
	for _, entity := range entities {
		row := omap.New().Set("type", typeName(entity)).Set("offset", entity.GetOffset()).Set("length", entity.GetLength())
		switch e := entity.(type) {
		case *tg.MessageEntityTextURL:
			row.Set("url", e.URL)
		case *tg.MessageEntityMentionName:
			row.Set("user_id", e.UserID)
		case *tg.MessageEntityCustomEmoji:
			row.Set("document_id", fmt.Sprint(e.DocumentID))
		case *tg.MessageEntityPre:
			row.Set("language", e.Language)
		case *tg.MessageEntityFormattedDate:
			row.Set("date", e.Date).Set("relative", e.Relative).
				Set("short_time", e.ShortTime).Set("long_time", e.LongTime).
				Set("short_date", e.ShortDate).Set("long_date", e.LongDate).
				Set("day_of_week", e.DayOfWeek)
		case *tg.MessageEntityDiffReplace:
			row.Set("old_text", e.OldText)
		case *tg.MessageEntityBlockquote:
			row.Set("collapsed", e.Collapsed)
		}
		rows = append(rows, row)
	}
	return rows
}
