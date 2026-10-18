package t3

// Протокол оркестрации 2 (T3 Code с 16.10.2026). HTTP API в нём только
// читает; команды идут по WebSocket /ws как RPC Effect: каждое сообщение —
// JSON {"_tag":"Request","id","tag","payload","headers"}, ответ —
// {"_tag":"Exit","requestId","exit":{"_tag":"Success"|"Failure",…}}.
// Подключение: одноразовый тикет POST /api/auth/websocket-ticket (тем же
// токеном) → /ws?orchestrationProtocol=2&wsTicket=….
//
// Сообщение в тред — команда message.dispatch с deliveryIntent "steer":
// сервер сам смотрит на тред — идёт ход, и сообщение уходит в него
// (steer_active, агент видит его сразу, а не после хода), нет — начинает
// ход (start_immediately).
//
// База десктопа — statev2.sqlite. Сессия Claude у треда —
// orchestration_v2_projection_provider_threads.payload_json
// $.nativeThreadRef.nativeId; сессии до переезда на v2 остались в
// provider_session_runtime (тред у них тот же).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const protocolV2 = 2

// ThreadOf — тред T3, в котором живёт сессия Claude Code (по базе десктопа,
// без запросов к серверу). NoThread — сессия не из T3, тред удалён или в архиве.
func ThreadOf(ctx context.Context, session string) (string, error) {
	h := home()
	if p := filepath.Join(h, "userdata", "statev2.sqlite"); exists(p) {
		return threadV2(ctx, p, session)
	}
	if p := filepath.Join(h, "userdata", "state.sqlite"); exists(p) {
		return threadV1(ctx, p, session)
	}
	return "", Unavailable
}

// CurrentSession — сессия Claude, в которой тред работает сейчас (последняя
// из его сессий). "" — не известна.
func CurrentSession(ctx context.Context, thread string) string {
	p := filepath.Join(home(), "userdata", "statev2.sqlite")
	if !exists(p) {
		return ""
	}
	db, err := openRO(p)
	if err != nil {
		return ""
	}
	defer db.Close()
	var native sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT json_extract(payload_json, '$.nativeThreadRef.nativeId')
		FROM orchestration_v2_projection_provider_threads
		WHERE thread_id = ? AND owner_node_id IS NULL ORDER BY updated_at DESC LIMIT 1`, thread).Scan(&native)
	return native.String
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func openRO(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(3000)")
}

func threadV1(ctx context.Context, path, session string) (string, error) {
	db, err := openRO(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var thread string
	err = db.QueryRowContext(ctx, `SELECT thread_id FROM provider_session_runtime
		WHERE json_extract(resume_cursor_json, '$.resume') = ? ORDER BY last_seen_at DESC LIMIT 1`, session).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", NoThread
	}
	if err != nil {
		return "", fmt.Errorf("база T3: %w", err)
	}
	return thread, nil
}

func threadV2(ctx context.Context, path, session string) (string, error) {
	db, err := openRO(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var thread string
	err = db.QueryRowContext(ctx, `SELECT thread_id FROM orchestration_v2_projection_provider_threads
		WHERE json_extract(payload_json, '$.nativeThreadRef.nativeId') = ? AND thread_id IS NOT NULL
		ORDER BY updated_at DESC LIMIT 1`, session).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		// сессия старше переезда на v2
		err = db.QueryRowContext(ctx, `SELECT thread_id FROM provider_session_runtime
			WHERE json_extract(resume_cursor_json, '$.resume') = ? ORDER BY last_seen_at DESC LIMIT 1`, session).Scan(&thread)
		if err != nil && strings.Contains(err.Error(), "no such table") {
			err = sql.ErrNoRows
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", NoThread
	}
	if err != nil {
		return "", fmt.Errorf("база T3: %w", err)
	}
	var archived, deleted sql.NullString
	err = db.QueryRowContext(ctx, `SELECT archived_at, deleted_at FROM orchestration_v2_projection_threads
		WHERE thread_id = ?`, thread).Scan(&archived, &deleted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", NoThread
	case err != nil:
		return "", fmt.Errorf("база T3: %w", err)
	case archived.Valid || deleted.Valid:
		return "", NoThread // владелец тред закрыл — не тревожим
	}
	return thread, nil
}

// sendV2 — сообщение text в тред: в идущий ход или новым ходом.
func (e *Env) sendV2(ctx context.Context, thread, text string) error {
	return e.dispatchV2(ctx, map[string]any{
		"type":           "message.dispatch",
		"createdBy":      "user", // сервер всё равно ставит user
		"creationSource": "server",
		"commandId":      randomID(),
		"threadId":       thread,
		"messageId":      randomID(),
		"text":           text,
		"attachments":    []any{},
		"deliveryIntent": "steer",
		// без deliveryIntent (старый сервер) — просто начать ход
		"dispatchMode": map[string]string{"type": "start_immediately"},
	})
}

// dispatchV2 — команда оркестрации по WebSocket. ThreadGone — треда нет.
func (e *Env) dispatchV2(ctx context.Context, cmd map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if err := e.api(ctx, http.MethodPost, "/api/auth/websocket-ticket", nil, &tk); err != nil {
		return fmt.Errorf("тикет WebSocket T3: %w", err)
	}
	if tk.Ticket == "" {
		return errors.New("T3 не выдал тикет WebSocket")
	}
	u, err := url.Parse(strings.TrimRight(e.Origin, "/") + "/ws")
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.RawQuery = url.Values{"orchestrationProtocol": {fmt.Sprint(protocolV2)}, "wsTicket": {tk.Ticket}}.Encode()
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return fmt.Errorf("WebSocket T3: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	req, err := json.Marshal(map[string]any{"_tag": "Request", "id": "1",
		"tag": "orchestration.dispatchCommand", "payload": cmd, "headers": []any{}})
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, req); err != nil {
		return fmt.Errorf("WebSocket T3: %w", err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("WebSocket T3: %w", err)
		}
		for _, m := range rpcMessages(raw) {
			switch {
			case m.Tag == "Exit" && m.RequestID == "1":
				conn.Close(websocket.StatusNormalClosure, "")
				return exitError(m.Exit)
			case m.Tag == "Defect":
				return fmt.Errorf("T3: сбой RPC: %s", tail(string(m.Defect), 300))
			case m.Tag == "Ping":
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"_tag":"Pong"}`))
			}
		}
	}
}

type rpcMessage struct {
	Tag       string          `json:"_tag"`
	RequestID string          `json:"requestId"`
	Exit      json.RawMessage `json:"exit"`
	Defect    json.RawMessage `json:"defect"`
}

// rpcMessages — кадр RPC: одно сообщение или массив.
func rpcMessages(raw []byte) []rpcMessage {
	var many []rpcMessage
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var one rpcMessage
	if json.Unmarshal(raw, &one) == nil {
		return []rpcMessage{one}
	}
	return nil
}

// exitError — nil для Success, иначе текст отказа (ThreadGone — нет треда).
func exitError(raw json.RawMessage) error {
	var exit struct {
		Tag   string `json:"_tag"`
		Cause []struct {
			Tag    string          `json:"_tag"`
			Error  json.RawMessage `json:"error"`
			Defect json.RawMessage `json:"defect"`
		} `json:"cause"`
	}
	if err := json.Unmarshal(raw, &exit); err != nil {
		return fmt.Errorf("T3: непонятный ответ RPC: %s", tail(string(raw), 300))
	}
	if exit.Tag == "Success" {
		return nil
	}
	if strings.Contains(string(raw), "ProjectionStoreThreadNotFoundError") {
		return ThreadGone
	}
	for _, c := range exit.Cause {
		var e struct {
			Message string `json:"message"`
		}
		if c.Tag == "Fail" && json.Unmarshal(c.Error, &e) == nil && e.Message != "" {
			return fmt.Errorf("T3 отклонил команду: %s", e.Message)
		}
		if c.Tag == "Die" && len(c.Defect) > 0 {
			return fmt.Errorf("T3 отклонил команду: %s", tail(string(c.Defect), 300))
		}
	}
	return fmt.Errorf("T3 отклонил команду: %s", tail(string(raw), 300))
}
