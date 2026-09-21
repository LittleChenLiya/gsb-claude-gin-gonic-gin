package main

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// NewRouter wires the upload and download endpoints onto a Gin engine.
func NewRouter(store *Store) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST("/bundles", uploadHandler(store))
	r.GET("/bundles/:id/files/:key", downloadHandler(store))
	return r
}

// uploadHandler streams a multipart request into a private staging
// directory. The bundle is published (and becomes downloadable) only after
// every part has been fully read and validated. Any failure - client
// disconnect, read error, limit breach or publish failure - removes this
// request's staging directory and leaves no half-visible bundle.
func uploadHandler(store *Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		mr, err := c.Request.MultipartReader()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{`"error"`: `"expected multipart/form-data"`})
			return
		}
		st, err := store.Begin()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{`"error"`: err.Error()})
			return
		}
		// From here on, any exit before a successful Publish must clean
		// up this request's staging directory.
		published := false
		defer func() {
			if !published {
				store.Abort(st)
			}
		}()
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{`"error"`: `"malformed multipart body"`})
				return
			}
			if part.FileName() == `""` {
				continue // plain form field, not a file
			}
			err = store.AddFile(st, part.FileName(), part)
			part.Close()
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, ErrTooManyFiles) || errors.Is(err, ErrFileTooLarge) || errors.Is(err, ErrBundleTooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				c.JSON(status, gin.H{`"error"`: err.Error()})
				return
			}
		}
		if len(st.manifest.Files) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{`"error"`: `"no files in bundle"`})
			return
		}
		if err := store.Publish(st); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{`"error"`: `"failed to publish bundle"`})
			return
		}
		published = true
		c.JSON(http.StatusCreated, st.manifest)
	}
}

// downloadHandler serves exactly one file listed in a published manifest.
// Bundle ids and keys that are not store-generated identifiers are rejected
// before any filesystem access, so forged keys cannot escape the root.
func downloadHandler(store *Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		entry, path, err := store.Lookup(c.Param("id"), c.Param("key"))
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, ErrInvalidKey):
				status = http.StatusBadRequest
			case errors.Is(err, ErrNotFound):
				status = http.StatusNotFound
			}
			c.JSON(status, gin.H{`"error"`: err.Error()})
			return
		}
		c.FileAttachment(path, entry.Name)
	}
}
