package dropbox

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Metadata is a file or folder entry. Tag is "file", "folder" or "deleted".
type Metadata struct {
	Tag            string `json:".tag"`
	Name           string `json:"name"`
	ID             string `json:"id"`
	PathLower      string `json:"path_lower"`
	PathDisplay    string `json:"path_display"`
	ClientModified string `json:"client_modified,omitempty"`
	ServerModified string `json:"server_modified,omitempty"`
	Rev            string `json:"rev,omitempty"`
	Size           int64  `json:"size,omitempty"`
	ContentHash    string `json:"content_hash,omitempty"`
}

// IsDir reports whether the entry is a folder.
func (m Metadata) IsDir() bool { return m.Tag == "folder" }

// Modified returns the server modification time (zero for folders).
func (m Metadata) Modified() time.Time {
	t, _ := time.Parse(time.RFC3339, m.ServerModified)
	return t
}

// APIPath converts a slash path to Dropbox form: the root is "" and everything else starts with "/".
func APIPath(p string) string {
	if p == "/" {
		return ""
	}
	return p
}

type listFolderResult struct {
	Entries []Metadata `json:"entries"`
	Cursor  string     `json:"cursor"`
	HasMore bool       `json:"has_more"`
}

// ListFolder lists a folder, following cursors until has_more is false, and calls fn per page.
func (c *Client) ListFolder(ctx context.Context, path string, recursive bool, fn func([]Metadata) error) error {
	var res listFolderResult
	arg := map[string]any{"path": APIPath(path), "recursive": recursive, "limit": 2000, "include_non_downloadable_files": true}
	if err := c.RPC(ctx, "files/list_folder", arg, &res); err != nil {
		return err
	}
	for {
		if err := fn(res.Entries); err != nil {
			return err
		}
		if !res.HasMore {
			return nil
		}
		cursor := res.Cursor
		res = listFolderResult{}
		if err := c.RPC(ctx, "files/list_folder/continue", map[string]string{"cursor": cursor}, &res); err != nil {
			return err
		}
	}
}

// GetMetadata returns metadata for a path.
func (c *Client) GetMetadata(ctx context.Context, path string) (*Metadata, error) {
	var m Metadata
	if err := c.RPC(ctx, "files/get_metadata", map[string]any{"path": path}, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// TempLink is a get_temporary_link result.
type TempLink struct {
	Metadata Metadata `json:"metadata"`
	Link     string   `json:"link"`
}

// GetTemporaryLink returns a direct, pre-authenticated download link (valid about four hours).
func (c *Client) GetTemporaryLink(ctx context.Context, path string) (*TempLink, error) {
	var t TempLink
	if err := c.RPC(ctx, "files/get_temporary_link", map[string]string{"path": path}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Download opens files/download. Pass a Range header to read part of the file.
func (c *Client) Download(ctx context.Context, path string, headers http.Header) (*http.Response, error) {
	return c.Content(ctx, "files/download", map[string]string{"path": path}, headers)
}

// GetPreview opens files/get_preview (PDF for documents, HTML for spreadsheets).
func (c *Client) GetPreview(ctx context.Context, path string) (*http.Response, error) {
	return c.Content(ctx, "files/get_preview", map[string]string{"path": path}, nil)
}

// SearchMatch is one search_v2 match.
type SearchMatch struct {
	Metadata struct {
		Tag      string   `json:".tag"`
		Metadata Metadata `json:"metadata"`
	} `json:"metadata"`
}

// SearchResult is a search_v2 / search/continue_v2 response.
type SearchResult struct {
	Matches []SearchMatch `json:"matches"`
	HasMore bool          `json:"has_more"`
	Cursor  string        `json:"cursor"`
}

// SearchOptions maps to search_v2 options.
type SearchOptions struct {
	Path           string   `json:"path,omitempty"`
	MaxResults     int      `json:"max_results,omitempty"`
	FileStatus     string   `json:"file_status,omitempty"`
	FilenameOnly   bool     `json:"filename_only,omitempty"`
	FileCategories []string `json:"file_categories,omitempty"`
}

// Search starts a search.
func (c *Client) Search(ctx context.Context, query string, opts SearchOptions) (*SearchResult, error) {
	var r SearchResult
	err := c.RPC(ctx, "files/search_v2", map[string]any{"query": query, "options": opts}, &r)
	return &r, err
}

// SearchContinue fetches the next page of a search.
func (c *Client) SearchContinue(ctx context.Context, cursor string) (*SearchResult, error) {
	var r SearchResult
	err := c.RPC(ctx, "files/search/continue_v2", map[string]string{"cursor": cursor}, &r)
	return &r, err
}

// ThumbResult is one get_thumbnail_batch entry result.
type ThumbResult struct {
	OK       bool
	Data     []byte
	Metadata Metadata
	Err      string
}

// MaxThumbBatch is the documented per-call limit of get_thumbnail_batch.
const MaxThumbBatch = 25

// GetThumbnailBatch fetches thumbnails for up to 25 paths. size is e.g. "w256h256".
func (c *Client) GetThumbnailBatch(ctx context.Context, paths []string, size string) ([]ThumbResult, error) {
	if len(paths) > MaxThumbBatch {
		return nil, fmt.Errorf("get_thumbnail_batch: at most %d paths", MaxThumbBatch)
	}
	entries := make([]map[string]string, len(paths))
	for i, p := range paths {
		entries[i] = map[string]string{"path": p, "format": "jpeg", "size": size, "mode": "fitone_bestfit"}
	}
	// get_thumbnail_batch lives on the content host but is RPC-style: JSON body, JSON response.
	var raw struct {
		Entries []struct {
			Tag       string   `json:".tag"`
			Metadata  Metadata `json:"metadata"`
			Thumbnail string   `json:"thumbnail"`
			Failure   struct {
				Tag string `json:".tag"`
			} `json:"failure"`
		} `json:"entries"`
	}
	if err := c.contentRPC(ctx, "files/get_thumbnail_batch", map[string]any{"entries": entries}, &raw); err != nil {
		return nil, err
	}
	out := make([]ThumbResult, len(raw.Entries))
	for i, e := range raw.Entries {
		if e.Tag != "success" {
			out[i] = ThumbResult{Err: e.Failure.Tag}
			continue
		}
		b, err := base64.StdEncoding.DecodeString(e.Thumbnail)
		if err != nil {
			out[i] = ThumbResult{Err: "decode"}
			continue
		}
		out[i] = ThumbResult{OK: true, Data: b, Metadata: e.Metadata}
	}
	return out, nil
}

// RelocationEntry is a from/to pair for move/copy.
type RelocationEntry struct {
	FromPath string `json:"from_path"`
	ToPath   string `json:"to_path"`
}

// Move moves or renames a single entry.
func (c *Client) Move(ctx context.Context, from, to string) error {
	return c.RPC(ctx, "files/move_v2", map[string]any{"from_path": from, "to_path": to, "autorename": false}, nil)
}

// Copy copies a single entry.
func (c *Client) Copy(ctx context.Context, from, to string) error {
	return c.RPC(ctx, "files/copy_v2", map[string]any{"from_path": from, "to_path": to, "autorename": false}, nil)
}

// Delete deletes a single entry. A path that is already gone is success: Dropbox documents
// path_lookup/not_found as "it no longer exists", which is what a retried delete sees after the
// first attempt removed it and its response was lost.
func (c *Client) Delete(ctx context.Context, path string) error {
	err := c.RPC(ctx, "files/delete_v2", map[string]string{"path": path}, nil)
	if HasSummary(err, "not_found") {
		return nil
	}
	return err
}

// BatchFailure describes a failed entry in a batch job.
type BatchFailure struct {
	Index   int    `json:"index"`
	Summary string `json:"summary"`
}

type batchLaunch struct {
	Tag        string `json:".tag"`
	AsyncJobID string `json:"async_job_id"`
	Entries    []struct {
		Tag     string         `json:".tag"`
		Failure map[string]any `json:"failure"`
	} `json:"entries"`
}

// BatchResult is the per-entry result of a batch call and whether an earlier attempt of that same
// call was ambiguous. A failure summary is definitive only when Retried is false.
type BatchResult struct {
	Failures []BatchFailure
	Retried  bool
}

// MoveBatch moves entries and waits for the async job.
func (c *Client) MoveBatch(ctx context.Context, entries []RelocationEntry) ([]BatchFailure, error) {
	res, err := c.MoveBatchOutcome(ctx, entries)
	return res.Failures, err
}

// MoveBatchOutcome is MoveBatch with the retry ambiguity a caller needs to judge per-entry failures.
func (c *Client) MoveBatchOutcome(ctx context.Context, entries []RelocationEntry) (BatchResult, error) {
	bf, retried, err := c.runBatch(ctx, "files/move_batch_v2", "files/move_batch/check_v2",
		map[string]any{"entries": entries, "autorename": false})
	return BatchResult{Failures: bf, Retried: retried}, err
}

// CopyBatch copies entries and waits for the async job.
func (c *Client) CopyBatch(ctx context.Context, entries []RelocationEntry) ([]BatchFailure, error) {
	res, err := c.CopyBatchOutcome(ctx, entries)
	return res.Failures, err
}

// CopyBatchOutcome is CopyBatch with the retry ambiguity a caller needs to judge per-entry failures.
func (c *Client) CopyBatchOutcome(ctx context.Context, entries []RelocationEntry) (BatchResult, error) {
	bf, retried, err := c.runBatch(ctx, "files/copy_batch_v2", "files/copy_batch/check_v2",
		map[string]any{"entries": entries, "autorename": false})
	return BatchResult{Failures: bf, Retried: retried}, err
}

// DeleteBatch deletes paths and waits for the async job. An entry that is already gone is not a
// failure, for the same reason single Delete treats path_lookup/not_found as success.
func (c *Client) DeleteBatch(ctx context.Context, paths []string) ([]BatchFailure, error) {
	es := make([]map[string]string, len(paths))
	for i, p := range paths {
		es[i] = map[string]string{"path": p}
	}
	bf, _, err := c.runBatch(ctx, "files/delete_batch", "files/delete_batch/check", map[string]any{"entries": es})
	if err != nil {
		return nil, err
	}
	kept := bf[:0]
	for _, f := range bf {
		if strings.Contains(f.Summary, "not_found") {
			continue
		}
		kept = append(kept, f)
	}
	return kept, nil
}

// Op names one non-idempotent relocation.
type Op int

const (
	OpMove Op = iota
	OpCopy
)

// Relocation describes one intended move or copy and the state observed before it was attempted.
// A failure with an unknown outcome is reconciled from this evidence, never from "the source is
// gone" or "a file with the same name and content exists".
type Relocation struct {
	Op          Op
	From        string
	To          string
	Name        string
	SourceID    string // source id observed before the write; a move keeps it
	SourceRev   string // source revision observed before the write
	DestChecked bool   // the destination was inspected before the write
	DestExisted bool   // and it already existed, so a copy can only be a conflict
}

// RelocationOutcome is the evidence-based verdict for a failed relocation.
type RelocationOutcome int

const (
	// RelocationFailed means the requested change is known not to have happened.
	RelocationFailed RelocationOutcome = iota
	// RelocationApplied means the destination carries evidence that ties it to this write.
	RelocationApplied
	// RelocationUnknown means the available metadata cannot settle the outcome. Callers must say
	// so explicitly and invalidate cached listings; they must not call it success.
	RelocationUnknown
)

// ReconcileRelocation decides a move or copy that failed ambiguously. Move success requires the
// destination's stable id to equal the source id observed before the write; source disappearance
// alone proves nothing. Copy success requires the destination to have been absent before the write
// and to carry the source's unchanged content now, because an existing same-content file returns
// the same conflict a retried copy does. Every other case is unknown rather than success.
func (c *Client) ReconcileRelocation(ctx context.Context, r Relocation) RelocationOutcome {
	dst, err := c.GetMetadata(ctx, r.To)
	dstFound := err == nil
	if err != nil && !HasSummary(err, "not_found") {
		return RelocationUnknown
	}
	if r.Op == OpMove {
		return c.reconcileMove(ctx, r, dst, dstFound)
	}
	return c.reconcileCopy(ctx, r, dst, dstFound)
}

func (c *Client) reconcileMove(ctx context.Context, r Relocation, dst *Metadata, dstFound bool) RelocationOutcome {
	if r.SourceID == "" {
		return RelocationUnknown // no pre-move identity to compare the destination with
	}
	if dstFound && dst.ID == r.SourceID && (r.Name == "" || strings.EqualFold(dst.Name, r.Name)) {
		return RelocationApplied
	}
	src, err := c.GetMetadata(ctx, r.From)
	if err == nil && src.ID == r.SourceID {
		return RelocationFailed // the source still exists at its pre-move identity
	}
	// The source is gone, but the destination does not carry its id. It moved somewhere else, or
	// the metadata is incomplete; only an explicit unknown is honest here.
	return RelocationUnknown
}

func (c *Client) reconcileCopy(ctx context.Context, r Relocation, dst *Metadata, dstFound bool) RelocationOutcome {
	if r.DestChecked && r.DestExisted {
		return RelocationFailed // occupied before the write: a genuine conflict, not a copy
	}
	if !dstFound {
		return RelocationFailed // no destination entry now, so this copy did not create one
	}
	if !r.DestChecked {
		// Without pre-write destination evidence, an existing same-content file is
		// indistinguishable from a completed copy. Never turn that into success.
		return RelocationUnknown
	}
	if r.Name != "" && !strings.EqualFold(dst.Name, r.Name) {
		return RelocationUnknown
	}
	src, err := c.GetMetadata(ctx, r.From)
	if err != nil {
		return RelocationUnknown
	}
	if r.SourceID != "" && src.ID != r.SourceID {
		return RelocationUnknown
	}
	if r.SourceRev != "" && src.Rev != r.SourceRev {
		return RelocationUnknown
	}
	if src.IsDir() != dst.IsDir() {
		return RelocationUnknown
	}
	if src.IsDir() {
		// A folder copy has no content hash tying the new entry to its source.
		return RelocationUnknown
	}
	if src.ContentHash == "" || dst.ContentHash == "" {
		return RelocationUnknown
	}
	if src.Size == dst.Size && src.ContentHash == dst.ContentHash {
		return RelocationApplied
	}
	return RelocationUnknown
}

// ReconcileDelete decides a delete that failed ambiguously. The entry being gone is the outcome the
// caller asked for; the entry still being present means the delete did not happen.
func (c *Client) ReconcileDelete(ctx context.Context, path string) RelocationOutcome {
	_, err := c.GetMetadata(ctx, path)
	switch {
	case err == nil:
		return RelocationFailed
	case HasSummary(err, "not_found"):
		return RelocationApplied
	default:
		return RelocationUnknown
	}
}

func (c *Client) runBatch(ctx context.Context, launch, check string, arg any) ([]BatchFailure, bool, error) {
	ctx, marker := withRetryMarker(ctx)
	var l batchLaunch
	if err := c.RPC(ctx, launch, arg, &l); err != nil {
		return nil, marker.ambiguous.Load(), err
	}
	if l.Tag == "complete" {
		return failures(l), marker.ambiguous.Load(), nil
	}
	if l.AsyncJobID == "" {
		return nil, marker.ambiguous.Load(), fmt.Errorf("%s: unexpected response tag %q", launch, l.Tag)
	}
	wait := 300 * time.Millisecond
	for {
		if err := sleep(ctx, wait); err != nil {
			return nil, marker.ambiguous.Load(), err
		}
		var st batchLaunch
		if err := c.RPC(ctx, check, map[string]string{"async_job_id": l.AsyncJobID}, &st); err != nil {
			return nil, marker.ambiguous.Load(), err
		}
		switch st.Tag {
		case "in_progress":
			if wait < 3*time.Second {
				wait = wait * 3 / 2
			}
			continue
		case "complete":
			return failures(st), marker.ambiguous.Load(), nil
		default:
			return nil, marker.ambiguous.Load(), fmt.Errorf("%s: job ended with %q", check, st.Tag)
		}
	}
}

func failures(l batchLaunch) []BatchFailure {
	var out []BatchFailure
	for i, e := range l.Entries {
		if e.Tag == "failure" {
			out = append(out, BatchFailure{Index: i, Summary: summarize(e.Failure)})
		}
	}
	return out
}

// summarize flattens a tagged-union error into "a/b/c".
func summarize(m map[string]any) string {
	s := ""
	for m != nil {
		tag, _ := m[".tag"].(string)
		if tag == "" {
			break
		}
		if s != "" {
			s += "/"
		}
		s += tag
		next, _ := m[tag].(map[string]any)
		m = next
	}
	return s
}

// Account is a users/get_current_account response (subset).
type Account struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Name      struct {
		DisplayName string `json:"display_name"`
	} `json:"name"`
}

// CurrentAccount returns the authorized account.
func (c *Client) CurrentAccount(ctx context.Context) (*Account, error) {
	var a Account
	if err := c.RPC(ctx, "users/get_current_account", nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// Revoke revokes the current access token (which also invalidates the refresh grant).
func (c *Client) Revoke(ctx context.Context) error {
	return c.RPC(ctx, "auth/token/revoke", nil, nil)
}

// contentRPC posts a JSON body to the content host and decodes a JSON response.
func (c *Client) contentRPC(ctx context.Context, endpoint string, arg, out any) error {
	return c.rpcAt(ctx, c.ep.Content, endpoint, arg, out)
}

// Drain discards and closes a body so the connection can be reused.
func Drain(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	rc.Close()
}
