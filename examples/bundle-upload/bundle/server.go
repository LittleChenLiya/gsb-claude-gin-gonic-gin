package bundle

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// NewRouter builds the gin engine exposing the upload and download API.
func NewRouter(s *Store) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	h := &handler{store: s}
	r.POST("/bundles", h.upload)
	r.GET("/bundles/:id", h.manifest)
	r.GET("/bundles/:id/files/:key", h.download)
	return r
}

type handler struct {
	store *Store
}

// upload streams a multipart form into a staging session. The bundle is
// published only after every part has been fully read and validated;
// any failure (cancel, read error, limit, publish error) aborts the
// session and removes only this session's staging directory.
func (h *handler) upload(c *gin.Context) {
	mr, err := c.Request.MultipartReader()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	session, err := h.store.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Abort is a no-op once Commit succeeded.
	defer func() { _ = session.Abort() }()

	for {
		if err := c.Request.Context().Err(); err != nil {
			c.JSON(499, gin.H{"error": "request canceled"})
			return
		}
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if part.FileName() == "" { // plain form field, not a file
			_ = part.Close()
			continue
		}
		_, addErr := session.AddFile(part.FileName(), part)
		_ = part.Close()
		if addErr != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(addErr, ErrTooManyFiles),
				errors.Is(addErr, ErrFileTooLarge),
				errors.Is(addErr, ErrTotalTooLarge):
				status = http.StatusRequestEntityTooLarge
			}
			c.JSON(status, gin.H{"error": addErr.Error()})
			return
		}
	}

	m, err := session.Commit()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, m)
}

func (h *handler) manifest(c *gin.Context) {
	m, err := h.store.LoadManifest(c.Param("id"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

// download serves a published file by bundle ID and file key. Both are
// validated against the manifest, so no client input can resolve to an
// arbitrary disk path.
func (h *handler) download(c *gin.Context) {
	f, entry, err := h.store.Open(c.Param("id"), c.Param("key"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	defer func() { _ = f.Close() }()

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", entry.Name))
	c.Header("X-Content-SHA256", entry.SHA256)
	c.DataFromReader(http.StatusOK, entry.Size, "application/octet-stream", f, nil)
}

func writeStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrInvalidID):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
