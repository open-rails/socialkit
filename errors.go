package socialkit

import "errors"

// Sentinel errors an EntityResolver returns to gate a target. socialkit maps
// them to HTTP status and never leaks which one to unauthorized callers beyond
// the status code.
var (
	// ErrNotFound: the (type,id) does not exist. -> 404
	ErrNotFound = errors.New("socialkit: entity not found")
	// ErrNotVisible: exists but unpublished or soft-deleted. -> 404 (hidden).
	ErrNotVisible = errors.New("socialkit: entity not visible")
	// ErrForbidden: visible but the actor may not consume it (premium-locked). -> 403
	ErrForbidden = errors.New("socialkit: entity not accessible")
)

// errUnsupportedMedia is the default MediaStore's response (no store wired).
var errUnsupportedMedia = errors.New("socialkit: no MediaStore configured")
