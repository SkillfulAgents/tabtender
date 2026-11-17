//go:build e2e && darwin

package e2e

import (
	"testing"

	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
)

// macOS attributes what a process does to its "responsible" application. A
// browser must answer for itself; otherwise the system blames Tabtender when
// Chrome maintains its own app bundle ("Tabtender was prevented from
// modifying apps on your Mac").
func TestBrowserIsItsOwnResponsibleProcess(t *testing.T) {
	d := newSpec(t).start(t)
	s := d.createSession("")
	browser := mustBrowserProcess(t, d.ephemeralProfileDir(s.ID))

	responsible, err := platform.ResponsiblePID(browser.PID)
	if err != nil {
		t.Skipf("cannot query process responsibility: %v", err)
	}
	if responsible != browser.PID {
		t.Fatalf("browser %d is the responsibility of process %d, want itself", browser.PID, responsible)
	}
	// The browser is still the daemon's to manage.
	d.connect(s).call("Browser.getVersion", nil)
	d.deleteSession(s.ID)
	assertGone(t, "the browser", browser.PID)
}
