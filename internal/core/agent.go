package core

// Вызовы агентов из контейнеров (MCP по HTTP, mcpserver/http.go): у них
// s.Agent != nil, а белый список в s уже урезан до чатов агента
// (config.Settings.Restrict) — остальных чатов для них не существует.
// Файловая система шлюза агенту не видна, поэтому вложения с диска и
// скачивание на диск шлюза ему не даются.

import (
	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/outbox"
)

// logEvent — запись в аудит; у агента из контейнера — с его именем.
func logEvent(s *config.Settings, action string, kv ...any) {
	if name := s.AgentName(); name != "" {
		kv = append(kv, "agent", name)
	}
	_ = audit.Log(s.AuditPath(), action, kv...)
}

// visibleDraft — черновик чужого чата агенту из контейнера не виден вовсе.
func visibleDraft(s *config.Settings, d *outbox.Draft) error {
	if s.Agent == nil {
		return nil
	}
	if _, ok := s.Chats[d.Chat]; ok {
		return nil
	}
	return &outbox.NotFound{ID: d.ID}
}

// draftOrigin — кто создал черновик: на карточке видно имя агента из контейнера.
func draftOrigin(s *config.Settings) string {
	if name := s.AgentName(); name != "" {
		return "agent:" + name
	}
	return ""
}

const (
	remoteNoFiles = "files недоступны агенту из контейнера: пути указывают на диск шлюза, " +
		"а не на твой. Отправь текст без файлов или попроси владельца приложить файл самому."
	remoteNoDownload = "tg_download_file недоступен агенту из контейнера: файл лёг бы на диск шлюза, " +
		"а не к тебе. Картинки смотри через tg_view_media, голосовые — через tg_transcribe."
)
