package service_test

import (
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/service"
)

func TestGenerateSystemdUnit(t *testing.T) {
	unit := service.GenerateSystemdUnit("/usr/local/bin/shepherd", "/home/user/project")
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/shepherd start -f") {
		t.Errorf("expected ExecStart in unit, got:\n%s", unit)
	}
	if !strings.Contains(unit, "WorkingDirectory=/home/user/project") {
		t.Errorf("expected WorkingDirectory in unit, got:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("expected WantedBy in unit, got:\n%s", unit)
	}
}

func TestGenerateLaunchdPlist(t *testing.T) {
	plist := service.GenerateLaunchdPlist("/usr/local/bin/shepherd", "/Users/user/project", "/Users/user/Library/Logs")
	if !strings.Contains(plist, "<string>ai.shepherd.daemon</string>") {
		t.Errorf("expected label in plist, got:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>/usr/local/bin/shepherd</string>") {
		t.Errorf("expected exec path in plist, got:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>-f</string>") {
		t.Errorf("expected -f flag in plist, got:\n%s", plist)
	}
}
