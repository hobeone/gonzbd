package notifier

import (
	"context"
	"net"
	"testing"
	"time"
)

// deadlineConn records the deadline set on it.
type deadlineConn struct {
	net.Conn
	got time.Time
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.got = t
	return nil
}

// TestSetConnDeadline pins both sources of the SMTP connection deadline: the
// context's own, and smtpOperationTimeout when the context has none.
func TestSetConnDeadline(t *testing.T) {
	t.Parallel()
	t.Run("context deadline", func(t *testing.T) {
		t.Parallel()
		dl := time.Now().Add(7 * time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), dl)
		defer cancel()
		c := &deadlineConn{}
		setConnDeadline(ctx, c)
		if !c.got.Equal(dl) {
			t.Errorf("deadline = %v, want the context's %v", c.got, dl)
		}
	})
	t.Run("fallback", func(t *testing.T) {
		t.Parallel()
		before := time.Now()
		c := &deadlineConn{}
		setConnDeadline(context.Background(), c)
		lo, hi := before.Add(smtpOperationTimeout), time.Now().Add(smtpOperationTimeout)
		if c.got.Before(lo) || c.got.After(hi) {
			t.Errorf("deadline = %v, want now+%v (between %v and %v)", c.got, smtpOperationTimeout, lo, hi)
		}
	})
}
