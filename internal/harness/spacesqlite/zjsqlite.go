// Package zjsqlite is a minimal, read-only SQLite reader for exactly one
// purpose: resolving a session name to its directory id in Zellij's
// session_index.sqlite.
//
// The machine has no sqlite3 CLI and the Go standard library has no SQLite
// driver, so this package hand-reads the documented on-disk format: the
// file header, the sqlite_master table b-tree, the sessions table b-tree,
// record serial types, overflow-page chains, and the write-ahead-log
// overlay. Zellij keeps a long-lived connection with recent commits
// sitting in the WAL, so the overlay is mandatory for parity with the
// Python helper (whose stdlib sqlite3 reads through the WAL).
//
// Anything unexpected — corrupt magic, foreign schema, malformed pages, WAL
// checksum failure — degrades to "not found" so callers fall back to the
// session-metadata scan, exactly like a failed Python query would.
package spacesqlite

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"unicode/utf16"
)

const headerMagic = "SQLite format 3\x00"

// sqlite_master columns shared by every SQLite database.
const (
	masterTypeColumn = 0
	masterNameColumn = 1
	masterRootColumn = 3
	masterSQLColumn  = 4
)

// LookupSession returns the sessions.id value for the row whose
// sessions.name equals name in the database at dbPath. The bool is false
// when the row is absent or the database cannot be read safely; callers
// fall back to scanning session metadata directories.
func LookupSession(dbPath, name string) (string, bool) {
	db, err := openDatabase(dbPath)
	if err != nil {
		return "", false
	}
	defer db.close()
	id, err := db.lookup(name)
	if err != nil {
		return "", false
	}
	return id, true
}

// database is one open read view of a SQLite database plus its WAL.
type database struct {
	file     *os.File
	pageSize int
	reserved int
	encoding uint32 // 1 UTF-8, 2 UTF-16le, 3 UTF-16be
	wal      *walFile
}

func openDatabase(path string) (*database, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 100)
	if _, err := file.ReadAt(header, 0); err != nil {
		file.Close()
		return nil, err
	}
	if string(header[:16]) != headerMagic {
		file.Close()
		return nil, errors.New("zjsqlite: not a sqlite database")
	}
	pageSize := int(binary.BigEndian.Uint16(header[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize&(pageSize-1) != 0 {
		file.Close()
		return nil, errors.New("zjsqlite: invalid page size")
	}
	encoding := binary.BigEndian.Uint32(header[56:60])
	if encoding < 1 || encoding > 3 {
		encoding = 1
	}
	db := &database{
		file:     file,
		pageSize: pageSize,
		reserved: int(header[20]),
		encoding: encoding,
	}
	// An unreadable or absent WAL is not fatal: the main database still
	// holds every checkpointed row.
	walPath := path + "-wal"
	if st, err := os.Stat(walPath); err == nil && st.Mode().IsRegular() && st.Size() >= 32 {
		if wal, err := openWAL(walPath, pageSize); err == nil {
			db.wal = wal
		}
	}
	return db, nil
}

func (db *database) close() {
	if db.wal != nil {
		db.wal.close()
	}
	db.file.Close()
}

func (db *database) usable() int { return db.pageSize - db.reserved }

// readPage returns page pageNumber (1-based), honoring the WAL overlay.
func (db *database) readPage(pageNumber int) ([]byte, error) {
	if pageNumber < 1 {
		return nil, errors.New("zjsqlite: invalid page number")
	}
	if db.wal != nil {
		if frame, ok := db.wal.frameFor(pageNumber); ok {
			return frame, nil
		}
	}
	buf := make([]byte, db.pageSize)
	offset := int64(pageNumber-1) * int64(db.pageSize)
	if _, err := db.file.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("zjsqlite: read page %d: %w", pageNumber, err)
	}
	return buf, nil
}

func (db *database) lookup(name string) (string, error) {
	root, sql, err := db.sessionsTable()
	if err != nil {
		return "", err
	}
	idColumn, nameColumn, err := columnIndexes(sql)
	if err != nil {
		return "", err
	}
	var id string
	found := false
	if _, err := db.scanTable(root, func(record []value) (bool, error) {
		if nameColumn >= len(record) || idColumn >= len(record) {
			return false, errors.New("zjsqlite: short sessions row")
		}
		if record[nameColumn].text() != name {
			return false, nil
		}
		id = record[idColumn].text()
		found = true
		return true, nil
	}); err != nil {
		return "", err
	}
	if !found || id == "" {
		return "", errors.New("zjsqlite: session not found")
	}
	return id, nil
}

// sessionsTable finds the root page and CREATE TABLE sql of the sessions
// table via sqlite_master on page 1.
func (db *database) sessionsTable() (int, string, error) {
	var root int
	var sql string
	found := false
	if _, err := db.scanTable(1, func(record []value) (bool, error) {
		if len(record) <= masterSQLColumn {
			return false, nil
		}
		if record[masterTypeColumn].text() != "table" ||
			record[masterNameColumn].text() != "sessions" {
			return false, nil
		}
		root = int(record[masterRootColumn].integer())
		sql = record[masterSQLColumn].text()
		found = true
		return true, nil
	}); err != nil {
		return 0, "", err
	}
	if !found || root < 1 {
		return 0, "", errors.New("zjsqlite: no sessions table")
	}
	return root, sql, nil
}

// scanTable walks a table b-tree in row order, calling visit for each
// record until visit returns true or an error.
func (db *database) scanTable(root int, visit func([]value) (bool, error)) (bool, error) {
	seen := map[int]bool{}
	var walk func(page, depth int) (bool, error)
	walk = func(page, depth int) (bool, error) {
		if depth > 32 || seen[page] {
			return false, errors.New("zjsqlite: table b-tree loop")
		}
		seen[page] = true
		buf, err := db.readPage(page)
		if err != nil {
			return false, err
		}
		headerOffset := 0
		if page == 1 {
			headerOffset = 100
		}
		if headerOffset+8 > len(buf) {
			return false, errors.New("zjsqlite: short page")
		}
		kind := buf[headerOffset]
		cellCount := int(binary.BigEndian.Uint16(buf[headerOffset+3 : headerOffset+5]))
		switch kind {
		case 0x05: // interior table page
			pointerArea := headerOffset + 12
			for i := 0; i < cellCount; i++ {
				if pointerArea+2*i+2 > len(buf) {
					return false, errors.New("zjsqlite: bad cell pointer")
				}
				cell := int(binary.BigEndian.Uint16(buf[pointerArea+2*i : pointerArea+2*i+2]))
				if cell+4 > len(buf) || cell < 0 {
					return false, errors.New("zjsqlite: bad interior cell")
				}
				child := int(binary.BigEndian.Uint32(buf[cell : cell+4]))
				if stop, err := walk(child, depth+1); err != nil || stop {
					return stop, err
				}
			}
			right := int(binary.BigEndian.Uint32(buf[headerOffset+8 : headerOffset+12]))
			return walk(right, depth+1)
		case 0x0d: // leaf table page
			pointerArea := headerOffset + 8
			for i := 0; i < cellCount; i++ {
				if pointerArea+2*i+2 > len(buf) {
					return false, errors.New("zjsqlite: bad cell pointer")
				}
				cell := int(binary.BigEndian.Uint16(buf[pointerArea+2*i : pointerArea+2*i+2]))
				if cell < 0 || cell >= len(buf) {
					return false, errors.New("zjsqlite: bad leaf cell")
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
			return false, fmt.Errorf("zjsqlite: unexpected page type 0x%02x", kind)
		}
	}
	return walk(root, 0)
}

// leafPayload reassembles one table-leaf cell payload, following the
// documented overflow rules.
func (db *database) leafPayload(page []byte, cell int) ([]byte, error) {
	rest := page[cell:]
	length, n := uvarint(rest)
	if n == 0 {
		return nil, errors.New("zjsqlite: bad payload length")
	}
	rest = rest[n:]
	_, n = uvarint(rest) // rowid
	if n == 0 {
		return nil, errors.New("zjsqlite: bad rowid")
	}
	rest = rest[n:]
	payload := int(length)
	usable := db.usable()
	maxLocal := usable - 35
	if payload <= maxLocal {
		if payload > len(rest) {
			return nil, errors.New("zjsqlite: short inline payload")
		}
		return rest[:payload], nil
	}
	minLocal := ((usable-12)*32)/255 - 23
	k := minLocal + (payload-minLocal)%(usable-4)
	local := minLocal
	if k <= maxLocal {
		local = k
	}
	if local+4 > len(rest) {
		return nil, errors.New("zjsqlite: short overflow cell")
	}
	out := make([]byte, 0, payload)
	out = append(out, rest[:local]...)
	next := int(binary.BigEndian.Uint32(rest[local : local+4]))
	remaining := payload - local
	for next != 0 && remaining > 0 {
		if next < 1 {
			return nil, errors.New("zjsqlite: bad overflow page")
		}
		overflow, err := db.readPage(next)
		if err != nil {
			return nil, err
		}
		chunk := usable - 4
		if chunk > remaining {
			chunk = remaining
		}
		if 4+chunk > len(overflow) {
			return nil, errors.New("zjsqlite: short overflow content")
		}
		out = append(out, overflow[4:4+chunk]...)
		remaining -= chunk
		next = int(binary.BigEndian.Uint32(overflow[0:4]))
	}
	if remaining != 0 {
		return nil, errors.New("zjsqlite: truncated overflow chain")
	}
	return out, nil
}

// --- record decoding --------------------------------------------------------

type value interface {
	// text renders the value the way the Python helper would stringify it
	// (TEXT columns as stored, integers via decimal).
	text() string
	// integer returns numeric columns (root pages).
	integer() int64
}

type nullValue struct{}
type textValue string
type blobValue []byte
type intValue int64
type floatValue float64

func (nullValue) text() string      { return "" }
func (nullValue) integer() int64    { return 0 }
func (v textValue) text() string    { return string(v) }
func (v textValue) integer() int64  { return 0 }
func (v blobValue) text() string    { return string(v) }
func (v blobValue) integer() int64  { return 0 }
func (v intValue) text() string     { return fmt.Sprintf("%d", int64(v)) }
func (v intValue) integer() int64   { return int64(v) }
func (v floatValue) text() string   { return fmt.Sprintf("%v", float64(v)) }
func (v floatValue) integer() int64 { return int64(float64(v)) }

// decodeRecord parses one SQLite record: a varint header of serial types
// followed by the values.
func (db *database) decodeRecord(payload []byte) ([]value, error) {
	if len(payload) == 0 {
		return nil, errors.New("zjsqlite: empty record")
	}
	headerLen, n := uvarint(payload)
	if n == 0 || headerLen < int64(n) || headerLen > int64(len(payload)) {
		return nil, errors.New("zjsqlite: bad record header")
	}
	var serials []int64
	rest := payload[n:headerLen]
	for len(rest) > 0 {
		serial, n := uvarint(rest)
		if n == 0 {
			return nil, errors.New("zjsqlite: bad serial type")
		}
		serials = append(serials, serial)
		rest = rest[n:]
	}
	body := payload[headerLen:]
	out := make([]value, 0, len(serials))
	for _, serial := range serials {
		switch {
		case serial == 0:
			out = append(out, nullValue{})
		case serial >= 1 && serial <= 6:
			widths := [...]int{1, 2, 3, 4, 6, 8}
			width := widths[serial-1]
			if len(body) < width {
				return nil, errors.New("zjsqlite: short integer value")
			}
			var v int64
			for _, b := range body[:width] {
				v = v<<8 | int64(int8(b))
			}
			out = append(out, intValue(v))
			body = body[width:]
		case serial == 7:
			if len(body) < 8 {
				return nil, errors.New("zjsqlite: short float value")
			}
			out = append(out, floatValue(math.Float64frombits(binary.BigEndian.Uint64(body[:8]))))
			body = body[8:]
		case serial == 8:
			out = append(out, intValue(0))
		case serial == 9:
			out = append(out, intValue(1))
		case serial >= 12 && serial%2 == 0:
			length := int((serial - 12) / 2)
			if len(body) < length {
				return nil, errors.New("zjsqlite: short blob value")
			}
			out = append(out, blobValue(body[:length]))
			body = body[length:]
		case serial >= 13:
			length := int((serial - 13) / 2)
			if len(body) < length {
				return nil, errors.New("zjsqlite: short text value")
			}
			out = append(out, textValue(db.decodeText(body[:length])))
			body = body[length:]
		default:
			return nil, fmt.Errorf("zjsqlite: reserved serial type %d", serial)
		}
	}
	return out, nil
}

// decodeText renders TEXT bytes per the database's text encoding.
func (db *database) decodeText(b []byte) string {
	switch db.encoding {
	case 2, 3: // UTF-16le / UTF-16be
		u16 := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if db.encoding == 3 {
				u16 = append(u16, binary.BigEndian.Uint16(b[i:i+2]))
			} else {
				u16 = append(u16, binary.LittleEndian.Uint16(b[i:i+2]))
			}
		}
		return string(utf16.Decode(u16))
	default: // 1: UTF-8
		return string(b)
	}
}

// --- column parsing ---------------------------------------------------------

// columnIndexes extracts the positions of the id and name columns from a
// CREATE TABLE statement. Zellij's schema is a flat column list, so a
// tolerant top-level split is sufficient; anything unusual fails closed.
func columnIndexes(sql string) (int, int, error) {
	open := strings.IndexByte(sql, '(')
	if open < 0 {
		return 0, 0, errors.New("zjsqlite: malformed create table")
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
		return 0, 0, errors.New("zjsqlite: unterminated create table")
	}
	parts := splitTopLevel(sql[open+1 : end])
	idColumn, nameColumn := -1, -1
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		upper := strings.ToUpper(part)
		if strings.HasPrefix(upper, "PRIMARY KEY") ||
			strings.HasPrefix(upper, "UNIQUE") ||
			strings.HasPrefix(upper, "CHECK") ||
			strings.HasPrefix(upper, "FOREIGN KEY") ||
			strings.HasPrefix(upper, "CONSTRAINT") {
			continue
		}
		field := part
		if i := strings.IndexAny(field, " \t(\"[`'"); i >= 0 {
			field = field[:i]
		}
		field = strings.Trim(field, "\"'`[]")
		switch strings.ToLower(field) {
		case "id":
			if idColumn < 0 {
				idColumn = i
			}
		case "name":
			if nameColumn < 0 {
				nameColumn = i
			}
		}
	}
	if idColumn < 0 || nameColumn < 0 {
		return 0, 0, errors.New("zjsqlite: id/name columns not found")
	}
	return idColumn, nameColumn, nil
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

// --- WAL --------------------------------------------------------------------

type walFile struct {
	file *os.File
	// frames maps page number to the latest committed frame content.
	frames map[int][]byte
}

func openWAL(path string, pageSize int) (*walFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 32)
	if _, err := file.ReadAt(header, 0); err != nil {
		file.Close()
		return nil, err
	}
	magic := binary.BigEndian.Uint32(header[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		file.Close()
		return nil, errors.New("zjsqlite: bad WAL magic")
	}
	bigEndian := magic == 0x377f0683
	walPage := int(binary.BigEndian.Uint32(header[8:12]))
	if walPage != pageSize || walPage == 0 {
		file.Close()
		return nil, errors.New("zjsqlite: WAL page size mismatch")
	}
	salt1 := binary.BigEndian.Uint32(header[16:20])
	salt2 := binary.BigEndian.Uint32(header[20:24])

	// The header checksum covers its first 24 bytes with a zero seed; the
	// per-frame checksums then chain cumulatively across frames.
	s0, s1 := walChecksum(0, 0, header, 0, 24, bigEndian)
	if s0 != binary.BigEndian.Uint32(header[24:28]) ||
		s1 != binary.BigEndian.Uint32(header[28:32]) {
		file.Close()
		return nil, errors.New("zjsqlite: WAL header checksum mismatch")
	}

	st, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	frameSize := int64(24 + walPage)
	committed := make(map[int][]byte)
	pending := make(map[int][]byte)
	var offset int64 = 32
	for offset+frameSize <= st.Size() {
		frame := make([]byte, frameSize)
		if _, err := file.ReadAt(frame, offset); err != nil {
			break
		}
		if binary.BigEndian.Uint32(frame[8:12]) != salt1 ||
			binary.BigEndian.Uint32(frame[12:16]) != salt2 {
			break // stale frames from an earlier checkpoint
		}
		// Frame checksums cover the first 8 header bytes then the page
		// content, chained from the running value.
		c0, c1 := walChecksum(s0, s1, frame, 0, 8, bigEndian)
		c0, c1 = walChecksum(c0, c1, frame, 24, int(frameSize), bigEndian)
		if c0 != binary.BigEndian.Uint32(frame[16:20]) ||
			c1 != binary.BigEndian.Uint32(frame[20:24]) {
			break // torn or corrupt frame: stop trusting the tail
		}
		s0, s1 = c0, c1
		pageNumber := int(binary.BigEndian.Uint32(frame[0:4]))
		content := make([]byte, walPage)
		copy(content, frame[24:])
		if pageNumber >= 1 {
			pending[pageNumber] = content
		}
		if binary.BigEndian.Uint32(frame[4:8]) != 0 { // commit frame
			for key, value := range pending {
				committed[key] = value
			}
			pending = make(map[int][]byte)
		}
		offset += frameSize
	}
	return &walFile{file: file, frames: committed}, nil
}

func (w *walFile) frameFor(pageNumber int) ([]byte, bool) {
	frame, ok := w.frames[pageNumber]
	return frame, ok
}

func (w *walFile) close() { w.file.Close() }

// walChecksum implements the documented WAL checksum: a cumulative sum of
// 32-bit words over two accumulators, in the byte order the WAL magic
// selects.
func walChecksum(s0, s1 uint32, data []byte, from, to int, bigEndian bool) (uint32, uint32) {
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

// uvarint decodes a SQLite big-endian variable-length integer (at most nine
// bytes, the ninth contributing all eight bits).
func uvarint(b []byte) (int64, int) {
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
