package convstore

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

const (
	// DefaultHeadBytes is the default head read budget for fast Stage-1 JSONL metadata parsing.
	DefaultHeadBytes int64 = 32 << 10
	// DefaultTailBytes is the default tail read budget for fast Stage-1 JSONL metadata parsing.
	DefaultTailBytes int64 = 32 << 10
	// DefaultMaxJSONLLineBytes skips single JSONL lines larger than 256 KB (e.g. huge base64/tool dumps).
	DefaultMaxJSONLLineBytes = 256 << 10
)

// ReadHeadTailLines reads bounded head and tail chunks of a JSONL file without
// loading multi-megabyte transcripts into memory. If the file fits within
// headBytes+tailBytes, all lines are returned in head and tail is nil.
func ReadHeadTailLines(path string, headBytes, tailBytes int64, maxHeadLines, maxTailLines int) (head []string, tail []string, err error) {
	if headBytes <= 0 {
		headBytes = DefaultHeadBytes
	}
	if tailBytes <= 0 {
		tailBytes = DefaultTailBytes
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil, nil
	}

	if size <= headBytes+tailBytes {
		buf := make([]byte, size)
		n, rerr := io.ReadFull(f, buf)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return nil, nil, rerr
		}
		lines := splitNonEmptyLines(buf[:n])
		if maxHeadLines > 0 && len(lines) > maxHeadLines {
			head = lines[:maxHeadLines]
			if maxTailLines > 0 {
				start := len(lines) - maxTailLines
				if start < maxHeadLines {
					start = maxHeadLines
				}
				tail = lines[start:]
			}
			return head, tail, nil
		}
		return lines, nil, nil
	}

	// Read head chunk.
	headBuf := make([]byte, headBytes)
	hn, herr := io.ReadFull(f, headBuf)
	if herr != nil && herr != io.EOF && herr != io.ErrUnexpectedEOF {
		return nil, nil, herr
	}
	headChunk := headBuf[:hn]
	// Drop any partial trailing line if we stopped mid-file.
	if idx := bytes.LastIndexByte(headChunk, '\n'); idx >= 0 {
		headChunk = headChunk[:idx]
	}
	head = splitNonEmptyLines(headChunk)
	if maxHeadLines > 0 && len(head) > maxHeadLines {
		head = head[:maxHeadLines]
	}

	// Read tail chunk.
	tailStart := size - tailBytes
	if tailStart < 0 {
		tailStart = 0
	}
	if _, serr := f.Seek(tailStart, io.SeekStart); serr != nil {
		return head, nil, nil
	}
	tailBuf := make([]byte, size-tailStart)
	tn, terr := io.ReadFull(f, tailBuf)
	if terr != nil && terr != io.EOF && terr != io.ErrUnexpectedEOF {
		return head, nil, nil
	}
	tailChunk := tailBuf[:tn]
	// Drop partial leading line when seeking into the middle of the file.
	if tailStart > 0 {
		if idx := bytes.IndexByte(tailChunk, '\n'); idx >= 0 {
			tailChunk = tailChunk[idx+1:]
		} else {
			tailChunk = nil
		}
	}
	tail = splitNonEmptyLines(tailChunk)
	if maxTailLines > 0 && len(tail) > maxTailLines {
		tail = tail[len(tail)-maxTailLines:]
	}
	return head, tail, nil
}

func splitNonEmptyLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	raw := bytes.Split(data, []byte{'\n'})
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			out = append(out, string(line))
		}
	}
	return out
}

// ReadIncrementalJSONL streams lines from a JSONL file for Stage-2 message indexing.
//
// If prevOffset > 0 and prevPrefixHash != "" and the file size is >= prevOffset,
// it computes SHA-256 of bytes [0, prevOffset). If the hash matches, it seeks
// directly to prevOffset and streams only newly appended lines (appended = true).
// Otherwise it re-streams from offset 0 (appended = false).
//
// Lines exceeding maxLineBytes (default 256 KB) are skipped safely without aborting
// the stream, while still advancing newOffset and updating newPrefixHash.
func ReadIncrementalJSONL(
	path string,
	prevOffset int64,
	prevPrefixHash string,
	maxLineBytes int,
	onLine func(line []byte) error,
) (newOffset int64, newPrefixHash string, appended bool, err error) {
	if maxLineBytes <= 0 {
		maxLineBytes = DefaultMaxJSONLLineBytes
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, "", false, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, "", false, err
	}
	size := fi.Size()
	if size == 0 {
		sum := sha256.Sum256(nil)
		return 0, hex.EncodeToString(sum[:]), false, nil
	}

	hasher := sha256.New()
	var startOffset int64

	if prevOffset > 0 && prevPrefixHash != "" && size >= prevOffset {
		n, copyErr := io.CopyN(hasher, f, prevOffset)
		if copyErr == nil && n == prevOffset {
			prefixSum := hex.EncodeToString(hasher.Sum(nil))
			if prefixSum == prevPrefixHash {
				appended = true
				startOffset = prevOffset
			}
		}
	}

	if !appended {
		hasher.Reset()
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return 0, "", false, serr
		}
		startOffset = 0
	}

	reader := bufio.NewReaderSize(f, 64<<10)
	curOffset := startOffset
	var lineBuf []byte

	for {
		lineBuf = lineBuf[:0]
		oversized := false
		var rawLen int64
		eof := false

		for {
			frag, rerr := reader.ReadSlice('\n')
			if len(frag) > 0 {
				_, _ = hasher.Write(frag)
				rawLen += int64(len(frag))
				if !oversized {
					if len(lineBuf)+len(frag) > maxLineBytes {
						oversized = true
						lineBuf = lineBuf[:0]
					} else {
						lineBuf = append(lineBuf, frag...)
					}
				}
			}
			if rerr == bufio.ErrBufferFull {
				continue
			}
			if rerr == io.EOF {
				eof = true
				break
			}
			if rerr != nil {
				return curOffset, hex.EncodeToString(hasher.Sum(nil)), appended, rerr
			}
			break
		}

		if rawLen == 0 && eof {
			break
		}
		curOffset += rawLen

		if !oversized {
			trimmed := bytes.TrimSpace(lineBuf)
			if len(trimmed) > 0 && onLine != nil {
				if cbErr := onLine(trimmed); cbErr != nil {
					return curOffset, hex.EncodeToString(hasher.Sum(nil)), appended, cbErr
				}
			}
		}
		if eof {
			break
		}
	}

	return curOffset, hex.EncodeToString(hasher.Sum(nil)), appended, nil
}
