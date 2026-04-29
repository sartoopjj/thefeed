package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/gotd/td/tg"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// telegramMediaDownloadChunk is the per-RPC chunk size used by UploadGetFile.
// MTProto requires Limit to be a multiple of 4KB and ≤ 1MB; 256KB is a good
// trade-off between API call overhead and memory pressure for tiny files.
const telegramMediaDownloadChunk = 256 * 1024

// telegramMediaPhotoSizeOrder lists Telegram photo size codes from smallest
// to largest. The downloader picks the smallest *usable* size — for a
// DNS-tunnelled feed, bandwidth is precious and a thumbnail is usually
// enough for the user to decide whether to look at the original. The
// "stripped" placeholder type is filtered out separately because it is not
// a real renderable image.
//
//	a / b — tiny (≤ 100px)
//	c     — small chat preview
//	m     — medium
//	s     — small (legacy)
//	x     — high-quality
//	y / w — original / largest
var telegramMediaPhotoSizeOrder = []string{"a", "b", "c", "m", "s", "x", "y", "w"}

// downloadTelegramMedia fetches and caches media for a Telegram message. It
// returns the metadata that should be embedded in the message body, or an
// empty MediaMeta with ok=false to fall through to the legacy [TAG] path.
//
// The function is best-effort: any error (download failure, oversized file,
// missing download API) is logged once and the message is returned without
// downloadable metadata so the rest of the feed isn't blocked. The caller
// is responsible for substituting EncodeMediaText into the message body.
func (tr *TelegramReader) downloadTelegramMedia(ctx context.Context, api *tg.Client, msg *tg.Message) (protocol.MediaMeta, bool) {
	if api == nil || msg == nil || msg.Media == nil {
		return protocol.MediaMeta{}, false
	}
	cache := tr.feed.MediaCache()
	if cache == nil {
		return protocol.MediaMeta{}, false
	}

	switch m := msg.Media.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return protocol.MediaMeta{}, false
		}
		return tr.downloadTelegramPhoto(ctx, api, cache, photo)
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return protocol.MediaMeta{}, false
		}
		return tr.downloadTelegramDocument(ctx, api, cache, doc)
	}
	return protocol.MediaMeta{}, false
}

func (tr *TelegramReader) downloadTelegramPhoto(ctx context.Context, api *tg.Client, cache *MediaCache, photo *tg.Photo) (protocol.MediaMeta, bool) {
	cacheKey := "tg-photo:" + strconv.FormatInt(photo.ID, 10)

	// Hit the cache before doing any I/O — exact dedup, no bytes transferred.
	if meta, ok := cache.Lookup(cacheKey); ok {
		return meta, true
	}

	bestType, bestBytes := pickSmallestPhotoSize(photo.Sizes)
	if bestType == "" {
		return protocol.MediaMeta{}, false
	}
	// Honour the configured max-size early so we don't even open the RPC for
	// objects no enabled relay would accept. Files that fit GitHub but not
	// DNS still get fetched.
	if maxBytes := cache.MaxAcceptableBytes(); maxBytes > 0 && bestBytes > maxBytes {
		return protocol.MediaMeta{
			Tag:    protocol.MediaImage,
			Size:   bestBytes,
			Relays: nil,
		}, true
	}

	loc := &tg.InputPhotoFileLocation{
		ID:            photo.ID,
		AccessHash:    photo.AccessHash,
		FileReference: photo.FileReference,
		ThumbSize:     bestType,
	}
	bytes, err := tr.downloadTelegramFile(ctx, api, loc, bestBytes)
	if err != nil {
		// Transient fetch error (network, FILE_REFERENCE_EXPIRED, etc.).
		// We don't mark the message as non-downloadable in that case —
		// "non-downloadable" means "the file exists but the server chose
		// not to cache it" (i.e. oversized). Falling through to legacy
		// keeps the UI honest, and the next 10-min refresh cycle re-tries.
		tr.logMediaError("photo", photo.ID, err)
		return protocol.MediaMeta{}, false
	}

	meta, err := cache.Store(cacheKey, protocol.MediaImage, bytes, "image/jpeg", "")
	if err != nil {
		// ErrTooLarge is reported as non-downloadable; any other store error
		// is just dropped to legacy.
		if errors.Is(err, ErrTooLarge) {
			return meta, true
		}
		tr.logMediaError("photo", photo.ID, err)
		return protocol.MediaMeta{}, false
	}
	return meta, true
}

func (tr *TelegramReader) downloadTelegramDocument(ctx context.Context, api *tg.Client, cache *MediaCache, doc *tg.Document) (protocol.MediaMeta, bool) {
	cacheKey := "tg-doc:" + strconv.FormatInt(doc.ID, 10)
	if meta, ok := cache.Lookup(cacheKey); ok {
		return meta, true
	}

	tag, filename := classifyDocumentTagAndName(doc)
	if tag == protocol.MediaSticker {
		return protocol.MediaMeta{}, false
	}

	if maxBytes := cache.MaxAcceptableBytes(); maxBytes > 0 && doc.Size > maxBytes {
		return protocol.MediaMeta{
			Tag:    tag,
			Size:   doc.Size,
			Relays: nil,
		}, true
	}

	loc := &tg.InputDocumentFileLocation{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
		ThumbSize:     "",
	}
	bytes, err := tr.downloadTelegramFile(ctx, api, loc, doc.Size)
	if err != nil {
		// See note in downloadTelegramPhoto: transient fetch errors should
		// not be surfaced as "non-downloadable", they should fall through
		// to legacy [TAG]\ncaption rendering and let the next refresh retry.
		tr.logMediaError("doc", doc.ID, err)
		return protocol.MediaMeta{}, false
	}

	meta, err := cache.Store(cacheKey, tag, bytes, doc.MimeType, filename)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return meta, true
		}
		tr.logMediaError("doc", doc.ID, err)
		return protocol.MediaMeta{}, false
	}
	return meta, true
}

// downloadTelegramFile downloads `expectedSize` bytes (or all available bytes
// when expectedSize <= 0) from the given Telegram file location. It enforces
// the configured max-size cap defensively so a file that lies about its size
// still can't blow past the limit on the wire.
func (tr *TelegramReader) downloadTelegramFile(ctx context.Context, api *tg.Client, loc tg.InputFileLocationClass, expectedSize int64) ([]byte, error) {
	cache := tr.feed.MediaCache()
	maxBytes := int64(0)
	if cache != nil {
		maxBytes = cache.MaxAcceptableBytes()
	}

	var (
		out    []byte
		offset int64
	)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		req := &tg.UploadGetFileRequest{
			Location: loc,
			Offset:   offset,
			Limit:    telegramMediaDownloadChunk,
		}
		res, err := api.UploadGetFile(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("upload.getFile offset=%d: %w", offset, err)
		}
		fileRes, ok := res.(*tg.UploadFile)
		if !ok {
			return nil, fmt.Errorf("unexpected upload response type %T", res)
		}
		if len(fileRes.Bytes) == 0 {
			break
		}
		out = append(out, fileRes.Bytes...)
		offset += int64(len(fileRes.Bytes))

		// Hard guard against runaway downloads.
		if maxBytes > 0 && int64(len(out)) > maxBytes {
			return nil, fmt.Errorf("download exceeded configured max-size (%d > %d)", len(out), maxBytes)
		}

		// We consider the transfer complete when the server returned less than
		// the requested chunk (canonical EOF) or we've reached the expected size.
		if len(fileRes.Bytes) < telegramMediaDownloadChunk {
			break
		}
		if expectedSize > 0 && int64(len(out)) >= expectedSize {
			break
		}
	}
	return out, nil
}

// pickSmallestPhotoSize returns the smallest usable size in a Telegram
// Photo as (type-code, byte-size). DNS-tunnelled bandwidth is precious, so
// we prefer a small chat-preview thumbnail over the full-resolution
// original whenever Telegram offers both. Returns empty type when no usable
// size is available (e.g. only stripped placeholder thumbs).
func pickSmallestPhotoSize(sizes []tg.PhotoSizeClass) (string, int64) {
	type candidate struct {
		typ  string
		size int64
	}
	var pool []candidate
	add := func(typ string, size int64) {
		if typ == "" {
			return
		}
		pool = append(pool, candidate{typ: typ, size: size})
	}
	for _, s := range sizes {
		switch v := s.(type) {
		case *tg.PhotoSize:
			add(v.Type, int64(v.Size))
		case *tg.PhotoCachedSize:
			add(v.Type, int64(len(v.Bytes)))
		case *tg.PhotoSizeProgressive:
			// Progressive carries a slice of progressive sizes; the FIRST
			// element is the smallest progressive prefix the server can
			// stream, which suits "smallest usable" perfectly.
			if len(v.Sizes) > 0 {
				add(v.Type, int64(v.Sizes[0]))
			} else {
				add(v.Type, 0)
			}
		case *tg.PhotoStrippedSize:
			// Stripped sizes are tiny placeholder thumbs — skip.
		}
	}
	if len(pool) == 0 {
		return "", 0
	}

	// Prefer the entry with the smallest declared byte size; break ties
	// using the type-code preference order (smallest first). When the
	// declared size is 0 (unknown), the type code alone decides the order.
	rank := make(map[string]int, len(telegramMediaPhotoSizeOrder))
	for i, t := range telegramMediaPhotoSizeOrder {
		rank[t] = i
	}
	bestIdx := -1
	for i, c := range pool {
		if bestIdx < 0 {
			bestIdx = i
			continue
		}
		b := pool[bestIdx]
		// Prefer a strictly smaller known size.
		if c.size > 0 && b.size > 0 {
			if c.size < b.size {
				bestIdx = i
				continue
			}
			if c.size == b.size && rank[c.typ] < rank[b.typ] {
				bestIdx = i
			}
			continue
		}
		// One of them has unknown size — fall back to type-code rank.
		if rank[c.typ] < rank[b.typ] {
			bestIdx = i
		}
	}
	chosen := pool[bestIdx]
	return chosen.typ, chosen.size
}

// classifyDocumentTagAndName returns the protocol media tag and best-effort
// filename for a Telegram Document. The tag mirrors classifyDocument's logic
// but also exposes the filename attribute so the HTTP layer can offer a
// reasonable Content-Disposition.
func classifyDocumentTagAndName(doc *tg.Document) (string, string) {
	tag := protocol.MediaFile
	filename := ""
	for _, attr := range doc.Attributes {
		switch a := attr.(type) {
		case *tg.DocumentAttributeVideo:
			tag = protocol.MediaVideo
		case *tg.DocumentAttributeAudio:
			tag = protocol.MediaAudio
		case *tg.DocumentAttributeSticker:
			tag = protocol.MediaSticker
		case *tg.DocumentAttributeAnimated:
			tag = protocol.MediaGIF
		case *tg.DocumentAttributeFilename:
			filename = a.FileName
		}
	}
	return tag, filename
}

func (tr *TelegramReader) logMediaError(kind string, id int64, err error) {
	// Best-effort log; the receiver's package log is fine for now.
	logfMedia("[telegram] media %s id=%d download failed: %v", kind, id, err)
}
