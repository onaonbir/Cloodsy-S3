package handler

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// awsChunkedReader decodes AWS chunked transfer encoding.
//
// AWS SDKs may send request bodies in a custom chunked format when
// X-Amz-Content-Sha256 is "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" or
// "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER" or Content-Encoding
// contains "aws-chunked".
//
// Each chunk has the format:
//
//	<hex-size>;chunk-signature=<sig>\r\n
//	<data>\r\n
//
// The final chunk is:
//
//	0;chunk-signature=<sig>\r\n
//	\r\n
type awsChunkedReader struct {
	reader    *bufio.Reader
	remaining int64
	done      bool
}

func newAWSChunkedReader(r io.Reader) *awsChunkedReader {
	return &awsChunkedReader{
		reader: bufio.NewReaderSize(r, 64*1024),
	}
}

func (cr *awsChunkedReader) Read(p []byte) (int, error) {
	if cr.done {
		return 0, io.EOF
	}

	// If we have remaining data in the current chunk, read it
	if cr.remaining > 0 {
		toRead := int64(len(p))
		if toRead > cr.remaining {
			toRead = cr.remaining
		}
		n, err := cr.reader.Read(p[:toRead])
		cr.remaining -= int64(n)

		// When chunk data is fully read, consume trailing \r\n
		if cr.remaining == 0 {
			cr.consumeCRLF()
		}

		if err != nil && err != io.EOF {
			return n, err
		}
		return n, nil
	}

	// Read next chunk header: "<hex-size>;chunk-signature=<sig>\r\n"
	line, err := cr.reader.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) == 0 {
			cr.done = true
			return 0, io.EOF
		}
		if errors.Is(err, io.EOF) {
			// Partial line at EOF — try to parse it
		} else {
			return 0, err
		}
	}

	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		cr.done = true
		return 0, io.EOF
	}

	// Parse chunk size (everything before the first ';')
	sizeStr := string(line)
	if idx := strings.IndexByte(sizeStr, ';'); idx >= 0 {
		sizeStr = sizeStr[:idx]
	}

	chunkSize, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
	if err != nil || chunkSize < 0 {
		cr.done = true
		return 0, io.EOF
	}

	if chunkSize > maxChunkSize {
		cr.done = true
		return 0, fmt.Errorf("chunk size %d exceeds maximum %d", chunkSize, maxChunkSize)
	}

	if chunkSize == 0 {
		// Final chunk — consume any trailing data (trailer headers + CRLF)
		cr.done = true
		// Drain remaining trailer lines
		for {
			trailer, err := cr.reader.ReadBytes('\n')
			if err != nil || len(bytes.TrimRight(trailer, "\r\n")) == 0 {
				break
			}
		}
		return 0, io.EOF
	}

	cr.remaining = chunkSize

	// Now read from the chunk data
	toRead := int64(len(p))
	if toRead > cr.remaining {
		toRead = cr.remaining
	}
	n, err := cr.reader.Read(p[:toRead])
	cr.remaining -= int64(n)

	if cr.remaining == 0 {
		cr.consumeCRLF()
	}

	if err != nil && err != io.EOF {
		return n, err
	}
	return n, nil
}

func (cr *awsChunkedReader) consumeCRLF() {
	b, _ := cr.reader.Peek(2)
	if len(b) >= 2 && b[0] == '\r' && b[1] == '\n' {
		cr.reader.Discard(2)
	} else if len(b) >= 1 && b[0] == '\n' {
		cr.reader.Discard(1)
	}
}
