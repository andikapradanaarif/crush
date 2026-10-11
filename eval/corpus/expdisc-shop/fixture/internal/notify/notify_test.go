package notify

import "testing"

func TestSendQueues(t *testing.T) {
	before := Pending()
	Send("u@x", "hi")
	if Pending() != before+1 {
		t.Fatal("send didn't queue")
	}
}
