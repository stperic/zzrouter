package server

import (
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// auditValueMax bounds every audit query value; the store's cursors are shorter.
const auditValueMax = 256

var auditParams = []string{"limit", "cursor", "actor", "subject", "action", "since", "until"}

// auditPage is one page of the access history, newest first.
type auditPage struct {
	Events []audit.Event `json:"events"`
	Next   string        `json:"next,omitempty"`
}

func (s *Server) handleAccessAudit(c *gin.Context) {
	filter, cursor, limit, errs := auditQuery(c.Request.URL.RawQuery)
	if len(errs) > 0 {
		RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", errs)
		return
	}
	events, next, err := s.accessStore.AuditPage(c.Request.Context(), filter, cursor, limit)
	if errors.Is(err, audit.ErrCursor) {
		Conflict(c, err.Error())
		return
	}
	if err != nil {
		if !respondKnownFailure(c, err) {
			ServiceUnavailable(c, "Could not read audit history")
		}
		return
	}
	respondRetrievalSuccess(c, "Audit history", auditPage{Events: events, Next: next})
}

// auditQuery reads the page parameters, with one entry per refused key.
func auditQuery(raw string) (f audit.Filter, cursor string, limit int, errs []utils.ParamError) {
	limit = 50
	refuse := func(code httperr.ParamErrorCode, e utils.ParamError) {
		e.Code = string(code)
		errs = append(errs, e)
	}
	query, err := url.ParseQuery(raw)
	if err != nil {
		refuse(httperr.CodeInvalidValue, utils.ParamError{Want: "a URL-encoded query", Message: "query string: " + err.Error()})
	}
	for _, key := range slices.Sorted(maps.Keys(query)) {
		values := query[key]
		value := values[0]
		switch {
		case !slices.Contains(auditParams, key):
			refuse(httperr.CodeUnknownField, utils.ParamError{Key: key, Want: "one of: " + strings.Join(auditParams, ", "), Message: "unknown parameter " + strconv.Quote(key)})
		case len(values) > 1:
			refuse(httperr.CodeInvalidValue, utils.ParamError{Key: key, Want: "one value", Message: key + " is repeated"})
		case value == "":
			refuse(httperr.CodeInvalidValue, utils.ParamError{Key: key, Want: "a nonempty value", Message: key + " is empty"})
		case len(value) > auditValueMax:
			refuse(httperr.CodeOutOfRange, utils.ParamError{Key: key, Max: auditValueMax, Message: key + " is longer than " + strconv.Itoa(auditValueMax) + " bytes"})
		case key == "limit":
			n, err := strconv.Atoi(value)
			switch {
			case err != nil:
				refuse(httperr.CodeWrongType, utils.ParamError{Key: key, Want: "an integer", Message: "limit is not an integer"})
			case n < 1 || n > audit.MaxPage:
				refuse(httperr.CodeOutOfRange, utils.ParamError{Key: key, Min: 1, Max: audit.MaxPage, Message: "limit must be between 1 and " + strconv.Itoa(audit.MaxPage)})
			default:
				limit = n
			}
		case key == "cursor":
			cursor = value
		case key == "actor":
			f.Actor = value
		case key == "subject":
			f.Subject = value
		case key == "action":
			if f.Action = audit.Action(value); !slices.Contains(audit.Actions(), f.Action) {
				names := make([]string, 0, len(audit.Actions()))
				for _, action := range audit.Actions() {
					names = append(names, string(action))
				}
				refuse(httperr.CodeInvalidValue, utils.ParamError{Key: key, Want: "one of: " + strings.Join(names, ", "), Message: "unknown action " + strconv.Quote(value)})
			}
		default: // since, until
			at, err := time.Parse(time.RFC3339, value)
			switch {
			case err != nil:
				e := utils.ParamError{Key: key, Want: "an RFC 3339 date-time", Message: key + " is not an RFC 3339 date-time"}
				if strings.Contains(value, " ") {
					e.Hint = "a + in a query string decodes as a space: encode the offset's + as %2B"
				}
				refuse(httperr.CodeInvalidValue, e)
			case key == "since":
				f.Since = &at
			default:
				f.Until = &at
			}
		}
	}
	if f.Since != nil && f.Until != nil && f.Since.After(*f.Until) {
		refuse(httperr.CodeInvalidValue, utils.ParamError{Key: "since", Want: "at or before until", Message: "since is after until"})
	}
	return f, cursor, limit, errs
}
