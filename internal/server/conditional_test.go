package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestConditionalJSONCompleteRepresentation(t *testing.T) {
	resources := 1
	r := gin.New()
	r.GET("/snapshot", func(c *gin.Context) {
		writeJSONWithETag(c, gin.H{"models": []string{"weights"}, "resources": resources})
	})
	read := func(header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/snapshot", nil)
		if header != "" {
			req.Header.Set("If-None-Match", header)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	first := read("")
	require.Equal(t, 200, first.Code)
	etag := first.Header().Get("ETag")
	require.NotEmpty(t, etag)
	for _, header := range []string{etag, "W/" + etag, `"different", W/` + etag, "*"} {
		rec := read(header)
		require.Equal(t, 304, rec.Code, header)
		require.Empty(t, rec.Body.String())
		require.Equal(t, etag, rec.Header().Get("ETag"))
	}
	for _, header := range []string{`"different"`, `W/"different"`, etag + "invalid", `"unterminated`} {
		require.Equal(t, 200, read(header).Code, header)
	}
	resources++
	changed := read(etag)
	require.Equal(t, 200, changed.Code, "unchanged models cannot conceal changed resource metrics")
	require.NotEqual(t, etag, changed.Header().Get("ETag"))
}
