package event

import (
	"time"
)

var appStartTime time.Time

func AppInitialized() {
	appStartTime = time.Now()
	send("app initialized")
}

func AppExited() {
	duration := time.Since(appStartTime).Truncate(time.Second)
	send(
		"app exited",
		"app duration pretty", duration.String(),
		"app duration in seconds", int64(duration.Seconds()),
	)
	Flush()
}

func SessionCreated() {
	send("session created")
}

func SessionDeleted() {
	send("session deleted")
}

func SessionSwitched() {
	send("session switched")
}

func FilePickerOpened() {
	send("filepicker opened")
}

func PromptSent(props ...any) {
	send(
		"prompt sent",
		props...,
	)
}

func PromptResponded(props ...any) {
	send(
		"prompt responded",
		props...,
	)
}

func TokensUsed(props ...any) {
	send(
		"tokens used",
		props...,
	)
}

func StatsViewed() {
	send("stats viewed")
}

// PlanDeclarationBounced records a scope-gate plan rejection — the
// conformance-health signal: a high bounce rate means can't-conform
// (unsurfaced vocabulary or schema) rather than won't (model quality).
func PlanDeclarationBounced(props ...any) {
	send("plan declaration bounced", props...)
}

// PlanDeclarationEscalated records the bounce-budget exhaustion that
// turns a stuck declaration loop into the real scope question.
func PlanDeclarationEscalated(props ...any) {
	send("plan declaration escalated", props...)
}

func SessionListed(json bool) {
	send("session listed", "json", json)
}

func SessionShown(json bool) {
	send("session shown", "json", json)
}

func SessionLastShown(json bool) {
	send("session last shown", "json", json)
}

func SessionDeletedCommand(json bool) {
	send("session deleted", "json", json)
}

func SessionRenamed(json bool) {
	send("session renamed", "json", json)
}
