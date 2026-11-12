package core

import (
	"tgagent/internal/config"
	"tgagent/internal/store"
)

// stateDB — база состояния профиля (data/state.db).
func stateDB(s *config.Settings) (*store.DB, error) { return store.Open(s.DataDir()) }

// Подписки и запросы доступа старой установки переносятся через свои типы:
// адрес сессии в них (inbox.Addr) ложится в базу с зашифрованным токеном,
// а не открытым, как в старых файлах.
func init() {
	store.RegisterLegacy(store.TypedDocs[*subscription]("subscriptions.json", store.TSubscriptions, false))
	store.RegisterLegacy(store.TypedDocs[*accessRequest]("access_requests.json", store.TAccessRequests, false))
}
