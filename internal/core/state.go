package core

import (
	"encoding/json"

	"tgagent/internal/config"
	"tgagent/internal/inbox"
	"tgagent/internal/store"
)

// stateDB — база состояния профиля (data/state.db).
func stateDB(s *config.Settings) (*store.DB, error) { return store.Open(s.DataDir()) }

// stateWaiting — почему состояние профиля ещё не всё в базе (перенос
// старых файлов ждёт tg cutover или отката); "" — всё на месте. Решения
// владельца, пока перенос ждёт, разбирать нельзя: черновика или запроса из
// старого файла в базе ещё нет, и нажатие ушло бы в «не найден».
func stateWaiting(s *config.Settings) string {
	db, err := stateDB(s)
	if err != nil {
		return err.Error()
	}
	return db.LegacyBlocked()
}

// Подписки и запросы доступа старой установки переносятся через свои типы:
// адрес сессии в них (inbox.Addr) ложится в базу с зашифрованным токеном,
// а не открытым, как в старых файлах.
// При откате (tg rollback) — обратно с открытым токеном (plainAgent):
// старая сборка зашифрованного не знает.
func init() {
	store.RegisterLegacy(store.TypedDocs[*subscription]("subscriptions.json", store.TSubscriptions, false, plainAgent))
	store.RegisterLegacy(store.TypedDocs[*accessRequest]("access_requests.json", store.TAccessRequests, false, plainAgent))
}

// plainAgent — тело записи с адресом сессии (поле agent) в виде старой
// сборки: токен канала открытым. Прочие поля — как есть.
func plainAgent(raw json.RawMessage) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	a, ok := m["agent"]
	if !ok || string(a) == "null" {
		return raw, nil
	}
	var addr inbox.Addr
	if err := json.Unmarshal(a, &addr); err != nil {
		return nil, err
	}
	p, err := addr.PlainJSON()
	if err != nil {
		return nil, err
	}
	m["agent"] = p
	return json.Marshal(m)
}
