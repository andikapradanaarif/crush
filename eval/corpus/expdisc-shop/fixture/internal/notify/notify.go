// Package notify queues outbound customer messages; the transport
// is out of scope for the eval fixture.
package notify

var outbox []string

// Send queues a message.
func Send(user, msg string) {
	outbox = append(outbox, user+": "+msg)
}

// Pending reports the queue depth.
func Pending() int { return len(outbox) }
