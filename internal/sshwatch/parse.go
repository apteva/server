package sshwatch

import (
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Line is one raw log record produced by a Source.
type Line struct {
	// Cursor is the source position just after this record. The collector
	// stores it in the same transaction as any event derived from the record.
	Cursor string
	// Key identifies the record for deduplication; it defaults to Cursor.
	Key      string
	Time     time.Time
	Hostname string
	Ident    string
	PID      int
	Message  string
}

// sshIdents are the syslog identifiers that carry sshd authentication and
// session records. OpenSSH >= 9.8 logs per-connection records from
// sshd-session (and >= 10.0 pre-auth from sshd-auth).
var sshIdents = map[string]bool{"sshd": true, "sshd-session": true, "sshd-auth": true}

func isSSHIdent(ident string) bool { return sshIdents[ident] }

const (
	maxMessageLen = 512
	maxFieldLen   = 128
)

var (
	// User names on failed/invalid lines are attacker controlled. Greedy user
	// captures bind "from <ip> port <n>" to the LAST occurrence, which sshd
	// itself appends, so a crafted user name cannot spoof the source address.
	// Accepted lines name a real account, while a certificate trailer can carry
	// a free-form ID, so they bind to the FIRST occurrence instead.
	reAccepted  = regexp.MustCompile(`^Accepted (\S+) for (.*?) from (\S+) port (\d+)(?: ssh2)?(?:: (.*))?$`)
	reFailed    = regexp.MustCompile(`^Failed (\S+) for (invalid user )?(.*) from (\S+) port (\d+)(?: ssh2)?(?:: (.*))?$`)
	reInvalid   = regexp.MustCompile(`^Invalid user (.*) from (\S+?)(?: port (\d+))?$`)
	reClosedBy  = regexp.MustCompile(`^Connection (?:closed|reset) by (authenticating|invalid) user (.*) (\S+) port (\d+)$`)
	reDiscFrom  = regexp.MustCompile(`^Disconnected from (authenticating user|invalid user|user) (.*) (\S+) port (\d+)$`)
	reMaxTries  = regexp.MustCompile(`^(?:error: )?maximum authentication attempts exceeded for (invalid user )?(.*) from (\S+) port (\d+)(?: ssh2)?$`)
	reNotAllow  = regexp.MustCompile(`^User (.*) from (\S+) not allowed because (.*)$`)
	reRootDeny  = regexp.MustCompile(`^ROOT LOGIN REFUSED FROM (\S+)(?: port (\d+))?$`)
	rePAM       = regexp.MustCompile(`^pam_unix\(sshd:session\): session (opened|closed) for user (\S+?)(?:\(uid=\d+\))?(?: by .*)?$`)
	reKeyInfo   = regexp.MustCompile(`^(\S+) (SHA256:[A-Za-z0-9+/=]+|MD5(?::[0-9a-f]{2})+)`)
	reSecretKVs = regexp.MustCompile(`(?i)\b(password|passwd|pass|token|secret|api[_-]?key)=\S+`)
)

// ParseMessage normalizes one sshd message. ok is false for records that are
// not authentication or session events (they still advance the cursor).
func ParseMessage(message string) (ev Event, ok bool) {
	msg := strings.TrimSpace(message)
	msg = strings.TrimSuffix(msg, " [preauth]")
	msg = strings.TrimSpace(msg)
	setAddr := func(ip, port string) {
		ev.SourceIP = normalizeIP(ip)
		if p, err := strconv.Atoi(port); err == nil && p > 0 && p < 65536 {
			ev.SourcePort = p
		}
	}
	switch {
	case reAccepted.MatchString(msg):
		m := reAccepted.FindStringSubmatch(msg)
		ev.Kind, ev.Result, ev.Method, ev.User = KindSSHAuth, ResultAccepted, cleanField(m[1]), cleanField(m[2])
		setAddr(m[3], m[4])
		ev.KeyType, ev.Fingerprint = keyInfo(m[5])
	case reFailed.MatchString(msg):
		m := reFailed.FindStringSubmatch(msg)
		ev.Kind, ev.Result, ev.Method, ev.User = KindSSHAuth, ResultFailed, cleanField(m[1]), cleanField(m[3])
		if m[2] != "" {
			ev.Result = ResultInvalidUser
		}
		setAddr(m[4], m[5])
		ev.KeyType, ev.Fingerprint = keyInfo(m[6])
	case reInvalid.MatchString(msg):
		m := reInvalid.FindStringSubmatch(msg)
		ev.Kind, ev.Result, ev.User = KindSSHAuth, ResultInvalidUser, cleanField(m[1])
		setAddr(m[2], m[3])
	case reClosedBy.MatchString(msg):
		m := reClosedBy.FindStringSubmatch(msg)
		ev.Kind, ev.Result, ev.User = KindSSHAuth, ResultFailed, cleanField(m[2])
		if m[1] == "invalid" {
			ev.Result = ResultInvalidUser
		}
		setAddr(m[3], m[4])
	case reDiscFrom.MatchString(msg):
		m := reDiscFrom.FindStringSubmatch(msg)
		ev.User = cleanField(m[2])
		setAddr(m[3], m[4])
		switch m[1] {
		case "user":
			ev.Kind, ev.Result = KindSSHSession, ResultDisconnected
		case "invalid user":
			ev.Kind, ev.Result = KindSSHAuth, ResultInvalidUser
		default:
			ev.Kind, ev.Result = KindSSHAuth, ResultFailed
		}
	case reMaxTries.MatchString(msg):
		m := reMaxTries.FindStringSubmatch(msg)
		ev.Kind, ev.Result, ev.User = KindSSHAuth, ResultFailed, cleanField(m[2])
		if m[1] != "" {
			ev.Result = ResultInvalidUser
		}
		setAddr(m[3], m[4])
	case reNotAllow.MatchString(msg):
		m := reNotAllow.FindStringSubmatch(msg)
		ev.Kind, ev.Result, ev.User = KindSSHAuth, ResultFailed, cleanField(m[1])
		setAddr(m[2], "")
	case reRootDeny.MatchString(msg):
		m := reRootDeny.FindStringSubmatch(msg)
		ev.Kind, ev.Result, ev.User = KindSSHAuth, ResultFailed, "root"
		setAddr(m[1], m[2])
	case rePAM.MatchString(msg):
		m := rePAM.FindStringSubmatch(msg)
		ev.Kind, ev.User = KindSSHSession, cleanField(m[2])
		ev.Result = ResultOpened
		if m[1] == "closed" {
			ev.Result = ResultClosed
		}
	default:
		return Event{}, false
	}
	ev.Message = SanitizeMessage(message)
	return ev, true
}

func keyInfo(rest string) (keyType, fingerprint string) {
	m := reKeyInfo.FindStringSubmatch(strings.TrimSpace(rest))
	if m == nil {
		return "", ""
	}
	return cleanField(m[1]), m[2]
}

func normalizeIP(raw string) string {
	raw = strings.Trim(raw, "[]")
	if a, err := netip.ParseAddr(raw); err == nil {
		return a.Unmap().String()
	}
	// UseDNS can log host names; keep them (sanitized) so rules treat them as
	// unapproved rather than dropping the source.
	return cleanField(raw)
}

func cleanField(s string) string { return truncate(sanitize(s), maxFieldLen) }

// SanitizeMessage strips control characters, redacts key=value pairs that look
// like credentials and truncates the message.
func SanitizeMessage(s string) string {
	s = reSecretKVs.ReplaceAllString(sanitize(s), "$1=[redacted]")
	return truncate(strings.TrimSpace(s), maxMessageLen)
}

func sanitize(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ParseSyslogLine parses a classic ("Nov 16 10:00:00 host ident[pid]: msg")
// or RFC 3339 ("2026-11-16T10:00:00.123+00:00 host ident[pid]: msg") syslog
// line. now resolves the missing year of classic timestamps.
func ParseSyslogLine(line string, now time.Time) (Line, bool) {
	var ts time.Time
	var rest string
	if len(line) >= 16 && line[3] == ' ' && line[6] == ' ' && line[9] == ':' {
		t, err := time.ParseInLocation("Jan _2 15:04:05", line[:15], now.Location())
		if err != nil {
			return Line{}, false
		}
		ts = time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location())
		if ts.After(now.Add(48 * time.Hour)) {
			ts = ts.AddDate(-1, 0, 0)
		}
		rest = line[16:]
	} else {
		field, after, found := strings.Cut(line, " ")
		if !found {
			return Line{}, false
		}
		t, err := time.Parse(time.RFC3339Nano, field)
		if err != nil {
			return Line{}, false
		}
		ts, rest = t, after
	}
	host, rest, found := strings.Cut(strings.TrimLeft(rest, " "), " ")
	if !found {
		return Line{}, false
	}
	tag, msg, found := strings.Cut(rest, ": ")
	if !found {
		return Line{}, false
	}
	ident, pid := tag, 0
	if open := strings.IndexByte(tag, '['); open > 0 && strings.HasSuffix(tag, "]") {
		ident = tag[:open]
		pid, _ = strconv.Atoi(tag[open+1 : len(tag)-1])
	}
	return Line{Time: ts, Hostname: host, Ident: ident, PID: pid, Message: msg}, true
}

// journalRecord is the subset of `journalctl --output=json` fields we use.
type journalRecord struct {
	Cursor   string          `json:"__CURSOR"`
	Realtime string          `json:"__REALTIME_TIMESTAMP"`
	Hostname string          `json:"_HOSTNAME"`
	Ident    string          `json:"SYSLOG_IDENTIFIER"`
	PID      string          `json:"_PID"`
	SysPID   string          `json:"SYSLOG_PID"`
	Message  json.RawMessage `json:"MESSAGE"`
}

// ParseJournalRecord parses one JSON object from journalctl --output=json.
// MESSAGE may be a string or, for non-UTF-8 payloads, an array of bytes.
func ParseJournalRecord(b []byte) (Line, error) {
	var r journalRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return Line{}, err
	}
	if r.Cursor == "" {
		return Line{}, errors.New("journal record without cursor")
	}
	l := Line{Cursor: r.Cursor, Hostname: r.Hostname, Ident: r.Ident}
	if us, err := strconv.ParseInt(r.Realtime, 10, 64); err == nil {
		l.Time = time.UnixMicro(us).UTC()
	}
	l.PID, _ = strconv.Atoi(r.PID)
	if l.PID == 0 {
		l.PID, _ = strconv.Atoi(r.SysPID)
	}
	if len(r.Message) > 0 {
		var s string
		if json.Unmarshal(r.Message, &s) == nil {
			l.Message = s
		} else {
			var raw []byte
			var ints []int
			if json.Unmarshal(r.Message, &ints) == nil {
				raw = make([]byte, 0, len(ints))
				for _, v := range ints {
					raw = append(raw, byte(v))
				}
				l.Message = string(raw)
			}
		}
	}
	return l, nil
}

// journalCursorTime extracts the realtime ("t=" hex microseconds) field of a
// journal cursor, used to bound collector_gap events.
func journalCursorTime(cursor string) time.Time {
	for _, part := range strings.Split(cursor, ";") {
		if v, ok := strings.CutPrefix(part, "t="); ok {
			if us, err := strconv.ParseInt(v, 16, 64); err == nil {
				return time.UnixMicro(us).UTC()
			}
		}
	}
	return time.Time{}
}
