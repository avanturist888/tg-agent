package attach

import "testing"

func TestDenied(t *testing.T) {
	for _, p := range []string{`C:\x\.env`, `C:\x\.env.production`, `C:\u\.ssh\config`, `D:\keys\server.pem`, `C:\a\user.session`} {
		if deniedReason(p) == "" {
			t.Errorf("пропущен секрет %s", p)
		}
	}
	for _, p := range []string{`C:\docs\report.pdf`, `C:\docs\environment.txt`} {
		if r := deniedReason(p); r != "" {
			t.Errorf("ложный отказ %s: %s", p, r)
		}
	}
}
