package briefkasten

import (
	"bytes"
	"strings"
)

// searchMailbox searches via the backend's Searcher when available,
// otherwise falls back to scanning the unread backlog.
func searchMailbox(mb Mailbox, query string) ([]string, error) {
	if s, ok := mb.(Searcher); ok {
		return s.Search(query)
	}
	ids, err := mb.ListUnread()
	if err != nil {
		return nil, err
	}
	needle := []byte(strings.ToLower(query))
	var out []string
	for _, id := range ids {
		raw, err := mb.Fetch(id)
		if err != nil {
			continue
		}
		if bytes.Contains(bytes.ToLower(raw), needle) {
			out = append(out, id)
		}
	}
	return out, nil
}
