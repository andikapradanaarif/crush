package question

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRequest() Request {
	return Request{
		Questions: []Question{{
			Type:        TypeSingleChoice,
			Text:        "Pick one",
			Description: "A choice is needed.",
			Choices: []Choice{
				{ID: "a", Label: "A"},
				{ID: "b", Label: "B"},
			},
		}},
	}
}

func freeTextRequest(sessionID string) Request {
	return Request{
		SessionID: sessionID,
		Questions: []Question{{
			Type:        TypeFreeText,
			Text:        "What should we do?",
			Description: "Share your thoughts",
		}},
	}
}

func TestAskFailsFastWhilePending(t *testing.T) {
	t.Parallel()
	svc := NewService()
	ctx := context.Background()

	first := make(chan error, 1)
	go func() {
		_, err := svc.Ask(ctx, testRequest())
		first <- err
	}()

	// Wait for the first Ask to install its pending question.
	require.Eventually(t, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return svc.pending != nil
	}, time.Second, time.Millisecond)

	// A concurrent Ask must fail fast, not orphan its channels.
	_, err := svc.Ask(ctx, testRequest())
	require.ErrorIs(t, err, ErrQuestionPending)

	// Resolving frees the slot for the next Ask.
	require.True(t, svc.Answer([]Answer{{SelectedIDs: []string{"a"}}}))
	require.NoError(t, <-first)

	second := make(chan error, 1)
	go func() {
		_, err := svc.Ask(ctx, testRequest())
		second <- err
	}()
	require.Eventually(t, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return svc.pending != nil
	}, time.Second, time.Millisecond)
	require.True(t, svc.Answer([]Answer{{SelectedIDs: []string{"b"}}}))
	require.NoError(t, <-second)
}

// TestPending verifies that Pending reports the in-flight request
// (including its session) only while it awaits an answer.
func TestPending(t *testing.T) {
	t.Parallel()
	svc := NewService()

	_, ok := svc.Pending()
	assert.False(t, ok, "nothing pending yet")

	done := make(chan []Answer, 1)
	go func() {
		answers, _ := svc.Ask(t.Context(), freeTextRequest("s-ask"))
		done <- answers
	}()

	var pending Request
	require.Eventually(t, func() bool {
		pending, ok = svc.Pending()
		return ok
	}, 2*time.Second, 5*time.Millisecond, "question must be pending")
	require.Equal(t, "s-ask", pending.SessionID)
	require.NotEmpty(t, pending.ID)
	require.Len(t, pending.Questions, 1)

	require.True(t, svc.Answer([]Answer{{QuestionID: pending.Questions[0].ID, FillInText: "ship it"}}))
	answers := <-done
	require.Len(t, answers, 1)
	assert.Equal(t, "ship it", answers[0].FillInText)

	_, ok = svc.Pending()
	assert.False(t, ok, "answered question must no longer be pending")
}

// TestAnswerNotificationCarriesSessionID verifies that the resolution
// notification names the session the question belongs to, so SSE
// routing can scope it to that session's viewers.
func TestAnswerNotificationCarriesSessionID(t *testing.T) {
	t.Parallel()
	svc := NewService()

	notifications := svc.SubscribeNotifications(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.Ask(t.Context(), freeTextRequest("s-answer"))
	}()

	var pending Request
	require.Eventually(t, func() bool {
		var ok bool
		pending, ok = svc.Pending()
		return ok
	}, 2*time.Second, 5*time.Millisecond)

	require.True(t, svc.Answer([]Answer{{QuestionID: pending.Questions[0].ID, FillInText: "yes"}}))
	<-done

	ev := <-notifications
	assert.Equal(t, pending.ID, ev.Payload.BatchID)
	assert.Equal(t, "s-answer", ev.Payload.SessionID)
}

// TestCancelNotificationCarriesSessionID covers the cancel path of
// the resolution notification.
func TestCancelNotificationCarriesSessionID(t *testing.T) {
	t.Parallel()
	svc := NewService()

	notifications := svc.SubscribeNotifications(t.Context())

	done := make(chan error, 1)
	go func() {
		_, err := svc.Ask(t.Context(), freeTextRequest("s-cancel"))
		done <- err
	}()

	var pending Request
	require.Eventually(t, func() bool {
		var ok bool
		pending, ok = svc.Pending()
		return ok
	}, 2*time.Second, 5*time.Millisecond)

	require.True(t, svc.Cancel())
	require.ErrorIs(t, <-done, ErrCancelled)

	ev := <-notifications
	assert.Equal(t, pending.ID, ev.Payload.BatchID)
	assert.Equal(t, "s-cancel", ev.Payload.SessionID)
}
