package core

// Заметки к аккаунтам: что агенту знать об аккаунте — чей он, от чьего имени
// там пишут, каким тоном, что важно. Пишут владелец (бот: /notes, /note;
// окно управления) и агенты (tg_account_note). Агенты видят их в
// tg_list_chats.
//
// Заметки одни на установку — таблица account_notes в state.db основного
// профиля: бот и окно видят заметки всех аккаунтов, не спрашивая их служб,
// а служба любого аккаунта пишет туда транзакцией.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/store"
	"tgagent/internal/tgc"
)

const (
	noteMaxRunes = 2000
	notesPerAcct = 50
	noteByOwner  = "owner"
	noteByAgent  = "agent"
)

// AccountNote — одна заметка.
type AccountNote struct {
	ID   string    `json:"id"`
	Text string    `json:"text"`
	By   string    `json:"by"`             // owner / agent
	From string    `json:"from,omitempty"` // подпись агента: проект, задача
	At   time.Time `json:"at"`
}

// Author — кто оставил, по-человечески.
func (n AccountNote) Author() string {
	if n.By == noteByOwner {
		return "владелец"
	}
	if n.From != "" {
		return "агент (" + n.From + ")"
	}
	return "агент"
}

// Заметки — одни на установку: в базе основного профиля.
var notesTab = store.NewTable[[]AccountNote](store.TAccountNotes)

func notesDB() (*store.DB, error) {
	return store.Open(filepath.Join(config.HomeOf(config.MainAccount), "data"))
}

func loadNotes() map[string][]AccountNote {
	db, err := notesDB()
	if err != nil {
		return map[string][]AccountNote{}
	}
	m, err := notesTab.All(context.Background(), db.Q())
	if err != nil {
		return map[string][]AccountNote{}
	}
	return m
}

func editNotes(fn func(map[string][]AccountNote) error) error {
	db, err := notesDB()
	if err != nil {
		return err
	}
	return db.Write(context.Background(), func(tx *sql.Tx) error {
		return notesTab.EditTx(context.Background(), tx, func(m map[string][]AccountNote) (bool, error) {
			if err := fn(m); err != nil {
				return false, err
			}
			for k, v := range m {
				if len(v) == 0 {
					delete(m, k) // аккаунт без заметок — без записи
				}
			}
			return true, nil
		})
	})
}

// NotesOf — заметки аккаунта, старые сверху.
func NotesOf(account string) []AccountNote {
	return loadNotes()[account]
}

// AddNote — новая заметка к аккаунту.
func AddNote(account, text, by, from string) (AccountNote, error) {
	acct := config.FindAccount(account)
	if acct == "" {
		return AccountNote{}, &Bad{Msg: fmt.Sprintf("Аккаунта '%s' нет. Аккаунты: %s.", account,
			strings.Join(config.Accounts(), ", "))}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return AccountNote{}, &Bad{Msg: "Пустая заметка."}
	}
	if n := len([]rune(text)); n > noteMaxRunes {
		return AccountNote{}, &Bad{Msg: fmt.Sprintf("Слишком длинно: %d символов, предел — %d.", n, noteMaxRunes)}
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	note := AccountNote{ID: hex.EncodeToString(b), Text: text, By: by, From: strings.TrimSpace(from), At: time.Now().UTC()}
	err := editNotes(func(all map[string][]AccountNote) error {
		if len(all[acct]) >= notesPerAcct {
			return &Bad{Msg: fmt.Sprintf("У аккаунта %s уже %d заметок — удали лишние.", acct, notesPerAcct)}
		}
		all[acct] = append(all[acct], note)
		return nil
	})
	return note, err
}

// DeleteNote — убрать заметку; onlyAgents — агент может убрать только
// заметку агента, не владельца. Вернёт аккаунт заметки.
func DeleteNote(id string, onlyAgents bool) (string, error) {
	id = strings.TrimSpace(id)
	var acct string
	err := editNotes(func(all map[string][]AccountNote) error {
		for a, list := range all {
			for i, n := range list {
				if n.ID != id {
					continue
				}
				if onlyAgents && n.By != noteByAgent {
					return &Denied{Msg: "Заметку владельца агент не удаляет — попроси владельца."}
				}
				acct = a
				all[a] = append(list[:i:i], list[i+1:]...)
				if len(all[a]) == 0 {
					delete(all, a)
				}
				return nil
			}
		}
		return &NotFound{Msg: "Заметки " + id + " нет."}
	})
	return acct, err
}

// deleteNoteFor — убрать заметку, только если она принадлежит одному из allowed аккаунтов.
func deleteNoteFor(id string, allowed []string) (string, error) {
	id = strings.TrimSpace(id)
	var acct string
	err := editNotes(func(all map[string][]AccountNote) error {
		for a, list := range all {
			for i, n := range list {
				if n.ID != id {
					continue
				}
				if !slices.Contains(allowed, a) {
					return &Denied{Msg: "Эта кнопка не для вас."}
				}
				acct = a
				all[a] = append(list[:i:i], list[i+1:]...)
				if len(all[a]) == 0 {
					delete(all, a)
				}
				return nil
			}
		}
		return &NotFound{Msg: "Заметки " + id + " нет."}
	})
	return acct, err
}

// notesView — заметки для агента.
func notesView(notes []AccountNote) []*omap.Map {
	out := make([]*omap.Map, 0, len(notes))
	for _, n := range notes {
		out = append(out, omap.New().Set("id", n.ID).Set("text", n.Text).Set("by", n.Author()).
			Set("at", n.At.Local().Format("2006-01-02")))
	}
	return out
}

// AccountNoteTool — tg_account_note: добавить заметку или убрать свою.
func AccountNoteTool(ctx context.Context, s *config.Settings, account, text, deleteID, from string) (*omap.Map, error) {
	if err := onlyLocal(s); err != nil {
		return nil, err
	}
	if account == "" {
		account = config.AccountName()
	}
	if deleteID != "" {
		acct, err := DeleteNote(deleteID, true)
		if err != nil {
			return nil, err
		}
		logEvent(s, "account_note_deleted", "account", acct, "note_id", deleteID, "by", "agent")
		return omap.New().Set("account", acct).Set("deleted", deleteID).Set("notes", notesView(NotesOf(acct))), nil
	}
	note, err := AddNote(account, text, noteByAgent, from)
	if err != nil {
		return nil, err
	}
	acct := config.FindAccount(account)
	logEvent(s, "account_note_added", "account", acct, "note_id", note.ID, "by", "agent", "chars", len([]rune(note.Text)))
	return omap.New().Set("account", acct).Set("added", note.ID).Set("notes", notesView(NotesOf(acct))), nil
}

// ── кто владелец аккаунта ───────────────────────────────────────────────

// accountDB — база профиля аккаунта (в ней kv account — чей он).
func accountDB(account string) (*store.DB, error) {
	return store.Open(filepath.Join(config.HomeOf(account), "data"))
}

// saveAccountMeta сохраняет сведения об аккаунте в kv account текущего профиля.
// При merge=true непустые прежние поля сохраняются, если в новом снимке они пусты.
func saveAccountMeta(ctx context.Context, meta config.AccountMeta, merge bool) {
	meta.Phone = config.NormPhone(meta.Phone)
	db, err := accountDB(config.AccountName())
	if err != nil {
		return
	}
	_ = db.Write(ctx, func(tx *sql.Tx) error {
		var prev config.AccountMeta
		hasPrev, err := store.GetKV(ctx, tx, store.KeyAccount, &prev)
		if err != nil {
			return err
		}
		if merge && hasPrev {
			if meta.ID == 0 {
				meta.ID = prev.ID
			}
			if meta.Phone == "" {
				meta.Phone = prev.Phone
			}
			if meta.FirstName == "" {
				meta.FirstName = prev.FirstName
			}
			if meta.LastName == "" {
				meta.LastName = prev.LastName
			}
			if meta.Username == "" {
				meta.Username = prev.Username
			}
		}
		if hasPrev && meta == prev {
			return nil
		}
		return store.PutKV(ctx, tx, store.KeyAccount, meta)
	})
}

// SyncTelegramAccount обновляет kv account при подключении службы Telegram,
// чтобы ID, телефон и имя оставались актуальными даже после повторного входа.
func SyncTelegramAccount(ctx context.Context, c *tgc.Conn) {
	if c == nil {
		return
	}
	me, err := c.Self(ctx)
	if err != nil {
		return
	}
	saveAccountMeta(ctx, config.AccountMeta{
		ID:        me.ID,
		Phone:     config.NormPhone(me.Phone),
		FirstName: me.FirstName,
		LastName:  me.LastName,
		Username:  me.Username,
	}, false)
}

// AccountLabel — чей аккаунт (имя и @username); "" — ещё не знаем.
func AccountLabel(account string) string {
	meta, ok := config.AccountMetaOf(account)
	if !ok {
		return ""
	}
	return meta.Label()
}

// selfLabel — то же для своего аккаунта; первый раз спрашивает Telegram.
func selfLabel(ctx context.Context, s *config.Settings) string {
	if n := netOf(s); n != nil {
		return n.selfLabel()
	}
	if l := AccountLabel(config.AccountName()); l != "" {
		return l
	}
	var u config.AccountMeta
	err := tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		me, err := c.Self(ctx)
		if err != nil {
			return err
		}
		u = config.AccountMeta{
			ID:        me.ID,
			Phone:     config.NormPhone(me.Phone),
			FirstName: me.FirstName,
			LastName:  me.LastName,
			Username:  me.Username,
		}
		return nil
	})
	if err != nil {
		var nl *tgc.NotLoggedIn
		if errors.As(err, &nl) {
			return "(не выполнен вход)"
		}
		return ""
	}
	saveAccountMeta(ctx, u, false)
	return u.Label()
}
