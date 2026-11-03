package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

var errSourceChanged = errors.New("source_changed")

func queueSourceError(err error) string {
	if errors.Is(err, errSourceChanged) {
		return "source_changed"
	}
	return "provider_unavailable"
}
func validYandexShare(u *url.URL) bool {
	if u.Scheme != "https" || u.Port() != "" && u.Port() != "443" || u.RawQuery != "" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "disk.yandex.ru", "disk.yandex.com", "disk.yandex.com.tr", "yadi.sk":
	default:
		return false
	}
	return (strings.HasPrefix(u.Path, "/d/") || strings.HasPrefix(u.Path, "/i/")) && len(u.Path) > 3 && !strings.Contains(u.Path[3:], "/")
}

type yandexResource struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Size     *int64 `json:"size"`
	SHA256   string `json:"sha256"`
	Embedded *struct {
		Items  []yandexResource `json:"items"`
		Total  int              `json:"total"`
		Offset int              `json:"offset"`
		Limit  int              `json:"limit"`
	} `json:"_embedded"`
}

func (s *Server) yandexJSON(ctx context.Context, src queueSource, providerPath string, offset int, download bool, out any) error {
	if !strictQueuePath(providerPath, true) {
		return errors.New("invalid_provider_path")
	}
	endpoint := "https://cloud-api.yandex.net/v1/disk/public/resources"
	if download {
		endpoint += "/download"
	}
	u, _ := url.Parse(endpoint)
	query := url.Values{"public_key": {src.URL}}
	if providerPath != "" {
		query.Set("path", "/"+providerPath)
	}
	if !download {
		query.Set("limit", "200")
		query.Set("offset", strconv.Itoa(offset))
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("provider_unavailable")
	}
	req.Header.Set("User-Agent", "Remote-Drive/1.0")
	client := *s.imports.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.Host != "cloud-api.yandex.net" || len(via) >= 5 {
			return errors.New("provider_redirect")
		}
		if s.imports.client.CheckRedirect != nil {
			return s.imports.client.CheckRedirect(req, via)
		}
		return validateRemoteURL(req.URL)
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("provider_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.ContentLength > 2<<20 {
		return errors.New("provider_unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return errors.New("provider_response_limit")
	}
	if !validQueueJSON(data) || json.Unmarshal(data, out) != nil {
		return errors.New("provider_response_invalid")
	}
	return nil
}
func validYandexFile(resource yandexResource) bool {
	return resource.Type == "file" && resource.Size != nil && *resource.Size >= 0 && (resource.SHA256 == "" || validHex(resource.SHA256, 64))
}
func (s *Server) resolveYandex(ctx context.Context, src queueSource, it queueRecord) (string, error) {
	var resource yandexResource
	if err := s.yandexJSON(ctx, src, it.ProviderPath, 0, false, &resource); err != nil {
		return "", err
	}
	if !validYandexFile(resource) || *resource.Size != it.ExpectedSize || resource.SHA256 != it.ExpectedSHA {
		return "", errSourceChanged
	}
	var result struct {
		Href   string `json:"href"`
		Method string `json:"method"`
	}
	if err := s.yandexJSON(ctx, src, it.ProviderPath, 0, true, &result); err != nil {
		return "", err
	}
	u, err := url.Parse(result.Href)
	if err != nil || validateRemoteURL(u) != nil || result.Method != "" && result.Method != "GET" {
		return "", errors.New("provider_download_invalid")
	}
	return result.Href, nil
}
func normalizedProviderName(name string) (string, bool, error) {
	if !strictQueuePath(name, false) || strings.Contains(name, "/") {
		return "", false, errors.New("invalid_provider_path")
	}
	normalized := strings.TrimSpace(name)
	if !validDownloadName(normalized) {
		return "", false, errors.New("invalid_provider_path")
	}
	return normalized, normalized != name, nil
}
func queueTargetConflict(targets map[string]string, dest, identity string, dir bool) bool {
	existing, ok := targets[dest]
	if ok && existing != identity {
		return true
	}
	for parent := path.Dir(dest); parent != "." && parent != "/"; parent = path.Dir(parent) {
		if v, ok := targets[parent]; ok && !strings.HasPrefix(v, "d|") {
			return true
		}
	}
	if !dir {
		for key := range targets {
			if strings.HasPrefix(key, dest+"/") {
				return true
			}
		}
	}
	return false
}
func addYandexRecord(st *queueState, m queueManifest, source int, providerPath, dest string, resource yandexResource) error {
	dir := resource.Type == "dir"
	if !dir && !validYandexFile(resource) || !strictQueuePath(providerPath, true) || !strictQueuePath(dest, false) {
		return errors.New("invalid_provider_entry")
	}
	identity := fmt.Sprintf("f|%d:%s", source, providerPath)
	if dir {
		identity = fmt.Sprintf("d|%d:%s", source, providerPath)
	}
	if queueTargetConflict(st.Targets, dest, identity, dir) {
		return errors.New("destination_collision")
	}
	if st.Targets[dest] == identity {
		return errors.New("provider_inventory_changed")
	}
	st.Targets[dest] = identity
	size := int64(0)
	if !dir {
		size = *resource.Size
	}
	st.Items = append(st.Items, queueRecord{QueueItem: QueueItem{ID: queueIdentity(m.ID, identity), QueueID: m.ID, Position: len(st.Items), Name: path.Base(dest), Path: dest, State: "queued", Total: size}, Source: source, ProviderPath: providerPath, Directory: dir, ExpectedSize: size, ExpectedSHA: resource.SHA256})
	return nil
}
func (s *Server) discoverQueue(ctx context.Context, q *durableQueue) {
	q.mu.Lock()
	if q.frozen || q.state.Control != "running" || len(q.state.Walk) == 0 {
		q.mu.Unlock()
		return
	}
	index := -1
	for i, w := range q.state.Walk {
		if !time.Now().Before(w.NextAttemptAt) {
			index = i
			break
		}
	}
	if index < 0 {
		q.mu.Unlock()
		return
	}
	nextStart := cloneQueueState(q.state)
	walk := nextStart.Walk[index]
	if walk.Deadline.IsZero() {
		walk.Deadline = minQueueTime(time.Now().Add(s.imports.options.Timeout), q.manifest.ExpiresAt)
	}
	if walk.Attempts >= 3 || !time.Now().Before(walk.Deadline) {
		nextStart.Discovered[walk.Source] = true
		nextStart.DiscoveryError = "provider_deadline_or_attempts_exceeded"
		filtered := nextStart.Walk[:0]
		for _, w := range nextStart.Walk {
			if w.Source != walk.Source {
				filtered = append(filtered, w)
			}
		}
		nextStart.Walk = filtered
		cc, c := queueContext()
		_ = s.commitQueue(cc, q, nextStart)
		c()
		q.mu.Unlock()
		return
	}
	walk.Attempts++
	nextStart.Walk[index] = walk
	cc, c := queueContext()
	commitErr := s.commitQueue(cc, q, nextStart)
	c()
	src := q.manifest.Request.Sources[walk.Source]
	q.mu.Unlock()
	if commitErr != nil {
		return
	}
	attemptCtx, cancel := context.WithDeadline(ctx, walk.Deadline)
	defer cancel()
	var resource yandexResource
	err := s.yandexJSON(attemptCtx, src, walk.ProviderPath, walk.Offset, false, &resource)
	// Discovery shares a bounded, persisted three-attempt budget per page.
	// Failed sources are visible and never prevent independent source work.
	owned, total := s.retainedQueueItems(q.manifest.Owner)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.frozen || q.state.Control == "cancelled" || len(q.state.Walk) == 0 {
		return
	}
	next := cloneQueueState(q.state)
	// Move the selected page to the front for atomic page replacement below.
	selected := next.Walk[index]
	next.Walk = append(next.Walk[:index], next.Walk[index+1:]...)
	next.Walk = append([]queueWalk{selected}, next.Walk...)
	if err != nil && walk.Attempts < 3 && time.Now().Before(walk.Deadline) {
		next = cloneQueueState(q.state)
		next.Walk[index].NextAttemptAt = time.Now().Add(time.Duration(1<<walk.Attempts) * time.Second)
		cc, c := queueContext()
		_ = s.commitQueue(cc, q, next)
		c()
		return
	}
	next.Pages++
	if next.Pages > 10000 {
		err = errors.New("provider_page_limit")
	}
	if err == nil && resource.Type == "file" && walk.ProviderPath == "" && walk.Offset == 0 {
		name, changed, e := normalizedProviderName(resource.Name)
		if e != nil {
			err = e
		} else {
			err = addYandexRecord(&next, q.manifest, walk.Source, "", path.Join(walk.Destination, name), resource)
			if changed {
				next.NormalizedNames++
			}
		}
		next.Walk = next.Walk[1:]
		next.Discovered[walk.Source] = true
	} else if err == nil {
		emb := resource.Embedded
		if resource.Type != "dir" || emb == nil || emb.Offset != walk.Offset || emb.Limit < 1 || emb.Limit > 200 || len(emb.Items) > emb.Limit || emb.Total < 0 || emb.Total > queueOwnerItems || walk.Total >= 0 && emb.Total != walk.Total || emb.Offset+len(emb.Items) > emb.Total || len(emb.Items) == 0 && emb.Offset < emb.Total {
			err = errors.New("provider_inventory_changed")
		} else {
			next.Walk = next.Walk[1:]
			for _, child := range emb.Items {
				name, changed, e := normalizedProviderName(child.Name)
				if e != nil {
					err = e
					break
				}
				providerPath := path.Join(walk.ProviderPath, child.Name)
				dest := path.Join(walk.Destination, name)
				if !strictQueuePath(providerPath, false) {
					err = errors.New("invalid_provider_path")
					break
				}
				if e = addYandexRecord(&next, q.manifest, walk.Source, providerPath, dest, child); e != nil {
					err = e
					break
				}
				if changed {
					next.NormalizedNames++
				}
				if child.Type == "dir" {
					next.Walk = append(next.Walk, queueWalk{Source: walk.Source, ProviderPath: providerPath, Destination: dest, Total: -1, Deadline: walk.Deadline})
				}
			}
			if emb.Offset+len(emb.Items) < emb.Total {
				next.Walk = append([]queueWalk{{Source: walk.Source, ProviderPath: walk.ProviderPath, Destination: walk.Destination, Offset: emb.Offset + len(emb.Items), Total: emb.Total, Deadline: walk.Deadline}}, next.Walk...)
			}
			pending := false
			for _, w := range next.Walk {
				if w.Source == walk.Source {
					pending = true
				}
			}
			next.Discovered[walk.Source] = !pending
		}
	}
	added := queueRetainedItems(next) - queueRetainedItems(q.state)
	if owned+added > queueOwnerItems || total+added > queueProcessItems {
		err = errors.New("queue_capacity")
	}
	if err != nil {
		next = cloneQueueState(q.state)
		next.DiscoveryError = queueSourceError(err)
		if err.Error() == "destination_collision" || err.Error() == "queue_capacity" || err.Error() == "invalid_provider_path" || err.Error() == "provider_page_limit" {
			next.DiscoveryError = err.Error()
		}
		next.Discovered[walk.Source] = true
		filtered := next.Walk[:0]
		for _, w := range next.Walk {
			if w.Source != walk.Source {
				filtered = append(filtered, w)
			}
		}
		next.Walk = filtered
	}
	cc, finishCommit := queueContext()
	defer finishCommit()
	if err = s.commitQueue(cc, q, next); errors.Is(err, errQueueCapacity) {
		next = cloneQueueState(q.state)
		next.DiscoveryError = "queue_capacity"
		next.Discovered[walk.Source] = true
		filtered := next.Walk[:0]
		for _, w := range next.Walk {
			if w.Source != walk.Source {
				filtered = append(filtered, w)
			}
		}
		next.Walk = filtered
		if err := s.commitQueue(cc, q, next); errors.Is(err, errQueueCapacity) {
			q.frozen = true
			q.blockedCode = "queue_capacity"
		}
	}
}
