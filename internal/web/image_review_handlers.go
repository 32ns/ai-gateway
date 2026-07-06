package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	imageReviewPageSize     = 24
	imageReviewMaxNoteRunes = 1000

	imageReviewStatusPending  = "pending"
	imageReviewStatusReviewed = "reviewed"
	imageReviewStatusFlagged  = "flagged"
	imageReviewStatusResolved = "resolved"
)

type imageReviewItem struct {
	ID            string `json:"id"`
	JobID         string `json:"jobId"`
	ResultIndex   int    `json:"resultIndex"`
	UserID        string `json:"userId"`
	ClientID      string `json:"clientId,omitempty"`
	Prompt        string `json:"prompt"`
	RevisedPrompt string `json:"revisedPrompt,omitempty"`
	Model         string `json:"model,omitempty"`
	Ratio         string `json:"ratio,omitempty"`
	Resolution    string `json:"resolution,omitempty"`
	Size          string `json:"size,omitempty"`
	APISize       string `json:"apiSize,omitempty"`
	MIME          string `json:"mime,omitempty"`
	AssetPath     string `json:"assetPath,omitempty"`
	FileSize      int64  `json:"fileSize,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt,omitempty"`
	Status        string `json:"status"`
	Note          string `json:"note,omitempty"`
	ReviewedAt    int64  `json:"reviewedAt,omitempty"`
	ReviewedBy    string `json:"reviewedBy,omitempty"`
}

type imageReviewStore struct {
	mu    sync.Mutex
	wg    sync.WaitGroup
	items map[string]imageReviewItem
}

type imageReviewFilter struct {
	Status   string
	UserID   string
	Query    string
	Page     int
	PageSize int
}

type imageReviewStats struct {
	Total    int
	Pending  int
	Reviewed int
	Flagged  int
	Resolved int
}

type imageReviewPage struct {
	Items      []imageReviewItem
	Filter     imageReviewFilter
	Stats      imageReviewStats
	Total      int
	Page       int
	PageSize   int
	HasPrev    bool
	PrevPage   int
	HasNext    bool
	NextPage   int
	ReturnTo   string
	StatusList []string
}

func newImageReviewStore() *imageReviewStore {
	return &imageReviewStore{
		items: make(map[string]imageReviewItem),
	}
}

func (s *Server) loadImageReviewIndex() error {
	if s == nil || s.imageReviews == nil {
		return nil
	}
	return s.imageReviews.load(s.imageReviewIndexPath())
}

func (st *imageReviewStore) load(indexPath string) error {
	if st == nil {
		return nil
	}
	f, err := os.Open(strings.TrimSpace(indexPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	items := make(map[string]imageReviewItem)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item imageReviewItem
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		item = normalizeImageReviewItem(item)
		if item.ID == "" || item.JobID == "" || item.AssetPath == "" {
			continue
		}
		items[item.ID] = item
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	st.mu.Lock()
	st.items = items
	st.mu.Unlock()
	return nil
}

func (st *imageReviewStore) add(rootDir, indexPath string, snapshot imageLabTaskSnapshot, result imageLabResultEvent, data []byte) error {
	if st == nil || !result.OK || len(data) == 0 {
		return nil
	}
	jobID := sanitizeImageLabFileID(snapshot.ID)
	if jobID == "" {
		return nil
	}
	id := imageReviewItemID(jobID, result.Index)
	if id == "" {
		return nil
	}

	st.mu.Lock()
	if _, exists := st.items[id]; exists {
		st.mu.Unlock()
		return nil
	}
	st.mu.Unlock()

	mimeType := normalizeImageLabOutputMIME(result.MIME, data)
	if mimeType == "" {
		return nil
	}
	filename := imageLabResultFilename(result.Index, mimeType)
	assetRel := filepath.ToSlash(filepath.Join("assets", jobID, filename))
	assetPath := filepath.Join(rootDir, filepath.FromSlash(assetRel))
	if err := os.MkdirAll(filepath.Dir(assetPath), 0700); err != nil {
		return err
	}
	tempPath := assetPath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tempPath, assetPath); err != nil {
		_ = os.Remove(tempPath)
		return err
	}

	now := time.Now().UnixMilli()
	item := normalizeImageReviewItem(imageReviewItem{
		ID:            id,
		JobID:         strings.TrimSpace(snapshot.ID),
		ResultIndex:   result.Index,
		UserID:        strings.TrimSpace(snapshot.UserID),
		ClientID:      strings.TrimSpace(snapshot.ClientID),
		Prompt:        strings.TrimSpace(snapshot.Prompt),
		RevisedPrompt: strings.TrimSpace(result.Text),
		Model:         strings.TrimSpace(snapshot.Model),
		Ratio:         strings.TrimSpace(snapshot.Ratio),
		Resolution:    strings.TrimSpace(snapshot.Resolution),
		Size:          strings.TrimSpace(snapshot.Size),
		APISize:       strings.TrimSpace(snapshot.APISize),
		MIME:          mimeType,
		AssetPath:     assetRel,
		FileSize:      int64(len(data)),
		CreatedAt:     now,
		UpdatedAt:     now,
		Status:        imageReviewStatusPending,
	})

	st.mu.Lock()
	defer st.mu.Unlock()
	if _, exists := st.items[item.ID]; exists {
		return nil
	}
	if err := appendImageReviewIndexLine(indexPath, item); err != nil {
		return err
	}
	st.items[item.ID] = item
	return nil
}

func (st *imageReviewStore) get(id string) (imageReviewItem, bool) {
	if st == nil {
		return imageReviewItem{}, false
	}
	id = strings.TrimSpace(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	item, ok := st.items[id]
	return item, ok
}

func (st *imageReviewStore) updateStatus(indexPath, id, status, note, reviewedBy string) (imageReviewItem, bool, error) {
	if st == nil {
		return imageReviewItem{}, false, nil
	}
	status, ok := normalizeImageReviewStatus(status)
	if !ok || status == "" {
		return imageReviewItem{}, false, fmt.Errorf("invalid image review status")
	}
	note = truncateRunes(strings.TrimSpace(note), imageReviewMaxNoteRunes)
	reviewedBy = strings.TrimSpace(reviewedBy)
	now := time.Now().UnixMilli()

	st.mu.Lock()
	defer st.mu.Unlock()
	item, exists := st.items[strings.TrimSpace(id)]
	if !exists {
		return imageReviewItem{}, false, nil
	}
	previous := item
	item.Status = status
	item.Note = note
	item.ReviewedAt = now
	item.ReviewedBy = reviewedBy
	item.UpdatedAt = now
	st.items[item.ID] = item
	if err := st.saveIndexLocked(indexPath); err != nil {
		st.items[previous.ID] = previous
		return previous, true, err
	}
	return item, true, nil
}

func (st *imageReviewStore) page(filter imageReviewFilter) imageReviewPage {
	if st == nil {
		return imageReviewPage{Filter: filter, StatusList: imageReviewStatuses()}
	}
	filter.Status, _ = normalizeImageReviewStatus(filter.Status)
	filter.UserID = strings.TrimSpace(filter.UserID)
	filter.Query = strings.ToLower(strings.TrimSpace(filter.Query))
	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = imageReviewPageSize
	}

	st.mu.Lock()
	items := make([]imageReviewItem, 0, len(st.items))
	for _, item := range st.items {
		items = append(items, item)
	}
	st.mu.Unlock()

	stats := imageReviewStats{Total: len(items)}
	for _, item := range items {
		switch item.Status {
		case imageReviewStatusReviewed:
			stats.Reviewed++
		case imageReviewStatusFlagged:
			stats.Flagged++
		case imageReviewStatusResolved:
			stats.Resolved++
		default:
			stats.Pending++
		}
	}

	filtered := make([]imageReviewItem, 0, len(items))
	for _, item := range items {
		if filter.Status != "" && item.Status != filter.Status {
			continue
		}
		if filter.UserID != "" && !strings.EqualFold(strings.TrimSpace(item.UserID), filter.UserID) {
			continue
		}
		if filter.Query != "" && !imageReviewItemMatchesQuery(item, filter.Query) {
			continue
		}
		filtered = append(filtered, item)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].CreatedAt == filtered[j].CreatedAt {
			return filtered[i].ID > filtered[j].ID
		}
		return filtered[i].CreatedAt > filtered[j].CreatedAt
	})

	total := len(filtered)
	start := (filter.Page - 1) * filter.PageSize
	if start > total {
		start = total
	}
	end := start + filter.PageSize
	if end > total {
		end = total
	}
	pageItems := filtered[start:end]
	return imageReviewPage{
		Items:      pageItems,
		Filter:     filter,
		Stats:      stats,
		Total:      total,
		Page:       filter.Page,
		PageSize:   filter.PageSize,
		HasPrev:    filter.Page > 1,
		PrevPage:   filter.Page - 1,
		HasNext:    end < total,
		NextPage:   filter.Page + 1,
		StatusList: imageReviewStatuses(),
	}
}

func (st *imageReviewStore) saveIndexLocked(indexPath string) error {
	items := make([]imageReviewItem, 0, len(st.items))
	for _, item := range st.items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt == items[j].CreatedAt {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt < items[j].CreatedAt
	})

	if err := os.MkdirAll(filepath.Dir(indexPath), 0700); err != nil {
		return err
	}
	tempPath := indexPath + ".tmp"
	f, err := os.OpenFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, item := range items {
		if err := enc.Encode(item); err != nil {
			_ = f.Close()
			_ = os.Remove(tempPath)
			return err
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, indexPath); err != nil {
		if removeErr := os.Remove(indexPath); removeErr != nil && !os.IsNotExist(removeErr) {
			_ = os.Remove(tempPath)
			return err
		}
		if renameErr := os.Rename(tempPath, indexPath); renameErr != nil {
			_ = os.Remove(tempPath)
			return renameErr
		}
	}
	return nil
}

func appendImageReviewIndexLine(indexPath string, item imageReviewItem) error {
	if err := os.MkdirAll(filepath.Dir(indexPath), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(indexPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(item)
}

func (s *Server) recordImageReviewItem(snapshot imageLabTaskSnapshot, result imageLabResultEvent, data []byte) {
	if s == nil || s.imageReviews == nil || !result.OK || len(data) == 0 {
		return
	}
	reviews := s.imageReviews
	rootDir := s.imageReviewRootDir()
	indexPath := s.imageReviewIndexPath()
	reviews.wg.Add(1)
	go func() {
		defer reviews.wg.Done()
		_ = reviews.add(rootDir, indexPath, snapshot, result, data)
	}()
}

func (s *Server) waitImageReviewWrites() {
	if s == nil || s.imageReviews == nil {
		return
	}
	s.imageReviews.wg.Wait()
}

func (s *Server) handleImageReviewsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/image-reviews" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	page := imageReviewPage{}
	if s.imageReviews != nil {
		page = s.imageReviews.page(imageReviewFilter{
			Status:   strings.TrimSpace(r.URL.Query().Get("status")),
			UserID:   strings.TrimSpace(r.URL.Query().Get("user_id")),
			Query:    strings.TrimSpace(r.URL.Query().Get("q")),
			Page:     parsePositiveInt(r.URL.Query().Get("page"), 1),
			PageSize: imageReviewPageSize,
		})
	}
	page.ReturnTo = imageReviewReturnPath(r)
	if len(page.StatusList) == 0 {
		page.StatusList = imageReviewStatuses()
	}

	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":     "page_title_image_reviews",
		"ActiveNav":    "image-reviews",
		"Locale":       locale,
		"ImageReviews": page,
	}, r)
	s.render(w, "image_reviews.html", locale, data)
}

func (s *Server) handleImageReviewActions(w http.ResponseWriter, r *http.Request) {
	actionPath := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/image-reviews/"), "/")
	parts := strings.Split(actionPath, "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(id) == "" {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "asset":
		s.handleImageReviewAsset(w, r, id)
	case "status":
		s.handleImageReviewStatus(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleImageReviewAsset(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.imageReviews == nil {
		http.NotFound(w, r)
		return
	}
	item, ok := s.imageReviews.get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	path, ok := s.imageReviewAssetFilePath(item)
	if !ok {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	mimeType := strings.TrimSpace(item.MIME)
	if mimeType == "" {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), file)
}

func (s *Server) handleImageReviewStatus(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.imageReviews == nil {
		http.NotFound(w, r)
		return
	}
	status := strings.TrimSpace(r.FormValue("status"))
	normalizedStatus, ok := normalizeImageReviewStatus(status)
	if !ok || normalizedStatus == "" {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}
	user, _ := currentUserFromContext(r.Context())
	item, found, err := s.imageReviews.updateStatus(s.imageReviewIndexPath(), id, normalizedStatus, r.FormValue("note"), user.Username)
	if err != nil {
		http.Error(w, "save image review status failed", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	s.recordAdminAudit(r, "ok", "image_review.update", "image_review", item.ID, item.JobID, "status="+item.Status)
	redirectLocalSeeOther(w, r, imageReviewPostReturnPath(r))
}

func (s *Server) imageReviewRootDir() string {
	base := strings.TrimSpace(s.statePath)
	if base == "" {
		base = filepath.Join("data", "state.db")
	}
	dir := filepath.Dir(base)
	if dir == "." || dir == "" {
		dir = "data"
	}
	return filepath.Join(dir, "image-review")
}

func (s *Server) imageReviewIndexPath() string {
	return filepath.Join(s.imageReviewRootDir(), "index.jsonl")
}

func (s *Server) imageReviewAssetFilePath(item imageReviewItem) (string, bool) {
	relPath := strings.TrimSpace(item.AssetPath)
	if relPath == "" {
		return "", false
	}
	relPath = filepath.Clean(filepath.FromSlash(relPath))
	if filepath.IsAbs(relPath) || relPath == "." || strings.HasPrefix(relPath, "..") {
		return "", false
	}
	relSlash := filepath.ToSlash(relPath)
	if relSlash != "assets" && !strings.HasPrefix(relSlash, "assets/") {
		return "", false
	}
	root, err := filepath.Abs(s.imageReviewRootDir())
	if err != nil {
		return "", false
	}
	cleanPath, err := filepath.Abs(filepath.Clean(filepath.Join(root, relPath)))
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, cleanPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", false
	}
	return cleanPath, true
}

func normalizeImageReviewItem(item imageReviewItem) imageReviewItem {
	item.ID = strings.TrimSpace(item.ID)
	item.JobID = strings.TrimSpace(item.JobID)
	item.UserID = strings.TrimSpace(item.UserID)
	item.ClientID = strings.TrimSpace(item.ClientID)
	item.Prompt = strings.TrimSpace(item.Prompt)
	item.RevisedPrompt = strings.TrimSpace(item.RevisedPrompt)
	item.Model = strings.TrimSpace(item.Model)
	item.Ratio = strings.TrimSpace(item.Ratio)
	item.Resolution = strings.TrimSpace(item.Resolution)
	item.Size = strings.TrimSpace(item.Size)
	item.APISize = strings.TrimSpace(item.APISize)
	item.MIME = strings.TrimSpace(item.MIME)
	item.AssetPath = filepath.ToSlash(strings.TrimSpace(item.AssetPath))
	item.Note = truncateRunes(strings.TrimSpace(item.Note), imageReviewMaxNoteRunes)
	item.ReviewedBy = strings.TrimSpace(item.ReviewedBy)
	if status, ok := normalizeImageReviewStatus(item.Status); ok {
		item.Status = status
	} else {
		item.Status = imageReviewStatusPending
	}
	if item.Status == "" {
		item.Status = imageReviewStatusPending
	}
	if item.CreatedAt <= 0 {
		item.CreatedAt = time.Now().UnixMilli()
	}
	if item.UpdatedAt <= 0 {
		item.UpdatedAt = item.CreatedAt
	}
	return item
}

func normalizeImageReviewStatus(status string) (string, bool) {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return "", true
	}
	switch status {
	case imageReviewStatusPending, imageReviewStatusReviewed, imageReviewStatusFlagged, imageReviewStatusResolved:
		return status, true
	default:
		return "", false
	}
}

func imageReviewStatuses() []string {
	return []string{imageReviewStatusPending, imageReviewStatusReviewed, imageReviewStatusFlagged, imageReviewStatusResolved}
}

func imageReviewItemID(jobID string, index int) string {
	jobID = sanitizeImageLabFileID(jobID)
	if jobID == "" {
		return ""
	}
	if index < 0 {
		index = 0
	}
	return jobID + "-" + fmt.Sprintf("%03d", index)
}

func imageReviewItemMatchesQuery(item imageReviewItem, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	values := []string{
		item.ID,
		item.JobID,
		item.UserID,
		item.ClientID,
		item.Prompt,
		item.RevisedPrompt,
		item.Model,
		item.Ratio,
		item.Resolution,
		item.Size,
		item.APISize,
		item.MIME,
		item.Note,
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func imageReviewReturnPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "/admin/image-reviews"
	}
	target := sanitizeLocalRedirectTarget(r.URL.RequestURI())
	if !strings.HasPrefix(target, "/admin/image-reviews") {
		return "/admin/image-reviews"
	}
	return target
}

func imageReviewPostReturnPath(r *http.Request) string {
	target := strings.TrimSpace(r.FormValue("return_to"))
	if target == "" {
		return "/admin/image-reviews"
	}
	target = sanitizeLocalRedirectTarget(target)
	if !strings.HasPrefix(target, "/admin/image-reviews") {
		return "/admin/image-reviews"
	}
	return target
}

func imageReviewPageURL(filter imageReviewFilter, page int) string {
	values := url.Values{}
	if status, ok := normalizeImageReviewStatus(filter.Status); ok && status != "" {
		values.Set("status", status)
	}
	if userID := strings.TrimSpace(filter.UserID); userID != "" {
		values.Set("user_id", userID)
	}
	if query := strings.TrimSpace(filter.Query); query != "" {
		values.Set("q", query)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if encoded := values.Encode(); encoded != "" {
		return "/admin/image-reviews?" + encoded
	}
	return "/admin/image-reviews"
}

func imageReviewStatusText(locale, status string) string {
	status, ok := normalizeImageReviewStatus(status)
	if !ok || status == "" {
		status = imageReviewStatusPending
	}
	return translate(locale, "image_review_status_"+status)
}

func imageReviewStatusClass(status string) string {
	switch strings.TrimSpace(status) {
	case imageReviewStatusReviewed:
		return "tone-good"
	case imageReviewStatusFlagged:
		return "tone-bad"
	case imageReviewStatusResolved:
		return "tone-muted"
	default:
		return "tone-warn"
	}
}

func imageReviewTime(unixMilli int64) string {
	if unixMilli <= 0 {
		return "-"
	}
	return time.UnixMilli(unixMilli).Local().Format("2006-01-02 15:04:05")
}

func imageReviewFileSize(size int64) string {
	if size <= 0 {
		return "-"
	}
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}
