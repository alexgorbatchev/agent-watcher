package scanner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
)

const (
	tailChunkSize     = 256 * 1024      // 256KB chunks for backward scanning
	maxLineBufferSize = 8 * 1024 * 1024 // Bound incomplete-line accumulation
)

// ExtractLastActiveTimestamp reads backward to the last valid native activity
// record. parse must be the agent-parser activity reader for the transcript's
// harness. Invalid and nonactivity records are skipped; absent valid activity,
// the file modification time is returned. Cancellation and file errors propagate.
func ExtractLastActiveTimestamp(ctx context.Context, filePath string, parse func([]byte) (int64, bool, error)) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	file, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("opening transcript file: %w", err)
	}
	defer func() { _ = file.Close() }() // Read-only file; no buffered writes to flush.
	stat, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat transcript file: %w", err)
	}
	var found int64
	err = scanLinesBackward(file, stat.Size(), 0, func(line []byte) bool {
		if ctx.Err() != nil {
			return false
		}
		stamp, ok, err := parse(line)
		if err == nil && ok && stamp > 0 {
			found = stamp
			return false
		}
		return true
	})
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err != nil {
		return 0, err
	}
	if found != 0 {
		return found, nil
	}
	return stat.ModTime().UnixMilli(), nil
}

// scanLinesBackward calls visit with each non-empty trimmed line from the end of
// the file until visit returns false, or limit bytes have been read when limit is
// positive.
func scanLinesBackward(file io.ReaderAt, fileSize, limit int64, visit func(line []byte) bool) error {
	cursor := fileSize
	var remainder []byte

	for cursor > 0 {
		if limit > 0 && fileSize-cursor >= limit {
			return nil
		}
		readSize := int64(tailChunkSize)
		if cursor < readSize {
			readSize = cursor
		}
		readStart := cursor - readSize

		chunk := make([]byte, readSize)
		n, err := file.ReadAt(chunk, readStart)
		if err != nil && err != io.EOF {
			return fmt.Errorf("reading transcript chunk at offset %d: %w", readStart, err)
		}
		chunk = chunk[:n]

		var data []byte
		if len(remainder) > 0 {
			data = make([]byte, len(chunk)+len(remainder))
			copy(data, chunk)
			copy(data[len(chunk):], remainder)
		} else {
			data = chunk
		}

		cursor = readStart

		lines := bytes.Split(data, []byte("\n"))
		var startIdx int
		if cursor > 0 {
			// When seeking into the middle of a file, the first slice before the first \n
			// is truncated at the buffer boundary. Retain it as remainder for the next backward chunk.
			remainder = lines[0]
			if len(remainder) > maxLineBufferSize {
				// Prevent unbounded memory growth if file has no newlines
				remainder = nil
			}
			startIdx = 1
		} else {
			// Reached beginning of file; all slices including lines[0] are complete lines.
			remainder = nil
			startIdx = 0
		}

		for i := len(lines) - 1; i >= startIdx; i-- {
			line := bytes.TrimSpace(lines[i])
			if len(line) == 0 {
				continue
			}
			if !visit(line) {
				return nil
			}
		}
	}

	return nil
}

// ExtractFirstActiveTimestamp reads forward to the first valid native activity
// record using the harness-specific agent-parser reader. Invalid and nonactivity
// records are skipped; absent valid activity, the file modification time is
// returned. Cancellation and file errors propagate.
func ExtractFirstActiveTimestamp(ctx context.Context, filePath string, parse func([]byte) (int64, bool, error)) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	file, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("opening transcript file: %w", err)
	}
	defer func() { _ = file.Close() }() // Read-only file; no buffered writes to flush.
	stat, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat transcript file: %w", err)
	}
	reader := bufio.NewReader(file)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		line, readErr := reader.ReadBytes('\n')
		stamp, ok, err := parse(bytes.TrimSpace(line))
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err == nil && ok && stamp > 0 {
			return stamp, nil
		}
		if readErr != nil {
			if readErr != io.EOF {
				return 0, fmt.Errorf("reading transcript line: %w", readErr)
			}
			return stat.ModTime().UnixMilli(), nil
		}
	}
}
