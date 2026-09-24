//go:build !windows

package svc

import (
	"context"
	"net"
	"os"
	"path/filepath"

	"tgagent/internal/config"
)

func address(id string) string { return filepath.Join(config.Root, "data", "tg-agent-"+id+".sock") }

func listen(addr string) (net.Listener, error) {
	if conn, err := net.Dial("unix", addr); err == nil {
		conn.Close()
		return nil, os.ErrExist
	}
	_ = os.Remove(addr) // сокет мёртвой службы
	ln, err := net.Listen("unix", addr)
	if err == nil {
		_ = os.Chmod(addr, 0o600)
	}
	return ln, err
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", addr)
}
