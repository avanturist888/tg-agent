package config

// Агенты в контейнерах.
//
// config/t3.toml — окружения T3 Code, кроме местного десктопа: серверы
// `t3 serve` в контейнерах. Служба заводит там свои треды и будит их.
//
// config/agents.toml — агенты, которые ходят к инструментам по HTTP (из
// контейнера именованный канал ноутбука не виден): имя, переменная .env с
// bearer-токеном, свои чаты (подмножество белого списка) и окружение T3.
//
// Оба файла необязательные и перечитываются на каждом обращении.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// LocalEnv — имя местного десктопа T3 Code (в t3.toml его не описывают).
const LocalEnv = "local"

// DefaultListen — адрес HTTP-транспорта MCP по умолчанию: из контейнера на
// этом же хосте он виден как host.docker.internal:8790.
const DefaultListen = "127.0.0.1:8790"

// T3Env — окружение T3 Code из config/t3.toml.
type T3Env struct {
	Name           string   `toml:"name"`
	Origin         string   `toml:"origin"`
	Model          string   `toml:"model"`
	Project        string   `toml:"project"`
	TokenFile      string   `toml:"token_file"`
	PairingCmd     []string `toml:"pairing_cmd"`
	AdminTokenEnv  string   `toml:"admin_token_env"`
	TurnTimeoutSec float64  `toml:"turn_timeout_sec"`
	PollSec        float64  `toml:"poll_sec"`
}

// AdminToken — admin-токен окружения из .env ("" — не задан).
func (e T3Env) AdminToken() string {
	if e.AdminTokenEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(e.AdminTokenEnv))
}

// TokenPath — где служба хранит выпущенный для окружения токен.
func (e T3Env) TokenPath() string {
	return filepath.Join(Root, "data", "t3-token-"+e.Name+".json")
}

func T3Path() string     { return filepath.Join(Root, "config", "t3.toml") }
func AgentsPath() string { return filepath.Join(Root, "config", "agents.toml") }

// LoadT3 — окружения из config/t3.toml (нет файла — пусто).
func LoadT3() ([]T3Env, error) {
	applyDotenv(filepath.Join(Root, ".env"))
	path := T3Path()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw struct {
		Env []T3Env `toml:"env"`
	}
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, cfgErr("%s: %v", path, err)
	}
	seen := map[string]bool{}
	for i := range raw.Env {
		e := &raw.Env[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Origin = strings.TrimRight(strings.TrimSpace(e.Origin), "/")
		switch {
		case e.Name == "":
			return nil, cfgErr("%s: у одного из [[env]] нет name", path)
		case strings.EqualFold(e.Name, LocalEnv):
			return nil, cfgErr("%s: имя %q занято местным десктопом T3 Code", path, LocalEnv)
		case !safeName(e.Name):
			return nil, cfgErr("%s: name %q — только латиница, цифры, - и _", path, e.Name)
		case seen[strings.ToLower(e.Name)]:
			return nil, cfgErr("%s: окружение %q повторяется", path, e.Name)
		}
		seen[strings.ToLower(e.Name)] = true
		if u, err := url.Parse(e.Origin); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, cfgErr("%s: у окружения %q origin должен быть вида http://127.0.0.1:3774", path, e.Name)
		}
		if e.TokenFile == "" && len(e.PairingCmd) == 0 && e.AdminTokenEnv == "" {
			return nil, cfgErr("%s: у окружения %q нет способа получить токен: token_file, pairing_cmd или admin_token_env", path, e.Name)
		}
		if e.TokenFile != "" && !filepath.IsAbs(e.TokenFile) {
			e.TokenFile = filepath.Join(Root, e.TokenFile)
		}
	}
	return raw.Env, nil
}

// FindT3 — окружение по имени.
func FindT3(name string) (*T3Env, error) {
	envs, err := LoadT3()
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range envs {
		if strings.EqualFold(envs[i].Name, name) {
			return &envs[i], nil
		}
		names = append(names, envs[i].Name)
	}
	if len(names) == 0 {
		return nil, cfgErr("Окружение T3 %q не найдено: config/t3.toml пуст или его нет (образец — config/t3.example.toml).", name)
	}
	return nil, cfgErr("Окружение T3 %q не найдено в config/t3.toml. Есть: %s.", name, strings.Join(names, ", "))
}

// Agent — агент из контейнера: ходит к инструментам по HTTP со своим токеном.
type Agent struct {
	Name     string   `toml:"name"`
	TokenEnv string   `toml:"token_env"`
	Chats    []string `toml:"chats"`
	Env      string   `toml:"env"`
}

// Token — bearer-токен агента из .env ("" — не задан).
func (a Agent) Token() string {
	if a.TokenEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(a.TokenEnv))
}

// MinTokenLen — короче токен агента не принимаем.
const MinTokenLen = 24

// Agents — config/agents.toml.
type Agents struct {
	Listen string // адрес HTTP-транспорта MCP
	List   []Agent
}

// LoadAgents — агенты из config/agents.toml (нет файла — пусто).
func LoadAgents() (*Agents, error) {
	applyDotenv(filepath.Join(Root, ".env"))
	out := &Agents{Listen: DefaultListen}
	path := AgentsPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var raw struct {
		HTTP struct {
			Listen string `toml:"listen"`
		} `toml:"http"`
		Agent []Agent `toml:"agent"`
	}
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, cfgErr("%s: %v", path, err)
	}
	if l := strings.TrimSpace(raw.HTTP.Listen); l != "" {
		if _, _, err := net.SplitHostPort(l); err != nil {
			return nil, cfgErr("%s: [http] listen должен быть вида 127.0.0.1:8790", path)
		}
		out.Listen = l
	}
	seen := map[string]bool{}
	for _, a := range raw.Agent {
		a.Name = strings.TrimSpace(a.Name)
		a.Env = strings.TrimSpace(a.Env)
		switch {
		case a.Name == "":
			return nil, cfgErr("%s: у одного из [[agent]] нет name", path)
		case !safeName(a.Name):
			return nil, cfgErr("%s: name %q — только латиница, цифры, - и _", path, a.Name)
		case seen[strings.ToLower(a.Name)]:
			return nil, cfgErr("%s: агент %q повторяется", path, a.Name)
		case a.TokenEnv == "":
			return nil, cfgErr("%s: у агента %q нет token_env (имя переменной в .env с его токеном)", path, a.Name)
		}
		seen[strings.ToLower(a.Name)] = true
		out.List = append(out.List, a)
	}
	return out, nil
}

// Restrict — права агента из контейнера: из белого списка остаются только
// его чаты (по alias). Агенту остальных чатов не существует.
func (s *Settings) Restrict(a *Agent) *Settings {
	cp := *s
	cp.Agent = a
	cp.Chats = map[string]ChatRule{}
	cp.ChatOrder = nil
	allowed := map[string]bool{}
	for _, c := range a.Chats {
		allowed[strings.ToLower(strings.TrimSpace(c))] = true
	}
	for _, alias := range s.ChatOrder {
		if allowed[strings.ToLower(alias)] {
			cp.Chats[alias] = s.Chats[alias]
			cp.ChatOrder = append(cp.ChatOrder, alias)
		}
	}
	return &cp
}

// AgentName — имя агента из контейнера ("" — местный агент или владелец).
func (s *Settings) AgentName() string {
	if s.Agent == nil {
		return ""
	}
	return s.Agent.Name
}

func safeName(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return s != ""
}

// String — для отладки без токена.
func (a Agent) String() string {
	return fmt.Sprintf("%s (чаты %s, окружение %s)", a.Name, strings.Join(a.Chats, ", "), a.Env)
}
