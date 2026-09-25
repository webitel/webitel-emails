package mail

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

func TestReadFetchedMessageSizeLimit(t *testing.T) {
	const maxMessageSize = 4

	tests := []struct {
		name         string
		raw          string
		wantTooLarge bool
	}{
		{name: "at limit", raw: "mail"},
		{name: "over limit", raw: "email", wantTooLarge: true},
	}

	session := &imapSession{maxMessageSize: maxMessageSize}
	requestedSection := &imap.BodySectionName{Peek: true, Partial: []int{0, maxMessageSize + 1}}
	responseSection := &imap.BodySectionName{Partial: []int{0}}
	internalDate := time.Date(2025, time.September, 23, 10, 15, 0, 0, time.UTC)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := &imap.Message{
				Uid:          7,
				InternalDate: internalDate,
				Body: map[*imap.BodySectionName]imap.Literal{
					responseSection: bytes.NewBufferString(tt.raw),
				},
			}

			got, err := session.readFetchedMessage(message, requestedSection, message.Uid)
			if tt.wantTooLarge {
				if !errors.Is(err, ErrMessageTooLarge) {
					t.Fatalf("error = %v, want ErrMessageTooLarge", err)
				}
				if got != nil {
					t.Errorf("message = %+v, want nil", got)
				}

				return
			}

			if err != nil {
				t.Fatalf("readFetchedMessage: %v", err)
			}
			if got == nil || got.UID != message.Uid || got.InternalDate != internalDate || string(got.Raw) != tt.raw {
				t.Errorf("message = %+v", got)
			}
		})
	}
}

func TestFetchRawAdaptiveBatch(t *testing.T) {
	t.Run("ordered batch", func(t *testing.T) {
		session, server := newSelectedTestIMAPSession(t)
		result := startFetchRaw(session, []uint32{2, 3, 4})

		server.serveFetch(t, "2:4", []testFetchMessage{{2, "two"}, {3, "three"}, {4, "four"}})
		assertFetchResult(t, <-result, []uint32{2, 3, 4})
		if session.sequentialFetch {
			t.Fatal("sequentialFetch = true after ordered batch")
		}
	})

	t.Run("expunged uid", func(t *testing.T) {
		session, server := newSelectedTestIMAPSession(t)
		result := startFetchRaw(session, []uint32{2, 3, 4})

		server.serveFetch(t, "2:4", []testFetchMessage{{2, "two"}, {4, "four"}})
		server.serveFetch(t, "3", nil)
		server.serveFetch(t, "4", []testFetchMessage{{4, "four"}})

		assertFetchResult(t, <-result, []uint32{2, 4})
		if session.sequentialFetch {
			t.Fatal("sequentialFetch = true after expunged UID")
		}
	})

	t.Run("unordered response enables sticky fallback", func(t *testing.T) {
		session, server := newSelectedTestIMAPSession(t)
		result := startFetchRaw(session, []uint32{2, 3, 4})

		server.serveFetch(t, "2:4", []testFetchMessage{{3, "three"}, {2, "two"}, {4, "four"}})
		server.serveFetch(t, "2", []testFetchMessage{{2, "two"}})
		server.serveFetch(t, "3", []testFetchMessage{{3, "three"}})
		server.serveFetch(t, "4", []testFetchMessage{{4, "four"}})

		assertFetchResult(t, <-result, []uint32{2, 3, 4})
		if !session.sequentialFetch {
			t.Fatal("sequentialFetch = false after unordered response")
		}

		next := startFetchRaw(session, []uint32{5, 6})
		server.serveFetch(t, "5", []testFetchMessage{{5, "five"}})
		server.serveFetch(t, "6", []testFetchMessage{{6, "six"}})
		assertFetchResult(t, <-next, []uint32{5, 6})
	})
}

type testFetchMessage struct {
	uid uint32
	raw string
}

type testFetchResult struct {
	uids []uint32
	err  error
}

func startFetchRaw(session *imapSession, uids []uint32) <-chan testFetchResult {
	done := make(chan testFetchResult, 1)
	go func() {
		var fetched []uint32
		err := session.FetchRaw(uids, func(message *IMAPMessage) {
			fetched = append(fetched, message.UID)
		})
		done <- testFetchResult{uids: fetched, err: err}
	}()

	return done
}

func assertFetchResult(t *testing.T, result testFetchResult, want []uint32) {
	t.Helper()

	if result.err != nil {
		t.Fatalf("FetchRaw: %v", result.err)
	}
	if fmt.Sprint(result.uids) != fmt.Sprint(want) {
		t.Fatalf("fetched UIDs = %v, want %v", result.uids, want)
	}
}

type testIMAPServer struct {
	conn   net.Conn
	reader *bufio.Reader
}

func newSelectedTestIMAPSession(t *testing.T) (*imapSession, *testIMAPServer) {
	t.Helper()

	serverConn, clientConn := net.Pipe()
	greeting := make(chan error, 1)
	go func() {
		_, err := io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
		greeting <- err
	}()

	imapClient, err := client.New(clientConn)
	if err != nil {
		t.Fatalf("create IMAP client: %v", err)
	}
	if err := <-greeting; err != nil {
		t.Fatalf("write IMAP greeting: %v", err)
	}

	server := &testIMAPServer{conn: serverConn, reader: bufio.NewReader(serverConn)}
	selected := make(chan error, 1)
	go func() {
		_, err := imapClient.Select("INBOX", true)
		selected <- err
	}()

	tag, command := server.readCommand(t)
	if command != "EXAMINE INBOX" {
		t.Fatalf("IMAP command = %q, want EXAMINE INBOX", command)
	}
	server.write(t, "* 0 EXISTS\r\n")
	server.write(t, "* OK [UIDVALIDITY 1] UIDs valid\r\n")
	server.write(t, "* OK [UIDNEXT 2] Predicted next UID\r\n")
	server.write(t, tag+" OK [READ-ONLY] EXAMINE completed\r\n")
	if err := <-selected; err != nil {
		t.Fatalf("select mailbox: %v", err)
	}

	t.Cleanup(func() {
		_ = imapClient.Terminate()
		_ = serverConn.Close()
	})

	return &imapSession{client: imapClient, maxMessageSize: 1024}, server
}

func (s *testIMAPServer) serveFetch(t *testing.T, wantSet string, messages []testFetchMessage) {
	t.Helper()

	tag, command := s.readCommand(t)
	if !strings.HasPrefix(command, "UID FETCH "+wantSet+" ") {
		t.Fatalf("IMAP command = %q, want UID FETCH %s", command, wantSet)
	}
	for sequence, message := range messages {
		s.write(t, fmt.Sprintf("* %d FETCH (UID %d INTERNALDATE \"23-Sep-2025 10:15:00 +0000\" BODY[]<0> {%d}\r\n%s)\r\n",
			sequence+1, message.uid, len(message.raw), message.raw))
	}
	s.write(t, tag+" OK UID FETCH completed\r\n")
}

func (s *testIMAPServer) readCommand(t *testing.T) (string, string) {
	t.Helper()

	line, err := s.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read IMAP command: %v", err)
	}
	parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
	if len(parts) != 2 {
		t.Fatalf("invalid IMAP command %q", line)
	}

	return parts[0], parts[1]
}

func (s *testIMAPServer) write(t *testing.T, response string) {
	t.Helper()

	if _, err := io.WriteString(s.conn, response); err != nil {
		t.Fatalf("write IMAP response: %v", err)
	}
}
