package notifier

import (
	"bufio"
	"net"
	"net/smtp"
	"strings"
	"testing"
)

// TestFinishSend_StopsAtTheRefusedRecipient drives finishSend against a
// scripted server that refuses the second recipient: the send fails naming
// that recipient, no DATA is sent, and the connection is still QUIT.
func TestFinishSend_StopsAtTheRefusedRecipient(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })

	commands := make(chan []string, 1)
	go func() {
		defer serverConn.Close()
		r := bufio.NewReader(serverConn)
		var seen []string
		reply := func(s string) { _, _ = serverConn.Write([]byte(s + "\r\n")) }
		reply("220 mock")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			cmd := strings.TrimSpace(line)
			seen = append(seen, cmd)
			switch {
			case strings.HasPrefix(cmd, "RCPT TO:<b@example.com>"):
				reply("550 no such user")
			case cmd == "QUIT":
				reply("221 bye")
				commands <- seen
				return
			default:
				reply("250 ok")
			}
		}
		commands <- seen
	}()

	c, err := smtp.NewClient(clientConn, "mock")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	e := NewEmailNotifier(EmailConfig{From: "from@example.com", To: []string{"a@example.com", "b@example.com"}})
	err = e.finishSend(c, nil, []byte("body"))
	if err == nil || !strings.Contains(err.Error(), "RCPT TO b@example.com") {
		t.Errorf("finishSend error = %v, want the refused recipient named", err)
	}
	seen := <-commands
	for _, cmd := range seen {
		if cmd == "DATA" {
			t.Errorf("DATA was sent after a refused recipient; commands: %q", seen)
		}
	}
	if len(seen) == 0 || seen[len(seen)-1] != "QUIT" {
		t.Errorf("the connection was not QUIT; commands: %q", seen)
	}
}
