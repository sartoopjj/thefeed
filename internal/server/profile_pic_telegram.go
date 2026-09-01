package server

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/gotd/td/tg"
)

// extractChatPhotoID returns (photoID, dcID), or (0, 0) when there is none.
func extractChatPhotoID(p tg.ChatPhotoClass) (int64, int) {
	switch ph := p.(type) {
	case *tg.ChatPhoto:
		return ph.PhotoID, ph.DCID
	}
	return 0, 0
}

// extractUserPhotoID is the User-profile equivalent.
func extractUserPhotoID(p tg.UserProfilePhotoClass) (int64, int) {
	switch ph := p.(type) {
	case *tg.UserProfilePhoto:
		return ph.PhotoID, ph.DCID
	}
	return 0, 0
}

// fetchProfilePhoto downloads the small (~5KB / 160px) thumb. Big:false
// is Telegram's "a" thumb size.
func (tr *TelegramReader) fetchProfilePhoto(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, photoID int64) ([]byte, error) {
	if photoID == 0 {
		return nil, fmt.Errorf("no photo")
	}
	loc := &tg.InputPeerPhotoFileLocation{
		Peer:    peer,
		PhotoID: photoID,
	}
	loc.Big = false
	return tr.downloadTelegramFile(ctx, api, loc, 0, 0)
}

// fetchAllProfilePhotos downloads avatars (public by username,
// private by privateChannelID) and merges them into the feed bundle.
// Per-channel failures are logged and skipped.
func (tr *TelegramReader) fetchAllProfilePhotos(ctx context.Context, api *tg.Client) {
	totalSlots := len(tr.channels)
	if tr.privates != nil {
		totalSlots += len(tr.privates.ordered)
	}
	pics := make(map[string][]byte, totalSlots)

	// Public channels.
	for _, username := range tr.channels {
		if ctx.Err() != nil {
			return
		}
		username = strings.TrimSpace(username)
		if username == "" {
			continue
		}
		rp, err := tr.resolvePeer(ctx, api, username)
		if err != nil {
			log.Printf("[profile-pics] resolve %s: %v", username, err)
			continue
		}
		if rp.photoID == 0 {
			continue
		}
		// Skip the download if we already pushed this photoID.
		tr.mu.Lock()
		if tr.lastPhotoID == nil {
			tr.lastPhotoID = map[string]int64{}
		}
		prevID, hadPrev := tr.lastPhotoID[username]
		tr.mu.Unlock()
		if hadPrev && prevID == rp.photoID {
			continue
		}
		body, err := tr.fetchProfilePhoto(ctx, api, rp.peer, rp.photoID)
		if err != nil {
			log.Printf("[profile-pics] download %s (id=%d): %v", username, rp.photoID, err)
			continue
		}
		pics[username] = body
		tr.mu.Lock()
		tr.lastPhotoID[username] = rp.photoID
		tr.mu.Unlock()
	}

	// Private channels — keyed by privateChannelID to match the Feed
	// Name.
	if tr.privates != nil {
		for _, hash := range tr.privates.ordered {
			if ctx.Err() != nil {
				return
			}
			rp, ok := tr.privates.get(hash)
			if !ok || rp.photoID == 0 {
				continue
			}
			key := privateChannelID(hash)
			tr.mu.Lock()
			if tr.lastPhotoID == nil {
				tr.lastPhotoID = map[string]int64{}
			}
			prevID, hadPrev := tr.lastPhotoID[key]
			tr.mu.Unlock()
			if hadPrev && prevID == rp.photoID {
				continue
			}
			body, err := tr.fetchProfilePhoto(ctx, api, rp.peer, rp.photoID)
			if err != nil {
				log.Printf("[profile-pics] download private %s (id=%d): %v", rp.title, rp.photoID, err)
				continue
			}
			pics[key] = body
			tr.mu.Lock()
			tr.lastPhotoID[key] = rp.photoID
			tr.mu.Unlock()
		}
	}

	if len(pics) == 0 {
		return
	}
	// Merge so other readers' contributions (e.g. X "x:" entries) survive.
	total := tr.feed.MergeProfilePics(pics)
	log.Printf("[profile-pics] cycle done: %d new, %d total in bundle", len(pics), total)
}
