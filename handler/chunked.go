package handler

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/auth"
)

// Errors surfaced by the aws-chunked decoder. Handlers map them to S3 codes.
var (
	errMalformedChunk    = errors.New("malformed aws-chunked encoding")
	errChunkSignature    = errors.New("chunk signature mismatch")
	errChunkTooLarge     = errors.New("chunk exceeds maximum size")
	errIncompleteChunked = errors.New("incomplete aws-chunked body")
)

const maxChunkHeaderLine = 4096

// chunkSigner verifies the STREAMING-AWS4-HMAC-SHA256-PAYLOAD signature chain.
type chunkSigner struct {
	signingKey []byte
	amzDate    string
	scope      string
	prevSig    string
}

func newChunkSigner(secretKey string, a *auth.SigV4Auth) *chunkSigner {
	if a == nil || a.AmzDate == "" || a.Scope == "" {
		return nil
	}
	return &chunkSigner{
		signingKey: auth.DeriveSigningKey(secretKey, a.Date, a.Region, "s3"),
		amzDate:    a.AmzDate,
		scope:      a.Scope,
		prevSig:    a.Signature,
	}
}

func (cs *chunkSigner) verifyChunk(sig string, chunkHash []byte) error {
	stringToSign := "AWS4-HMAC-SHA256-PAYLOAD\n" + cs.amzDate + "\n" + cs.scope + "\n" + cs.prevSig + "\n" +
		auth.HashSHA256Hex(nil) + "\n" + hex.EncodeToString(chunkHash)
	expected := hex.EncodeToString(auth.HMACSHA256(cs.signingKey, []byte(stringToSign)))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return errChunkSignature
	}
	cs.prevSig = sig
	return nil
}

func (cs *chunkSigner) verifyTrailer(sig string, trailerHash []byte) error {
	stringToSign := "AWS4-HMAC-SHA256-TRAILER\n" + cs.amzDate + "\n" + cs.scope + "\n" + cs.prevSig + "\n" + hex.EncodeToString(trailerHash)
	expected := hex.EncodeToString(auth.HMACSHA256(cs.signingKey, []byte(stringToSign)))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return errChunkSignature
	}
	return nil
}

// awsChunkedReader decodes AWS chunked transfer encoding.
//
// AWS SDKs send request bodies in this format when X-Amz-Content-Sha256 is
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD[-TRAILER] or STREAMING-UNSIGNED-PAYLOAD-TRAILER
// (Content-Encoding contains "aws-chunked").
//
// Each chunk has the format:
//
//	<hex-size>[;chunk-signature=<sig>]\r\n
//	<data>\r\n
//
// The final chunk is "0[;chunk-signature=<sig>]\r\n" optionally followed by
// trailer headers ("name:value\r\n"), an optional "x-amz-trailer-signature",
// and a blank line.
type awsChunkedReader struct {
	reader    *bufio.Reader
	remaining int64
	done      bool
	err       error

	signer    *chunkSigner
	chunkHash hash.Hash
	chunkSig  string
	sawFinal  bool

	// Trailers collected after the final chunk (lower-case names).
	Trailers map[string]string
}

func newAWSChunkedReader(r io.Reader, signer *chunkSigner) *awsChunkedReader {
	return &awsChunkedReader{
		reader:   bufio.NewReaderSize(r, 64*1024),
		signer:   signer,
		Trailers: map[string]string{},
	}
}

func (cr *awsChunkedReader) fail(err error) (int, error) {
	cr.done = true
	cr.err = err
	return 0, err
}

func (cr *awsChunkedReader) Read(p []byte) (int, error) {
	if cr.err != nil {
		return 0, cr.err
	}
	if cr.done {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	for cr.remaining == 0 {
		if err := cr.readChunkHeader(); err != nil {
			return cr.fail(err)
		}
		if cr.done {
			return 0, io.EOF
		}
	}

	toRead := int64(len(p))
	if toRead > cr.remaining {
		toRead = cr.remaining
	}
	n, err := cr.reader.Read(p[:toRead])
	cr.remaining -= int64(n)
	if cr.chunkHash != nil && n > 0 {
		cr.chunkHash.Write(p[:n])
	}
	if cr.remaining == 0 {
		if cerr := cr.finishChunk(); cerr != nil {
			return n, cr.failKeep(n, cerr)
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			// Underlying body ended in the middle of a chunk.
			if cr.remaining > 0 {
				return n, cr.failKeep(n, errIncompleteChunked)
			}
			return n, nil
		}
		return n, cr.failKeep(n, err)
	}
	return n, nil
}

// failKeep records err and returns it, preserving the bytes already copied.
func (cr *awsChunkedReader) failKeep(n int, err error) error {
	cr.done = true
	cr.err = err
	return err
}

// readLine reads one CRLF-terminated line with a hard length bound.
func (cr *awsChunkedReader) readLine() ([]byte, error) {
	line, err := cr.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxChunkHeaderLine {
		return nil, errMalformedChunk
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, errIncompleteChunked
		}
		return nil, err
	}
	return bytes.TrimRight(line, "\r\n"), nil
}

func (cr *awsChunkedReader) readChunkHeader() error {
	line, err := cr.readLine()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return errIncompleteChunked // final "0" chunk never arrived
		}
		return err
	}
	header := string(line)
	sizeStr := header
	cr.chunkSig = ""
	if idx := strings.IndexByte(header, ';'); idx >= 0 {
		sizeStr = header[:idx]
		for _, ext := range strings.Split(header[idx+1:], ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(ext), "="); ok && k == "chunk-signature" {
				cr.chunkSig = v
			}
		}
	}
	chunkSize, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
	if err != nil || chunkSize < 0 {
		return errMalformedChunk
	}
	if chunkSize > maxChunkSize {
		return errChunkTooLarge
	}
	if cr.signer != nil && cr.chunkSig == "" {
		return errChunkSignature
	}

	if chunkSize == 0 {
		return cr.readTrailers()
	}
	cr.remaining = chunkSize
	if cr.signer != nil {
		cr.chunkHash = sha256.New()
	}
	return nil
}

// finishChunk consumes the CRLF after chunk data and verifies its signature.
func (cr *awsChunkedReader) finishChunk() error {
	if err := cr.consumeCRLF(); err != nil {
		return err
	}
	if cr.signer != nil {
		if err := cr.signer.verifyChunk(cr.chunkSig, cr.chunkHash.Sum(nil)); err != nil {
			return err
		}
		cr.chunkHash = nil
	}
	return nil
}

func (cr *awsChunkedReader) consumeCRLF() error {
	b, err := cr.reader.Peek(2)
	if len(b) >= 2 && b[0] == '\r' && b[1] == '\n' {
		cr.reader.Discard(2)
		return nil
	}
	if len(b) >= 1 && b[0] == '\n' {
		cr.reader.Discard(1)
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return errMalformedChunk
}

// readTrailers handles everything after the final zero-length chunk.
func (cr *awsChunkedReader) readTrailers() error {
	// The final chunk's own signature covers an empty payload.
	if cr.signer != nil {
		if err := cr.signer.verifyChunk(cr.chunkSig, sha256.New().Sum(nil)); err != nil {
			return err
		}
	}
	var trailerSig string
	var canonical strings.Builder
	for i := 0; i < 32; i++ {
		line, err := cr.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break // no trailer section at all
			}
			return err
		}
		if len(line) == 0 {
			break
		}
		name, value, ok := strings.Cut(string(line), ":")
		if !ok {
			return errMalformedChunk
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == "x-amz-trailer-signature" {
			trailerSig = value
			continue
		}
		cr.Trailers[name] = value
		canonical.WriteString(name + ":" + value + "\n")
	}
	if cr.signer != nil && (trailerSig != "" || canonical.Len() > 0) {
		sum := sha256.Sum256([]byte(canonical.String()))
		if err := cr.signer.verifyTrailer(trailerSig, sum[:]); err != nil {
			return err
		}
	}
	cr.sawFinal = true
	cr.done = true
	return nil
}

// Complete reports whether the terminating zero chunk was consumed.
func (cr *awsChunkedReader) Complete() bool { return cr.sawFinal }

// String is for debugging.
func (cr *awsChunkedReader) String() string {
	return fmt.Sprintf("awsChunkedReader{remaining=%d done=%v final=%v}", cr.remaining, cr.done, cr.sawFinal)
}
