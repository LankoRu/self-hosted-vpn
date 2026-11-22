package bot

import (
	"strings"
	"testing"
)

// TestHelpMentionsAdminOnly — справка должна честно предупреждать, что отчёты
// и списки пользователей доступны только админу (они раскрывают клиентов).
func TestHelpMentionsAdminOnly(t *testing.T) {
	b := &Bot{}
	help := b.helpText()
	if !strings.Contains(help, "только админу") {
		t.Errorf("в справке нет предупреждения о правах:\n%s", help)
	}
	for _, cmd := range []string{"/today", "/listusers", "/user", "/digest"} {
		if !strings.Contains(help, cmd) {
			t.Errorf("справка не упоминает %s", cmd)
		}
	}
}
