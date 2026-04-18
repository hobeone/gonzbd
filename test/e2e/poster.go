//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/config"
)

// postArticle connects to the NNTP server described by cfg and posts a single
// article. It handles TLS, greeting, authentication, and the POST command
// sequence. The article body should be yEnc-encoded (use mocknntp.EncodeYEnc).
//
// Returns nil on success, or an error describing the failure. If the server
// does not support POST (440), the error message contains "posting not allowed".
func postArticle(cfg config.ServerConfig, messageID, newsgroup, subject string, body []byte) error {
	port := cfg.Port
	if port == 0 {
		if cfg.SSL {
			port = 563
		} else {
			port = 119
		}
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	timeout := 30 * time.Second

	var nc net.Conn
	var err error
	if cfg.SSL {
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: timeout},
			Config: &tls.Config{
				ServerName:         cfg.Host,
				InsecureSkipVerify: cfg.SSLVerify == 0, //nolint:gosec // test code; user controls ssl_verify setting
			},
		}
		nc, err = d.Dial("tcp", addr)
	} else {
		nc, err = net.DialTimeout("tcp", addr, timeout)
	}
	if err != nil {
		return fmt.Errorf("poster: dial %s: %w", addr, err)
	}
	defer nc.Close() //nolint:errcheck // best-effort cleanup

	if err := nc.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("poster: set deadline: %w", err)
	}

	br := bufio.NewReader(nc)
	bw := bufio.NewWriter(nc)

	// Read greeting.
	code, _, err := readResponse(br)
	if err != nil {
		return fmt.Errorf("poster: read greeting: %w", err)
	}
	if code != 200 && code != 201 {
		return fmt.Errorf("poster: unexpected greeting code %d", code)
	}

	// Authenticate if credentials are provided.
	if cfg.Username != "" {
		if err := authenticate(bw, br, cfg.Username, cfg.Password); err != nil {
			return fmt.Errorf("poster: auth: %w", err)
		}
	}

	// Send POST command.
	if _, err := fmt.Fprintf(bw, "POST\r\n"); err != nil {
		return fmt.Errorf("poster: write POST: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("poster: flush POST: %w", err)
	}

	code, text, err := readResponse(br)
	if err != nil {
		return fmt.Errorf("poster: read POST response: %w", err)
	}
	if code == 440 {
		return fmt.Errorf("poster: posting not allowed: %s", text)
	}
	if code != 340 {
		return fmt.Errorf("poster: unexpected POST response %d %s", code, text)
	}

	// Send article: headers + blank line + body + terminator.
	article := buildArticleMessage(messageID, newsgroup, subject, body)
	if _, err := bw.Write(article); err != nil {
		return fmt.Errorf("poster: write article: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("poster: flush article: %w", err)
	}

	code, text, err = readResponse(br)
	if err != nil {
		return fmt.Errorf("poster: read article response: %w", err)
	}
	if code != 240 {
		return fmt.Errorf("poster: article rejected %d %s", code, text)
	}

	// QUIT.
	_, _ = fmt.Fprintf(bw, "QUIT\r\n") //nolint:errcheck // best-effort
	_ = bw.Flush()                     //nolint:errcheck // best-effort

	return nil
}

// buildArticleMessage constructs a complete NNTP article with headers and
// dot-stuffed body, terminated by CRLF.CRLF.
func buildArticleMessage(messageID, newsgroup, subject string, body []byte) []byte {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "From: e2e-test@sabnzbd-go.test\r\n")
	fmt.Fprintf(&buf, "Newsgroups: %s\r\n", newsgroup)
	fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
	fmt.Fprintf(&buf, "Message-ID: <%s>\r\n", messageID)
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	buf.WriteString("\r\n")

	// Dot-stuff the body: lines starting with "." get an extra ".".
	lines := bytes.Split(body, []byte("\n"))
	for i, line := range lines {
		if len(line) > 0 && line[0] == '.' {
			buf.WriteByte('.')
		}
		buf.Write(line)
		if i < len(lines)-1 {
			buf.WriteByte('\n')
		}
	}

	// Ensure body ends with CRLF before terminator.
	if !bytes.HasSuffix(buf.Bytes(), []byte("\r\n")) {
		buf.WriteString("\r\n")
	}
	buf.WriteString(".\r\n")

	return buf.Bytes()
}

// readResponse reads a single NNTP response line and parses the status code.
func readResponse(br *bufio.Reader) (code int, text string, err error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 3 {
		return 0, "", fmt.Errorf("response too short: %q", line)
	}
	code, err = strconv.Atoi(line[:3])
	if err != nil {
		return 0, "", fmt.Errorf("invalid response code: %q", line)
	}
	if len(line) > 4 {
		text = line[4:]
	}
	return code, text, nil
}

// authenticate performs the AUTHINFO USER/PASS sequence.
func authenticate(bw *bufio.Writer, br *bufio.Reader, user, pass string) error {
	if _, err := fmt.Fprintf(bw, "AUTHINFO USER %s\r\n", user); err != nil {
		return fmt.Errorf("write USER: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("flush USER: %w", err)
	}
	code, text, err := readResponse(br)
	if err != nil {
		return fmt.Errorf("read USER response: %w", err)
	}
	switch code {
	case 281:
		return nil
	case 381:
		// needs password
	default:
		return fmt.Errorf("USER rejected: %d %s", code, text)
	}

	if _, err := fmt.Fprintf(bw, "AUTHINFO PASS %s\r\n", pass); err != nil {
		return fmt.Errorf("write PASS: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("flush PASS: %w", err)
	}
	code, text, err = readResponse(br)
	if err != nil {
		return fmt.Errorf("read PASS response: %w", err)
	}
	if code != 281 {
		return fmt.Errorf("PASS rejected: %d %s", code, text)
	}
	return nil
}
