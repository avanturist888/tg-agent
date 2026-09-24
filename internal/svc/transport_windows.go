//go:build windows

package svc

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func address(id string) string { return `\\.\pipe\tg-agent-` + id }

// listen — канал доступен только текущей учётной записи (и SYSTEM): чужие
// пользователи машины не могут ни подключиться, ни подменить службу.
func listen(addr string) (net.Listener, error) {
	sddl := "D:P(A;;GA;;;SY)"
	if user, err := windows.GetCurrentProcessToken().GetTokenUser(); err == nil {
		sddl = "D:P(A;;GA;;;" + user.User.Sid.String() + ")(A;;GA;;;SY)"
	}
	return winio.ListenPipe(addr, &winio.PipeConfig{SecurityDescriptor: sddl, InputBufferSize: 64 << 10, OutputBufferSize: 1 << 20})
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, addr)
}
