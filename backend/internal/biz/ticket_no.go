package biz

import (
	"context"
	"strconv"
	"strings"
)

const (
	ticketNoSeqStart = 1001
	ticketNoLockKey  = 87233401
)

func FormatTicketNo(n int) string {
	if n <= 0 {
		return ""
	}
	return "T-" + strconv.Itoa(n)
}

func TicketNoValue(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// ParseTicketNo accepts "T-1842", "t1842", or "1842".
func ParseTicketNo(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	s = strings.TrimPrefix(strings.ToUpper(s), "T-")
	s = strings.TrimPrefix(s, "T")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func (s *TicketSvc) nextTicketNo(ctx context.Context) (int, error) {
	if err := s.ensureTicketNoSeq(ctx); err != nil {
		return 0, err
	}
	var n int
	if err := s.d.DB().QueryRowContext(ctx, `SELECT nextval('ticket_no_seq')`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *TicketSvc) ensureTicketNoSeq(ctx context.Context) error {
	_, err := s.d.DB().ExecContext(ctx, `CREATE SEQUENCE IF NOT EXISTS ticket_no_seq START WITH 1001`)
	return err
}

// EnsureTicketNumbers creates the sequence and fills ticket_no on rows
// that predate the column. Safe to call on every boot: setval only moves
// the sequence forward, never backward, so a second instance cannot
// rewind a nextval the first instance already handed out.
func (s *TicketSvc) EnsureTicketNumbers(ctx context.Context) error {
	conn, err := s.d.DB().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, ticketNoLockKey); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, ticketNoLockKey)
	if _, err := conn.ExecContext(ctx, `CREATE SEQUENCE IF NOT EXISTS ticket_no_seq START WITH 1001`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `
		SELECT setval(
			'ticket_no_seq',
			GREATEST(
				(SELECT last_value FROM ticket_no_seq),
				(SELECT COALESCE(MAX(ticket_no), $1 - 1) FROM tickets)
			)
		)
	`, ticketNoSeqStart); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `
		UPDATE tickets
		SET ticket_no = nextval('ticket_no_seq')
		WHERE ticket_no IS NULL
	`)
	return err
}
