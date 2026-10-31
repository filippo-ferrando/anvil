package termbg

import (
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestDetectThemeEnv(t *testing.T) {
	for env, dark := range map[string]bool{"light": false, "dark": true} {
		t.Setenv(ThemeEnv, env)
		start := time.Now()
		Detect()
		if lipgloss.HasDarkBackground() != dark || time.Since(start) > 100*time.Millisecond {
			t.Errorf("%s=%s: dark %v after %s", ThemeEnv, env, lipgloss.HasDarkBackground(), time.Since(start))
		}
	}
}
