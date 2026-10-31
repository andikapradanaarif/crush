package common

import (
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/session"
)

// SessionModelOverride returns the session's persisted model
// override, or nil when the session follows the shared selection. A
// malformed stored value reads as absent — the run path logs the
// same failure on rehydration.
func SessionModelOverride(sess *session.Session) *config.SelectedModel {
	if sess == nil || len(sess.ModelOverride) == 0 {
		return nil
	}
	override, err := proto.SelectedModelFromJSON(sess.ModelOverride)
	if err != nil {
		return nil
	}
	return override
}
