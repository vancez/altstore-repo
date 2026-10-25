package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// An IPA is a zip archive, and extracting a couple of members out of a
// hundred-megabyte file should not mean downloading all of it. archive/zip
// reads through an io.ReaderAt, so this turns HTTP range requests into one.
//
// Reads are cached in blocks: archive/zip walks the central directory with a
// buffered reader in 4 KiB steps, and a member's local header usually shares a
// block with its data. Without the cache every one of those steps would be a
// separate round trip.
const (
	remoteZipBlockSize  = 128 << 10
	remoteZipCacheLimit = 64 // blocks; a few megabytes for a large icon set
)

type remoteZip struct {
	ctx    context.Context
	client *http.Client
	url    string // final URL after redirects: signed CDN URLs must be reused as-is
	size   int64
	auth   string // Authorization header; empty for public files

	mu     sync.Mutex
	blocks map[int64][]byte
	order  []int64
}

// newRemoteZip probes with a one-byte range request. That learns the file's
// size and final URL, and proves range support before the zip reader is handed
// anything: a host that ignores Range would otherwise look fine until the
// first real read tried to skip ahead.
func newRemoteZip(ctx context.Context, client *http.Client, rawURL, auth string) (*remoteZip, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, trf(ctx, msgIPARequestFail, err)
	}
	setDownloadHeaders(req)
	req.Header.Set("Range", "bytes=0-0")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, trf(ctx, msgIPARequestFail, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil, trf(ctx, msgIPARangeNeeded)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, trf(ctx, msgIPARequestFail, resp.Status)
	}
	contentRange := strings.TrimSpace(resp.Header.Get("Content-Range"))
	// The probe asked for byte zero; anything else means a proxy rewrote the
	// request, and the offsets the zip reader computes would be wrong.
	if !strings.HasPrefix(contentRange, "bytes 0-0/") {
		return nil, trf(ctx, msgIPARequestFail, "invalid Content-Range from the file host")
	}
	size, ok := contentRangeTotal(contentRange)
	if !ok || size <= 0 {
		return nil, trf(ctx, msgIPARequestFail, "invalid Content-Range from the file host")
	}

	final := resp.Request.URL
	// A redirect usually means a CDN with a signed URL, which must not be sent
	// the API token. Keep the header only when the redirect stayed on the host
	// that was given the token in the first place.
	if auth != "" {
		orig, err := url.Parse(rawURL)
		if err != nil || !strings.EqualFold(orig.Hostname(), final.Hostname()) {
			auth = ""
		}
	}

	return &remoteZip{
		ctx:    ctx,
		client: client,
		url:    final.String(),
		size:   size,
		auth:   auth,
		blocks: map[int64][]byte{},
	}, nil
}

func setDownloadHeaders(req *http.Request) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity") // ranges are byte ranges of the stored object
	req.Header.Set("User-Agent", userAgent)
}

// contentRangeTotal pulls the total out of "bytes 0-0/12345".
func contentRangeTotal(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, false
	}
	slash := strings.LastIndexByte(v, '/')
	if slash < 0 {
		return 0, false
	}
	totalPart := strings.TrimSpace(v[slash+1:])
	if totalPart == "*" {
		return 0, false
	}
	total, err := strconv.ParseInt(totalPart, 10, 64)
	return total, err == nil
}

// ReadAt implements io.ReaderAt.
func (z *remoteZip) ReadAt(p []byte, off int64) (int, error) {
	want := len(p)
	if off < 0 {
		return 0, errors.New("remote zip: negative offset")
	}
	if off >= z.size {
		return 0, io.EOF
	}
	if int64(len(p)) > z.size-off {
		p = p[:z.size-off]
	}

	written := 0
	for written < len(p) {
		pos := off + int64(written)
		start := pos - pos%remoteZipBlockSize
		block, err := z.block(start)
		if err != nil {
			return written, err
		}
		if pos < start || pos >= start+int64(len(block)) {
			return written, io.ErrUnexpectedEOF
		}
		written += copy(p[written:], block[pos-start:])
	}
	if len(p) < want {
		return written, io.EOF
	}
	return written, nil
}

func (z *remoteZip) block(start int64) ([]byte, error) {
	z.mu.Lock()
	if b, ok := z.blocks[start]; ok {
		z.mu.Unlock()
		return b, nil
	}
	z.mu.Unlock()

	b, err := z.fetchBlock(start)
	if err != nil {
		return nil, err
	}

	z.mu.Lock()
	// Two readers may have raced for the same block; either copy is fine, but
	// only cache it once so eviction cannot drop a block still in the map.
	if old, ok := z.blocks[start]; ok {
		z.mu.Unlock()
		return old, nil
	}
	z.blocks[start] = b
	z.order = append(z.order, start)
	for len(z.order) > remoteZipCacheLimit {
		old := z.order[0]
		z.order = z.order[1:]
		delete(z.blocks, old)
	}
	z.mu.Unlock()
	return b, nil
}

func (z *remoteZip) fetchBlock(start int64) ([]byte, error) {
	end := start + remoteZipBlockSize - 1
	if end >= z.size {
		end = z.size - 1
	}
	return z.fetchRange(start, end)
}

// readRange fetches one contiguous span without going through the block cache.
// Large zip members (an Assets.car can be tens of megabytes) would otherwise
// cost one request per 128 KiB block; this turns them into a single request.
func (z *remoteZip) readRange(off, length int64) ([]byte, error) {
	if length <= 0 {
		return []byte{}, nil
	}
	end := off + length - 1
	if end >= z.size {
		end = z.size - 1
	}
	return z.fetchRange(off, end)
}

func (z *remoteZip) fetchRange(start, end int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(z.ctx, http.MethodGet, z.url, nil)
	if err != nil {
		return nil, trf(z.ctx, msgIPARequestFail, err)
	}
	setDownloadHeaders(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if z.auth != "" {
		req.Header.Set("Authorization", z.auth)
	}

	resp, err := z.client.Do(req)
	if err != nil {
		return nil, trf(z.ctx, msgIPARequestFail, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil, trf(z.ctx, msgIPARangeNeeded)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, trf(z.ctx, msgIPARequestFail, resp.Status)
	}
	wantPrefix := fmt.Sprintf("bytes %d-%d/", start, end)
	if cr := strings.TrimSpace(resp.Header.Get("Content-Range")); !strings.HasPrefix(cr, wantPrefix) {
		return nil, trf(z.ctx, msgIPARequestFail, "the file host returned a different byte range than requested")
	}

	want := end - start + 1
	data, err := io.ReadAll(io.LimitReader(resp.Body, want+1))
	if err != nil {
		return nil, trf(z.ctx, msgIPARequestFail, err)
	}
	if int64(len(data)) != want {
		return nil, trf(z.ctx, msgIPARequestFail,
			fmt.Sprintf("short range response: got %d of %d bytes", len(data), want))
	}
	return data, nil
}
