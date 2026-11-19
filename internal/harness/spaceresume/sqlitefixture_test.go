package spaceresume

import (
	"encoding/binary"
	"os"
	"testing"
)

// Minimal SQLite fixture writer for tests: builds a small valid database
// file (header + sqlite_master on page 1 + one table b-tree on page 2)
// readable by both sqliteread.go and spacesqlite.

func putUvarint(v int64) []byte {
	if v < 0 {
		out := make([]byte, 9)
		uv := uint64(v)
		for i := 8; i >= 0; i-- {
			b := byte(uv & 0x7f)
			if i != 8 {
				b |= 0x80
			}
			out[i] = b
			uv >>= 7
		}
		return out
	}
	if v <= 127 {
		return []byte{byte(v)}
	}
	var tmp [10]byte
	n := 0
	uv := uint64(v)
	for uv > 0 {
		tmp[n] = byte(uv & 0x7f)
		uv >>= 7
		n++
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		b := tmp[n-1-i]
		if i != n-1 {
			b |= 0x80
		}
		out[i] = b
	}
	return out
}

func encodeTestInt(v int64) (int64, []byte) {
	if v == 0 {
		return 8, nil
	}
	if v == 1 {
		return 9, nil
	}
	for _, w := range []int{1, 2, 3, 4, 6, 8} {
		min := -(int64(1) << (8*w - 1))
		max := (int64(1) << (8*w - 1)) - 1
		if w == 8 {
			min, max = -1<<63, 1<<63-1
		}
		if v >= min && v <= max {
			serial := int64(w)
			switch w {
			case 1:
				serial = 1
			case 2:
				serial = 2
			case 3:
				serial = 3
			case 4:
				serial = 4
			case 6:
				serial = 5
			case 8:
				serial = 6
			}
			b := make([]byte, w)
			for i := 0; i < w; i++ {
				b[w-1-i] = byte(v >> (8 * i))
			}
			return serial, b
		}
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return 6, b
}

func encodeTestRecord(vals []any) []byte {
	var serials []int64
	var body []byte
	for _, v := range vals {
		switch t := v.(type) {
		case nil:
			serials = append(serials, 0)
		case int:
			s, b := encodeTestInt(int64(t))
			serials = append(serials, s)
			body = append(body, b...)
		case int64:
			s, b := encodeTestInt(t)
			serials = append(serials, s)
			body = append(body, b...)
		case string:
			serials = append(serials, 13+int64(2*len(t)))
			body = append(body, []byte(t)...)
		default:
			serials = append(serials, 0)
		}
	}
	var header []byte
	for _, s := range serials {
		header = append(header, putUvarint(s)...)
	}
	headerLen := putUvarint(int64(len(header) + len(putUvarint(int64(len(header))))))
	// Recompute until stable (header length includes its own varint).
	for {
		total := len(header) + len(headerLen)
		candidate := putUvarint(int64(total))
		if len(candidate) == len(headerLen) {
			break
		}
		headerLen = candidate
	}
	return append(append(headerLen, header...), body...)
}

func buildLeafPage(pageSize int, isPageOne bool, cells [][]byte) []byte {
	page := make([]byte, pageSize)
	base := 0
	if isPageOne {
		base = 100
	}
	end := pageSize
	ptrs := make([]int, len(cells))
	for i, cell := range cells {
		end -= len(cell)
		copy(page[end:], cell)
		ptrs[i] = end
	}
	page[base] = 0x0D
	binary.BigEndian.PutUint16(page[base+3:], uint16(len(cells)))
	if len(cells) == 0 {
		binary.BigEndian.PutUint16(page[base+5:], uint16(pageSize))
	} else {
		binary.BigEndian.PutUint16(page[base+5:], uint16(end))
	}
	for i, p := range ptrs {
		binary.BigEndian.PutUint16(page[base+8+2*i:], uint16(p))
	}
	return page
}

func testCell(rowid int64, vals ...any) []byte {
	rec := encodeTestRecord(vals)
	cell := putUvarint(int64(len(rec)))
	cell = append(cell, putUvarint(rowid)...)
	return append(cell, rec...)
}

// writeTestDB creates path with `sql` as its single table holding rows.
func writeTestDB(t *testing.T, path, table, sql string, rows [][]any) {
	t.Helper()
	const pageSize = 4096
	header := make([]byte, 100)
	copy(header, "SQLite format 3\x00")
	binary.BigEndian.PutUint16(header[16:], pageSize)
	header[18], header[19] = 1, 1
	header[20] = 0
	header[21], header[22], header[23] = 64, 32, 32
	binary.BigEndian.PutUint32(header[28:], 2) // size in pages
	binary.BigEndian.PutUint32(header[44:], 4) // schema format
	binary.BigEndian.PutUint32(header[56:], 1) // utf-8
	master := [][]byte{testCell(1, "table", table, table, int64(2), sql)}
	masterPage := buildLeafPage(pageSize, true, master)
	copy(masterPage, header)
	var tableCells [][]byte
	for i, r := range rows {
		tableCells = append(tableCells, testCell(int64(i+1), r...))
	}
	tablePage := buildLeafPage(pageSize, false, tableCells)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(masterPage); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(tablePage); err != nil {
		t.Fatal(err)
	}
}
