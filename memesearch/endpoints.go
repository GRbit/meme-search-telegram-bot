package memesearch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"
)

// Folder is one library folder with its number of memes.
type Folder struct {
	// Name is relative to the memes root and is what SearchOptions.Folders takes.
	Name      string
	MemeCount int
}

// Tag is one tag name carried by at least one meme.
type Tag struct {
	Name      string
	MemeCount int
}

// ListOptions select one page of ListMemes.
type ListOptions struct {
	// Cursor is a MemePage.NextCursor exactly as received; "" lists from the start.
	Cursor string
	// Limit is 1..MaxListLimit; 0 means the server default (50).
	Limit int
}

// MemePage is one page of the collection in change order.
type MemePage struct {
	Memes   []Meme
	HasMore bool
	// NextCursor is where the next page or a later poll starts. It is empty only
	// on an empty page, which means nothing is newer: keep the previous cursor.
	NextCursor string
}

// UploadFile is one image for Upload.
type UploadFile struct {
	Name string // file name with extension; the server keeps it unless it clashes
	Data []byte
}

// UploadOptions tune one Upload request.
type UploadOptions struct {
	// Folder is an existing library folder; "" means the server's direct-uploads.
	Folder string
	// GenerateDescription queues description generation for created memes.
	GenerateDescription bool
}

// UploadResult reports one outcome per uploaded file.
type UploadResult struct {
	Created    []UploadCreated
	Duplicates []UploadDuplicate
	Errors     []UploadFailure
	// Folder and GenerateDescription are what the server applied.
	Folder              string
	GenerateDescription bool
}

// UploadCreated is a stored file.
type UploadCreated struct {
	Filename          string // as sent; Meme.Filename is the stored name
	DescriptionQueued bool
	Meme              Meme
}

// UploadDuplicate is a file whose bytes already exist in the target folder.
type UploadDuplicate struct {
	Filename string
	// Meme is the existing meme, nil when that file is not indexed yet.
	Meme *Meme
}

// UploadFailure is a file the server refused.
type UploadFailure struct {
	Filename string
	// Code is file_too_large, unsupported_file_type, invalid_image or storage_failed.
	Code    string
	Message string
}

// Folders lists the library folders. An older server fails with ErrUnsupported.
func (c *HTTPClient) Folders(ctx context.Context) ([]Folder, error) {
	items, err := c.namedCounts(ctx, "/api/v1/folders")
	if err != nil {
		return nil, err
	}
	out := make([]Folder, len(items))
	for i, it := range items {
		out[i] = Folder{Name: *it.Name, MemeCount: *it.MemeCount}
	}
	return out, nil
}

// Tags lists the tags in use. An older server fails with ErrUnsupported.
func (c *HTTPClient) Tags(ctx context.Context) ([]Tag, error) {
	items, err := c.namedCounts(ctx, "/api/v1/tags")
	if err != nil {
		return nil, err
	}
	out := make([]Tag, len(items))
	for i, it := range items {
		out[i] = Tag{Name: *it.Name, MemeCount: *it.MemeCount}
	}
	return out, nil
}

type namedCount struct {
	Name      *string `json:"name"`
	MemeCount *int    `json:"meme_count"`
}

func (c *HTTPClient) namedCounts(ctx context.Context, path string) ([]namedCount, error) {
	start := time.Now()
	var body struct {
		Data *[]namedCount `json:"data"`
		Meta *struct {
			Count *int `json:"count"`
		} `json:"meta"`
	}
	status, err := c.getJSON(ctx, path, nil, &body)
	if err == nil {
		switch {
		case body.Data == nil || body.Meta == nil || body.Meta.Count == nil:
			err = fmt.Errorf("%w: missing data or meta", ErrBadResponse)
		default:
			for _, it := range *body.Data {
				if it.Name == nil || it.MemeCount == nil || *it.MemeCount < 0 {
					err = fmt.Errorf("%w: item without name or meme_count", ErrBadResponse)
					break
				}
			}
		}
	}
	err = missingRoute(err, false)
	if err != nil {
		c.log.DebugContext(ctx, "memesearch call failed", "method", http.MethodGet, "path", path,
			"status", status, "duration", time.Since(start), "err", err)
		return nil, err
	}
	c.log.DebugContext(ctx, "memesearch call", "method", http.MethodGet, "path", path,
		"status", status, "duration", time.Since(start), "count", len(*body.Data))
	return *body.Data, nil
}

// ListMemes returns one page of the collection ordered by last change. An
// older server fails with ErrUnsupported.
func (c *HTTPClient) ListMemes(ctx context.Context, opts ListOptions) (MemePage, error) {
	if opts.Limit < 0 || opts.Limit > MaxListLimit {
		return MemePage{}, fmt.Errorf("%w: limit %d outside 1..%d", ErrInvalidRequest, opts.Limit, MaxListLimit)
	}
	if utf8.RuneCountInString(opts.Cursor) > maxCursorLen {
		return MemePage{}, fmt.Errorf("%w: cursor longer than %d characters", ErrInvalidRequest, maxCursorLen)
	}
	params := url.Values{}
	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		params.Set("cursor", opts.Cursor)
	}
	const path = "/api/v1/memes"
	start := time.Now()
	var body struct {
		Data *[]wireMeme `json:"data"`
		Meta *struct {
			Limit      *int    `json:"limit"`
			Count      *int    `json:"count"`
			HasMore    *bool   `json:"has_more"`
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	status, err := c.getJSON(ctx, path, params, &body)
	if err == nil {
		switch {
		case body.Data == nil || body.Meta == nil || body.Meta.Limit == nil || body.Meta.Count == nil || body.Meta.HasMore == nil:
			err = fmt.Errorf("%w: missing data or meta", ErrBadResponse)
		default:
			err = validateMemes(*body.Data)
		}
	}
	err = missingRoute(err, false)
	if err != nil {
		c.log.DebugContext(ctx, "memesearch call failed", "method", http.MethodGet, "path", path,
			"limit", opts.Limit, "has_cursor", opts.Cursor != "", "status", status, "duration", time.Since(start), "err", err)
		return MemePage{}, err
	}
	page := MemePage{Memes: memesOf(*body.Data), HasMore: *body.Meta.HasMore}
	if body.Meta.NextCursor != nil {
		page.NextCursor = *body.Meta.NextCursor
	}
	c.log.DebugContext(ctx, "memesearch call", "method", http.MethodGet, "path", path,
		"limit", opts.Limit, "has_cursor", opts.Cursor != "", "status", status, "duration", time.Since(start),
		"count", len(page.Memes), "has_more", page.HasMore)
	return page, nil
}

// Similar returns up to limit memes whose descriptions are closest to meme
// id's, without the meme itself; empty when it has no embedding yet. An
// unknown id fails with ErrNotFound, an older server with ErrUnsupported.
func (c *HTTPClient) Similar(ctx context.Context, id int64, limit int) ([]Meme, error) {
	if id < 1 {
		return nil, fmt.Errorf("%w: meme id %d", ErrInvalidRequest, id)
	}
	if limit < 1 || limit > MaxLimit {
		return nil, fmt.Errorf("%w: limit %d outside 1..%d", ErrInvalidRequest, limit, MaxLimit)
	}
	path := "/api/v1/memes/" + strconv.FormatInt(id, 10) + "/similar"
	start := time.Now()
	var body struct {
		Data *[]wireMeme `json:"data"`
		Meta *struct {
			MemeID *int64 `json:"meme_id"`
			Limit  *int   `json:"limit"`
			Count  *int   `json:"count"`
		} `json:"meta"`
	}
	status, err := c.getJSON(ctx, path, url.Values{"limit": {strconv.Itoa(limit)}}, &body)
	if err == nil {
		switch {
		case body.Data == nil || body.Meta == nil || body.Meta.MemeID == nil || body.Meta.Limit == nil || body.Meta.Count == nil:
			err = fmt.Errorf("%w: missing data or meta", ErrBadResponse)
		default:
			err = validateMemes(*body.Data)
		}
	}
	err = missingRoute(err, true)
	c.observeEndpoint(ctx, "similar", func(p *Capabilities) *bool { return &p.Similar }, similarEvidence(err))
	if err != nil {
		c.log.DebugContext(ctx, "memesearch call failed", "method", http.MethodGet, "path", path,
			"limit", limit, "status", status, "duration", time.Since(start), "err", err)
		return nil, err
	}
	c.log.DebugContext(ctx, "memesearch call", "method", http.MethodGet, "path", path,
		"limit", limit, "status", status, "duration", time.Since(start), "count", len(*body.Data))
	return memesOf(*body.Data), nil
}

// similarEvidence maps an unknown meme to success: the route answered.
func similarEvidence(err error) error {
	if errors.Is(err, ErrNotFound) && !errors.Is(err, ErrUnsupported) {
		return nil
	}
	return err
}

// Upload stores images through API v2 with the upload token. Without an
// upload token, or against an older server, it fails with ErrUnsupported.
func (c *HTTPClient) Upload(ctx context.Context, files []UploadFile, opts UploadOptions) (UploadResult, error) {
	if c.uploadToken == "" {
		return UploadResult{}, fmt.Errorf("%w: no upload token configured", ErrUnsupported)
	}
	if len(files) < 1 || len(files) > MaxUploadFiles {
		return UploadResult{}, fmt.Errorf("%w: %d files outside 1..%d", ErrInvalidRequest, len(files), MaxUploadFiles)
	}
	for i, f := range files {
		if f.Name == "" || len(f.Data) == 0 {
			return UploadResult{}, fmt.Errorf("%w: file %d has no name or no data", ErrInvalidRequest, i)
		}
	}
	res, err := c.upload(ctx, files, opts)
	c.observeEndpoint(ctx, "upload", func(p *Capabilities) *bool { return &p.Upload }, err)
	return res, err
}

// upload sends files without local checks; Probe uses it with none.
func (c *HTTPClient) upload(ctx context.Context, files []UploadFile, opts UploadOptions) (UploadResult, error) {
	const path = "/api/v2/memes"
	start := time.Now()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	size := 0
	for _, f := range files {
		w, err := mw.CreateFormFile("files[]", f.Name)
		if err != nil {
			return UploadResult{}, fmt.Errorf("memesearch: build upload: %w", err)
		}
		_, _ = w.Write(f.Data) // a bytes.Buffer does not fail
		size += len(f.Data)
	}
	if opts.Folder != "" {
		_ = mw.WriteField("folder", opts.Folder)
	}
	if opts.GenerateDescription {
		_ = mw.WriteField("generate_description", "true")
	}
	if err := mw.Close(); err != nil {
		return UploadResult{}, fmt.Errorf("memesearch: build upload: %w", err)
	}

	var body uploadResponse
	status := 0
	resp, err := c.do(ctx, http.MethodPost, path, nil, &buf, mw.FormDataContentType(), "application/json", c.uploadToken)
	if err == nil {
		status, err = decodeJSON(resp, path, &body)
	}
	if err == nil {
		err = body.validate()
	}
	err = missingRoute(err, false)
	if err != nil {
		c.log.DebugContext(ctx, "memesearch call failed", "method", http.MethodPost, "path", path,
			"files", len(files), "bytes", size, "folder", opts.Folder, "status", status,
			"duration", time.Since(start), "err", err)
		return UploadResult{}, err
	}
	res := body.result()
	c.log.DebugContext(ctx, "memesearch call", "method", http.MethodPost, "path", path,
		"files", len(files), "bytes", size, "folder", res.Folder, "status", status, "duration", time.Since(start),
		"created", len(res.Created), "duplicates", len(res.Duplicates), "errors", len(res.Errors))
	return res, nil
}

type uploadResponse struct {
	Data *struct {
		Created *[]struct {
			Filename          *string   `json:"filename"`
			DescriptionQueued *bool     `json:"description_queued"`
			Meme              *wireMeme `json:"meme"`
		} `json:"created"`
		Duplicates *[]struct {
			Filename *string   `json:"filename"`
			Meme     *wireMeme `json:"meme"`
		} `json:"duplicates"`
		Errors *[]struct {
			Filename *string `json:"filename"`
			Error    *struct {
				Code    *string `json:"code"`
				Message *string `json:"message"`
			} `json:"error"`
		} `json:"errors"`
	} `json:"data"`
	Meta *struct {
		Folder              *string `json:"folder"`
		GenerateDescription *bool   `json:"generate_description"`
	} `json:"meta"`
}

func (r *uploadResponse) validate() error {
	d := r.Data
	if d == nil || d.Created == nil || d.Duplicates == nil || d.Errors == nil ||
		r.Meta == nil || r.Meta.Folder == nil || r.Meta.GenerateDescription == nil {
		return fmt.Errorf("%w: missing upload data or meta", ErrBadResponse)
	}
	for _, c := range *d.Created {
		if c.Filename == nil || c.DescriptionQueued == nil || c.Meme == nil {
			return fmt.Errorf("%w: created entry", ErrBadResponse)
		}
		if err := c.Meme.validate(); err != nil {
			return err
		}
	}
	for _, dup := range *d.Duplicates {
		if dup.Filename == nil {
			return fmt.Errorf("%w: duplicate entry", ErrBadResponse)
		}
		if dup.Meme != nil {
			if err := dup.Meme.validate(); err != nil {
				return err
			}
		}
	}
	for _, e := range *d.Errors {
		if e.Filename == nil || e.Error == nil || e.Error.Code == nil || e.Error.Message == nil {
			return fmt.Errorf("%w: error entry", ErrBadResponse)
		}
	}
	return nil
}

func (r *uploadResponse) result() UploadResult {
	res := UploadResult{Folder: *r.Meta.Folder, GenerateDescription: *r.Meta.GenerateDescription}
	for _, c := range *r.Data.Created {
		res.Created = append(res.Created, UploadCreated{Filename: *c.Filename, DescriptionQueued: *c.DescriptionQueued, Meme: c.Meme.meme()})
	}
	for _, d := range *r.Data.Duplicates {
		dup := UploadDuplicate{Filename: *d.Filename}
		if d.Meme != nil {
			m := d.Meme.meme()
			dup.Meme = &m
		}
		res.Duplicates = append(res.Duplicates, dup)
	}
	for _, e := range *r.Data.Errors {
		res.Errors = append(res.Errors, UploadFailure{Filename: *e.Filename, Code: *e.Error.Code, Message: *e.Error.Message})
	}
	return res
}

// missingRoute marks a 404 as ErrUnsupported. With memeScoped, the route also
// 404s for an unknown meme, and only the API's own not_found envelope tells
// the two apart: an older server's routing error is HTML or Rails' generic JSON.
func missingRoute(err error, memeScoped bool) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		return err
	}
	if memeScoped && apiErr.Code == "not_found" {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnsupported, err)
}

func validateMemes(ws []wireMeme) error {
	for i := range ws {
		if err := ws[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

func memesOf(ws []wireMeme) []Meme {
	out := make([]Meme, len(ws))
	for i := range ws {
		out[i] = ws[i].meme()
	}
	return out
}
