package mcp

import (
	"encoding/base64"
	"errors"
	"strconv"
)

// defaultPageSize bounds how many entries tools/list and resources/list return per page.
const defaultPageSize = 50

var errInvalidCursor = errors.New("invalid cursor")

// EncodeCursor and DecodeCursor implement a simple opaque offset-based pagination cursor.
// Clients MUST treat cursor values as opaque; base64-encoding a decimal offset lets it
// round-trip safely as a JSON string while giving callers no reason to try to parse it
// themselves.
func EncodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// DecodeCursor reverses EncodeCursor. An empty cursor decodes to offset 0 (the first
// page).
func DecodeCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, errInvalidCursor
	}
	offset, err := strconv.Atoi(string(b))
	if err != nil || offset < 0 {
		return 0, errInvalidCursor
	}
	return offset, nil
}

// paginate slices items starting at cursor's offset, returning at most pageSize items and
// the cursor for the next page (empty string if this was the last page).
func paginate[T any](items []T, cursor string, pageSize int) (page []T, nextCursor string, err error) {
	offset, err := DecodeCursor(cursor)
	if err != nil || offset > len(items) {
		return nil, "", errInvalidCursor
	}
	end := offset + pageSize
	if end > len(items) {
		end = len(items)
	}
	page = items[offset:end]
	if end < len(items) {
		nextCursor = EncodeCursor(end)
	}
	return page, nextCursor, nil
}
