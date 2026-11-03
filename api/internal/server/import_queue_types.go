package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const queueNamespace = "_drive/import-queues"
const queueSnapshotLimit = 16 << 20
const queueMetadataLimit = 128 << 20
const queueOwnerItems = 10000
const queueProcessItems = 50000
const queueLifetime = 7 * 24 * time.Hour

var errQueueState = errors.New("queue_state_unavailable")
var errQueueCapacity = errors.New("queue_capacity")

type queueSource struct {
	Type   string `json:"type"`
	URL    string `json:"url"`
	Name   string `json:"name,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}
type queueRequest struct {
	SubmissionID string        `json:"submissionId"`
	Path         string        `json:"path"`
	Space        string        `json:"space"`
	Existing     string        `json:"existing"`
	Sources      []queueSource `json:"sources"`
}

// QueueSummary and QueueItem are intentionally separate from private records.
type QueueSummary struct {
	ID                     string    `json:"id"`
	Path                   string    `json:"path"`
	State                  string    `json:"state"`
	AcceptedAt             time.Time `json:"acceptedAt"`
	ExpiresAt              time.Time `json:"expiresAt"`
	TotalItems             int       `json:"totalItems"`
	CompletedItems         int       `json:"completedItems"`
	FailedItems            int       `json:"failedItems"`
	CancelledItems         int       `json:"cancelledItems"`
	QueuedItems            int       `json:"queuedItems"`
	ActiveItems            int       `json:"activeItems"`
	SavingItems            int       `json:"savingItems"`
	NeedsVerificationItems int       `json:"needsVerificationItems"`
	TotalBytes             int64     `json:"totalBytes"`
	CompletedBytes         int64     `json:"completedBytes"`
	UnknownSizeItems       int       `json:"unknownSizeItems"`
	DiscoveryState         string    `json:"discoveryState,omitempty"`
	ErrorCode              string    `json:"errorCode,omitempty"`
	NormalizedNames        int       `json:"normalizedNames,omitempty"`
}
type QueueItem struct {
	ID            string     `json:"id"`
	QueueID       string     `json:"queueId"`
	Position      int        `json:"position"`
	Name          string     `json:"name"`
	Path          string     `json:"path"`
	State         string     `json:"state"`
	Bytes         int64      `json:"bytes"`
	Total         int64      `json:"total"`
	Attempts      int        `json:"attempts"`
	TotalAttempts int        `json:"totalAttempts,omitempty"`
	SHA256        string     `json:"sha256,omitempty"`
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
	ErrorCode     string     `json:"errorCode,omitempty"`
	Reused        bool       `json:"reused,omitempty"`
}
type queueManifest struct {
	Schema                 int
	ID, Owner, Fingerprint string
	AcceptedAt, ExpiresAt  time.Time
	Request                queueRequest
}
type queueRecord struct {
	QueueItem
	Source       int
	ProviderPath string
	Directory    bool
	ExpectedSize int64
	ExpectedSHA  string
	Deadline     time.Time
	Scratch      string
	Generation   int
	History      []int
}
type queueWalk struct {
	Attempts                  int
	NextAttemptAt             time.Time
	Deadline                  time.Time
	Source                    int
	ProviderPath, Destination string
	Offset, Total             int
}
type queueState struct {
	Schema                 int
	ID, Owner, Fingerprint string
	Revision               uint64
	Control                string
	Items                  []queueRecord
	Walk                   []queueWalk
	Discovered             []bool
	DiscoveryError         string
	NormalizedNames        int
	Pages                  int
	// Original target identities prevent distinct normalized paths from merging.
	Targets map[string]string
}
type durableQueue struct {
	mu            sync.Mutex // transition serialization, never the global progress lock
	manifest      queueManifest
	state         queueState
	frozen        bool
	blockedCode   string
	busy          map[string]bool
	cancels       map[string]func()
	metadataBytes int
	stateBytes    int
	progress      map[string]int64
}
type queueManager struct {
	mu              sync.Mutex
	admission       sync.Mutex
	queues          map[string]*durableQueue
	metadataBytes   int
	ownerItems      map[string]int
	totalItems      int
	stop            chan struct{}
	done            chan struct{}
	lastOwner       string
	stopOnce        sync.Once
	capacityUnknown bool
}

func validHex(s string, n int) bool {
	if len(s) != n || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func queueIdentity(owner, submission string) string {
	h := sha256.Sum256([]byte(owner + ":" + submission))
	return hex.EncodeToString(h[:])
}

// Reject before normalization: CleanObjectPath alone would hide traversal aliases.
func strictQueuePath(p string, empty bool) bool {
	if p == "" {
		return empty
	}
	if !utf8.ValidString(p) || len(p) > 900 || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return false
	}
	parts := strings.Split(p, "/")
	if len(parts) > 32 {
		return false
	}
	for _, c := range parts {
		if c == "" || c == "." || c == ".." || len(c) > 255 {
			return false
		}
	}
	return true
}
func validateQueueRequest(in *queueRequest) error {
	if !validHex(in.SubmissionID, 32) || !strictQueuePath(in.Path, true) || in.Path != strings.TrimSpace(in.Path) || len(in.Sources) < 1 || len(in.Sources) > 1000 {
		return errors.New("invalid_queue_request")
	}
	if in.Space != "" && in.Space != "personal" {
		return errors.New("personal_space_required")
	}
	in.Space = "personal"
	if in.Existing == "" {
		in.Existing = "error"
	}
	if in.Existing != "error" && in.Existing != "skip_verified" {
		return errors.New("invalid_existing_policy")
	}
	for i := range in.Sources {
		src := &in.Sources[i]
		u, err := url.Parse(src.URL)
		if err != nil || validateRemoteURL(u) != nil {
			return errors.New("invalid_source")
		}
		switch src.Type {
		case "direct":
			if src.SHA256 != "" && !validHex(src.SHA256, 64) || src.Name != "" && !validDownloadName(src.Name) {
				return errors.New("invalid_source")
			}
			if src.Name == "" {
				src.Name = path.Base(u.Path)
				if !validDownloadName(src.Name) {
					src.Name = "download"
				}
			}
		case "yandex":
			if !validYandexShare(u) || src.Name != "" || src.SHA256 != "" {
				return errors.New("invalid_provider_source")
			}
		default:
			return errors.New("invalid_source_type")
		}
	}
	return nil
}
func (q *durableQueue) summary() QueueSummary {
	m, st := &q.manifest, &q.state
	out := QueueSummary{ID: m.ID, Path: m.Request.Path, State: "queued", AcceptedAt: m.AcceptedAt, ExpiresAt: m.ExpiresAt, TotalItems: len(st.Items), NormalizedNames: st.NormalizedNames, DiscoveryState: "complete", ErrorCode: st.DiscoveryError}
	discovering := false
	for _, v := range st.Discovered {
		if !v {
			discovering = true
		}
	}
	if discovering {
		out.DiscoveryState = "pending"
		out.State = "discovering"
	}
	if st.DiscoveryError != "" {
		out.DiscoveryState = "failed"
	}
	for _, it := range st.Items {
		if it.Total > math.MaxInt64-out.TotalBytes {
			out.TotalBytes = math.MaxInt64
		} else {
			out.TotalBytes += it.Total
		}
		if it.ExpectedSize < 0 && it.Total == 0 && !it.Directory && it.State != "complete" && it.State != "saving" {
			out.UnknownSizeItems++
		}
		switch it.State {
		case "complete":
			out.CompletedItems++
			if it.Total > math.MaxInt64-out.CompletedBytes {
				out.CompletedBytes = math.MaxInt64
			} else {
				out.CompletedBytes += it.Total
			}
		case "failed":
			out.FailedItems++
		case "cancelled", "expired":
			out.CancelledItems++
		case "saving":
			out.SavingItems++
			out.ActiveItems++
		case "resolving", "downloading":
			out.ActiveItems++
		case "needs_verification":
			out.NeedsVerificationItems++
		default:
			out.QueuedItems++
		}
	}
	switch {
	case q.frozen:
		out.State = "blocked"
		out.ErrorCode = "queue_state_unavailable"
		if q.blockedCode != "" {
			out.ErrorCode = q.blockedCode
		}
	case st.Control == "paused":
		out.State = "paused"
	case st.Control == "cancelled" && out.ActiveItems > 0:
		out.State = "cancelling"
	case st.Control == "cancelled":
		out.State = "cancelled"
	case st.Control == "expired":
		out.State = "expired"
	case out.NeedsVerificationItems > 0:
		out.State = "needs_verification"
	case out.ActiveItems > 0:
		out.State = "running"
	case !discovering && out.QueuedItems == 0:
		out.State = "complete"
		if out.FailedItems > 0 || st.DiscoveryError != "" || out.CancelledItems > 0 {
			out.State = "partial"
		}
	}
	return out
}
func queueTerminal(state string) bool {
	switch state {
	case "complete", "failed", "cancelled", "expired", "needs_verification":
		return true
	}
	return false
}

func queueRetainedItems(st queueState) int {
	n := len(st.Items)
	for _, done := range st.Discovered {
		if !done {
			n++
		}
	}
	return n
}

func (q *durableQueue) publicItem(it queueRecord) QueueItem {
	out := it.QueueItem
	if it.State == "downloading" {
		out.Bytes = max(out.Bytes, q.progress[it.ID])
	}
	return out
}

// encoding/json replaces invalid UTF-8 and unpaired JSON surrogates. Reject
// those encodings before decoding so private provider locators cannot alias.
func validQueueJSON(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		value, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
