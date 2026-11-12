package core

import (
	"tgagent/internal/config"
	"tgagent/internal/store"
)

// stateDB — база состояния профиля (data/state.db).
func stateDB(s *config.Settings) (*store.DB, error) { return store.Open(s.DataDir()) }
