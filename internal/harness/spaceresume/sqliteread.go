package spaceresume

// Minimal read-only SQLite lookup for the agent session stores.
//
// The Go standard library has no SQLite driver, so this file hand-reads the
// documented on-disk format the same way internal/harness/spacesqlite does:
// file header, sqlite_master b-tree, table b-trees, record serial types,
// overflow chains, and the WAL overlay. It covers exactly the query shapes
// the discover_* functions need: single-table equality filters with an
// ORDER BY ... DESC LIMIT, plus the conversation_summaries Top-N scan.
// Anything unexpected degrades to "not found" so callers fall back exactly
// like a failed Python sqlite3 query would.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"unicode/utf16"
)

const sqliteMagic = "SQLite format 3\x00"

type sqliteValue = any // nil | int64 | float64 | string

type sqliteDB struct {
	file     *os.File
	pageSize int
	reserved int
	encoding uint32
	wal      map[int][]byte
}

func sqliteOpen(path string) (*sqliteDB, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 100)
	if _, err := file.ReadAt(header, 0); err != nil {
		file.Close()
		return nil, err
	}
	if string(header[:16]) != sqliteMagic {
		file.Close()
		return nil, errors.New("zjresume: not a sqlite database")
	}
	pageSize := int(binary.BigEndian.Uint16(header[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize&(pageSize-1) != 0 {
		file.Close()
		return nil, errors.New("zjresume: invalid page size")
	}
	encoding := binary.BigEndian.Uint32(header[56:60])
	if encoding < 1 || encoding > 3 {
		encoding = 1
	}
	db := &sqliteDB{
		file:     file,
		pageSize: pageSize,
		reserved: int(header[20]),
		encoding: encoding,
	}
	if st, err := os.Stat(path + "-wal"); err == nil && st.Mode().IsRegular() && st.Size() >= 32 {
		if frames, err := sqliteReadWAL(path+"-wal", pageSize); err == nil {
			db.wal = frames
		}
	}
	return db, nil
}

func (db *sqliteDB) close() { db.file.Close() }

func (db *sqliteDB) usable() int { return db.pageSize - db.reserved }

func (db *sqliteDB) readPage(n int) ([]byte, error) {
	if n < 1 {
		return nil, errors.New("zjresume: invalid page number")
	}
	if db.wal != nil {
		if f, ok := db.wal[n]; ok {
			return f, nil
		}
	}
	buf := make([]byte, db.pageSize)
	if _, err := db.file.ReadAt(buf, int64(n-1)*int64(db.pageSize)); err != nil {
		return nil, fmt.Errorf("zjresume: read page %d: %w", n, err)
	}
	return buf, nil
}

func sqliteVarint(b []byte) (int64, int) {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return int64(v), i + 1
		}
	}
	if len(b) < 9 {
		return 0, 0
	}
	v = v<<8 | uint64(b[8])
	return int64(v), 9
}

func (db *sqliteDB) leafPayload(page []byte, cell int) ([]byte, error) {
	rest := page[cell:]
	length, n := sqliteVarint(rest)
	if n == 0 {
		return nil, errors.New("zjresume: bad payload length")
	}
	rest = rest[n:]
	if _, n := sqliteVarint(rest); n == 0 { // rowid
		return nil, errors.New("zjresume: bad rowid")
	} else {
		rest = rest[n:]
	}
	payload := int(length)
	usable := db.usable()
	maxLocal := usable - 35
	if payload <= maxLocal {
		if payload > len(rest) {
			return nil, errors.New("zjresume: short inline payload")
		}
		return rest[:payload], nil
	}
	minLocal := ((usable - 12) * 32 / 255) - 23
	k := minLocal + (payload-minLocal)%(usable-4)
	local := minLocal
	if k <= maxLocal {
		local = k
	}
	if local+4 > len(rest) {
		return nil, errors.New("zjresume: short overflow cell")
	}
	out := make([]byte, 0, payload)
	out = append(out, rest[:local]...)
	next := int(binary.BigEndian.Uint32(rest[local : local+4]))
	remaining := payload - local
	for next != 0 && remaining > 0 {
		overflow, err := db.readPage(next)
		if err != nil {
			return nil, err
		}
		chunk := usable - 4
		if chunk > remaining {
			chunk = remaining
		}
		if 4+chunk > len(overflow) {
			return nil, errors.New("zjresume: short overflow content")
		}
		out = append(out, overflow[4:4+chunk]...)
		remaining -= chunk
		next = int(binary.BigEndian.Uint32(overflow[0:4]))
	}
	if remaining != 0 {
		return nil, errors.New("zjresume: truncated overflow chain")
	}
	return out, nil
}

func (db *sqliteDB) decodeText(b []byte) string {
	switch db.encoding {
	case 2, 3:
		u16 := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if db.encoding == 3 {
				u16 = append(u16, binary.BigEndian.Uint16(b[i:i+2]))
			} else {
				u16 = append(u16, binary.LittleEndian.Uint16(b[i:i+2]))
			}
		}
		return string(utf16.Decode(u16))
	default:
		return string(b)
	}
}

func (db *sqliteDB) decodeRecord(payload []byte) ([]sqliteValue, error) {
	if len(payload) == 0 {
		return nil, errors.New("zjresume: empty record")
	}
	headerLen, n := sqliteVarint(payload)
	if n == 0 || headerLen < int64(n) || headerLen > int64(len(payload)) {
		return nil, errors.New("zjresume: bad record header")
	}
	var serials []int64
	for _, rest := range [][]byte{payload[n:headerLen]} {
		for len(rest) > 0 {
			s, m := sqliteVarint(rest)
			if m == 0 {
				return nil, errors.New("zjresume: bad serial type")
			}
			serials = append(serials, s)
			rest = rest[m:]
		}
	}
	body := payload[headerLen:]
	out := make([]sqliteValue, 0, len(serials))
	for _, serial := range serials {
		switch {
		case serial == 0:
			out = append(out, nil)
		case serial >= 1 && serial <= 6:
			widths := [...]int{1, 2, 3, 4, 6, 8}
			w := widths[serial-1]
			if len(body) < w {
				return nil, errors.New("zjresume: short integer value")
			}
			var v int64
			for _, b := range body[:w] {
				v = v<<8 | int64(int8(b))
			}
			out = append(out, v)
			body = body[w:]
		case serial == 7:
			if len(body) < 8 {
				return nil, errors.New("zjresume: short float value")
			}
			out = append(out, math.Float64frombits(binary.BigEndian.Uint64(body[:8])))
			body = body[8:]
		case serial == 8:
			out = append(out, int64(0))
		case serial == 9:
			out = append(out, int64(1))
		case serial >= 12 && serial%2 == 0:
			length := int((serial - 12) / 2)
			if len(body) < length {
				return nil, errors.New("zjresume: short blob value")
			}
			out = append(out, string(body[:length]))
			body = body[length:]
		case serial >= 13:
			length := int((serial - 13) / 2)
			if len(body) < length {
				return nil, errors.New("zjresume: short text value")
			}
			out = append(out, db.decodeText(body[:length]))
			body = body[length:]
		default:
			return nil, fmt.Errorf("zjresume: reserved serial type %d", serial)
		}
	}
	return out, nil
}

func (db *sqliteDB) scanTable(root int, visit func([]sqliteValue) (bool, error)) (bool, error) {
	seen := map[int]bool{}
	var walk func(page, depth int) (bool, error)
	walk = func(page, depth int) (bool, error) {
		if depth > 32 || seen[page] {
			return false, errors.New("zjresume: table b-tree loop")
		}
		seen[page] = true
		buf, err := db.readPage(page)
		if err != nil {
			return false, err
		}
		off := 0
		if page == 1 {
			off = 100
		}
		if off+8 > len(buf) {
			return false, errors.New("zjresume: short page")
		}
		kind := buf[off]
		cellCount := int(binary.BigEndian.Uint16(buf[off+3 : off+5]))
		switch kind {
		case 0x05:
			area := off + 12
			for i := 0; i < cellCount; i++ {
				if area+2*i+2 > len(buf) {
					return false, errors.New("zjresume: bad cell pointer")
				}
				cell := int(binary.BigEndian.Uint16(buf[area+2*i : area+2*i+2]))
				if cell+4 > len(buf) || cell < 0 {
					return false, errors.New("zjresume: bad interior cell")
				}
				child := int(binary.BigEndian.Uint32(buf[cell : cell+4]))
				if stop, err := walk(child, depth+1); err != nil || stop {
					return stop, err
				}
			}
			right := int(binary.BigEndian.Uint32(buf[off+8 : off+12]))
			return walk(right, depth+1)
		case 0x0D:
			area := off + 8
			for i := 0; i < cellCount; i++ {
				if area+2*i+2 > len(buf) {
					return false, errors.New("zjresume: bad cell pointer")
				}
				cell := int(binary.BigEndian.Uint16(buf[area+2*i : area+2*i+2]))
				if cell < 0 || cell >= len(buf) {
					return false, errors.New("zjresume: bad leaf cell")
				}
				payload, err := db.leafPayload(buf, cell)
				if err != nil {
					return false, err
				}
				record, err := db.decodeRecord(payload)
				if err != nil {
					return false, err
				}
				if stop, err := visit(record); err != nil || stop {
					return stop, err
				}
			}
			return false, nil
		default:
			return false, fmt.Errorf("zjresume: unexpected page type 0x%02x", kind)
		}
	}
	return walk(root, 0)
}

// tableInfo finds a table's root page and column positions.
func (db *sqliteDB) tableInfo(name string) (int, map[string]int, error) {
	var root int
	var sql string
	found := false
	if _, err := db.scanTable(1, func(record []sqliteValue) (bool, error) {
		if len(record) < 5 {
			return false, nil
		}
		typ, _ := record[0].(string)
		tbl, _ := record[1].(string)
		if typ != "table" || tbl != name {
			return false, nil
		}
		switch r := record[3].(type) {
		case int64:
			root = int(r)
		case float64:
			root = int(r)
		}
		sql, _ = record[4].(string)
		found = true
		return true, nil
	}); err != nil {
		return 0, nil, err
	}
	if !found || root < 1 {
		return 0, nil, fmt.Errorf("zjresume: no table %s", name)
	}
	cols, err := sqliteColumns(sql)
	if err != nil {
		return 0, nil, err
	}
	return root, cols, nil
}

// sqliteColumns extracts column positions from a CREATE TABLE statement.
func sqliteColumns(sql string) (map[string]int, error) {
	open := strings.IndexByte(sql, '(')
	if open < 0 {
		return nil, errors.New("zjresume: malformed create table")
	}
	depth := 0
	inString := false
	escaped := false
	end := -1
	for i := open; i < len(sql); i++ {
		ch := sql[i]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '\'' {
				inString = false
			}
			continue
		}
		switch ch {
		case '\'':
			inString = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil, errors.New("zjresume: unterminated create table")
	}
	cols := map[string]int{}
	pos := 0
	for _, part := range splitTopLevel(sql[open+1 : end]) {
		part = strings.TrimSpace(part)
		if part == "" {
			pos++
			continue
		}
		upper := strings.ToUpper(part)
		if strings.HasPrefix(upper, "PRIMARY KEY") || strings.HasPrefix(upper, "UNIQUE") ||
			strings.HasPrefix(upper, "CHECK") || strings.HasPrefix(upper, "FOREIGN KEY") ||
			strings.HasPrefix(upper, "CONSTRAINT") {
			pos++
			continue
		}
		field := part
		if len(field) > 0 && (field[0] == '`' || field[0] == '"' || field[0] == '\'' || field[0] == '[') {
			closeChar := field[0]
			if closeChar == '[' {
				closeChar = ']'
			}
			if endQuote := strings.IndexByte(field[1:], closeChar); endQuote >= 0 {
				field = field[1 : 1+endQuote]
			} else {
				field = strings.Trim(field, "\"'`[]")
			}
		} else if i := strings.IndexAny(field, " \t(\"[`'"); i >= 0 {
			field = field[:i]
		}
		field = strings.ToLower(strings.Trim(field, "\"'`[]"))
		if _, dup := cols[field]; !dup {
			cols[field] = pos
		}
		pos++
	}
	return cols, nil
}

func splitTopLevel(body string) []string {
	var parts []string
	depth := 0
	inString := false
	escaped := false
	start := 0
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '\'' {
				inString = false
			}
			continue
		}
		switch ch {
		case '\'':
			inString = true
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, body[start:])
}

func sqliteText(v sqliteValue) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int64:
		return fmt.Sprintf("%d", t)
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}

func sqliteLess(a, b sqliteValue) bool {
	af, aok := sqliteNum(a)
	bf, bok := sqliteNum(b)
	if aok && bok {
		return af < bf
	}
	return sqliteText(a) < sqliteText(b)
}

func sqliteNum(v sqliteValue) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case float64:
		return t, true
	default:
		return 0, false
	}
}

// sqliteRows scans a table, keeping rows that match all equality filters.
// Rows are returned newest-first by orderCol with at most limit entries
// (limit <= 0 means no limit).
func sqliteRows(dbPath, table string, equals map[string]string, orderCol string, limit int) ([]map[string]sqliteValue, error) {
	db, err := sqliteOpen(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.close()
	root, cols, err := db.tableInfo(table)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(cols))
	for name, idx := range cols {
		if idx >= 0 && idx < len(names) {
			names[idx] = name
		}
	}
	filter := map[string]string{}
	for k, v := range equals {
		lk := strings.ToLower(k)
		idx, ok := cols[lk]
		if !ok {
			return nil, fmt.Errorf("zjresume: no column %s", k)
		}
		_ = idx
		filter[lk] = v
	}
	var orderIdx = -1
	if orderCol != "" {
		var ok bool
		orderIdx, ok = cols[strings.ToLower(orderCol)]
		if !ok {
			return nil, fmt.Errorf("zjresume: no column %s", orderCol)
		}
	}
	var rows []map[string]sqliteValue
	if _, err := db.scanTable(root, func(record []sqliteValue) (bool, error) {
		row := map[string]sqliteValue{}
		for i, v := range record {
			if i < len(names) && names[i] != "" {
				row[names[i]] = v
			}
		}
		for col, want := range filter {
			if sqliteText(row[col]) != want {
				return false, nil
			}
		}
		rows = append(rows, row)
		return false, nil
	}); err != nil {
		return nil, err
	}
	if orderIdx >= 0 {
		orderName := ""
		for name, idx := range cols {
			if idx == orderIdx {
				orderName = name
				break
			}
		}
		sort.SliceStable(rows, func(i, j int) bool {
			// Newest first.
			return sqliteLess(rows[j][orderName], rows[i][orderName])
		})
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// sqliteLatestID implements `select id from <table> where <dir>=<val> order
// by <updated> desc limit 1`, with the optional mcode task-kind exclusion.
func sqliteLatestID(dbPath, table, dirCol, dirVal, idCol, updatedCol string, excludeTask bool) (string, bool) {
	rows, err := sqliteRows(dbPath, table, map[string]string{dirCol: dirVal}, updatedCol, 0)
	if err != nil || len(rows) == 0 {
		return "", false
	}
	idKey := strings.ToLower(idCol)
	for _, row := range rows {
		if excludeTask {
			kind, present := row["session_kind"]
			if present && kind != nil && sqliteText(kind) != "" && sqliteText(kind) != "conversation" {
				// Python: coalesce(session_kind, 'conversation') != 'task'
				if sqliteText(kind) == "task" {
					continue
				}
			}
		}
		if id := sqliteText(row[idKey]); id != "" {
			return id, true
		}
	}
	return "", false
}

// --- WAL overlay (adapted from internal/harness/spacesqlite) -------------------

func sqliteReadWAL(path string, pageSize int) (map[int][]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	header := make([]byte, 32)
	if _, err := file.ReadAt(header, 0); err != nil {
		return nil, err
	}
	magic := binary.BigEndian.Uint32(header[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return nil, errors.New("zjresume: bad WAL magic")
	}
	bigEndian := magic == 0x377f0683
	walPage := int(binary.BigEndian.Uint32(header[8:12]))
	if walPage != pageSize || walPage == 0 {
		return nil, errors.New("zjresume: WAL page size mismatch")
	}
	salt1 := binary.BigEndian.Uint32(header[16:20])
	salt2 := binary.BigEndian.Uint32(header[20:24])
	s0, s1 := sqliteWALChecksum(0, 0, header, 0, 24, bigEndian)
	if s0 != binary.BigEndian.Uint32(header[24:28]) ||
		s1 != binary.BigEndian.Uint32(header[28:32]) {
		return nil, errors.New("zjresume: WAL header checksum mismatch")
	}
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	frameSize := int64(24 + walPage)
	committed := map[int][]byte{}
	pending := map[int][]byte{}
	for offset := int64(32); offset+frameSize <= st.Size(); offset += frameSize {
		frame := make([]byte, frameSize)
		if _, err := file.ReadAt(frame, offset); err != nil {
			break
		}
		if binary.BigEndian.Uint32(frame[8:12]) != salt1 ||
			binary.BigEndian.Uint32(frame[12:16]) != salt2 {
			break
		}
		c0, c1 := sqliteWALChecksum(s0, s1, frame, 0, 8, bigEndian)
		c0, c1 = sqliteWALChecksum(c0, c1, frame, 24, int(frameSize), bigEndian)
		if c0 != binary.BigEndian.Uint32(frame[16:20]) ||
			c1 != binary.BigEndian.Uint32(frame[20:24]) {
			break
		}
		s0, s1 = c0, c1
		pageNumber := int(binary.BigEndian.Uint32(frame[0:4]))
		content := make([]byte, walPage)
		copy(content, frame[24:])
		if pageNumber >= 1 {
			pending[pageNumber] = content
		}
		if binary.BigEndian.Uint32(frame[4:8]) != 0 {
			for k, v := range pending {
				committed[k] = v
			}
			pending = map[int][]byte{}
		}
	}
	return committed, nil
}

func sqliteWALChecksum(s0, s1 uint32, data []byte, from, to int, bigEndian bool) (uint32, uint32) {
	for i := from; i+8 <= to; i += 8 {
		var x0, x1 uint32
		if bigEndian {
			x0 = binary.BigEndian.Uint32(data[i : i+4])
			x1 = binary.BigEndian.Uint32(data[i+4 : i+8])
		} else {
			x0 = binary.LittleEndian.Uint32(data[i : i+4])
			x1 = binary.LittleEndian.Uint32(data[i+4 : i+8])
		}
		s0 += x0 + s1
		s1 += x1 + s0
	}
	return s0, s1
}
