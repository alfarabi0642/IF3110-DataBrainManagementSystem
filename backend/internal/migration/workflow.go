package migration

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
)

type ReviewPage struct {
	httpx.Layout
	Draft                                                       DraftItem
	RequestKey, Error, Code, RequestID, ReviewHref, ReceiptHref string
}

// AttemptItem menyiapkan snapshot history untuk template. Identitas/bukti operator dan request key
// tidak masuk data tampilan.
type AttemptItem struct {
	ID, Version, Title, TargetName, Revision, Actor, State          string
	SQL, SQLHash, CreatedAt, StartedAt, CommitStartedAt, FinishedAt string
	FirstUnknownAt, ResolvedAt, ResolutionSummary                   string
	FailureCode, FailureMessage, SQLState                           string
	StatementIndex                                                  int
}

type HistoryPage struct {
	httpx.Layout
	Attempts  []AttemptItem
	Pager     DraftPager
	SearchKey string
}

type AttemptPage struct {
	httpx.Layout
	Attempt AttemptItem
}

func reviewPath(target, draft uuid.UUID, input ReserveInput) string {
	return "/databases/" + target.String() + "/drafts/" + draft.String() + "/review?" + url.Values{
		"expected_revision": {strconv.FormatInt(input.ExpectedRevision, 10)}, "request_key": {input.RequestKey.String()},
	}.Encode()
}

func receiptPath(target, key uuid.UUID) string {
	return "/databases/" + target.String() + "/migrations?" + url.Values{"request_key": {key.String()}}.Encode()
}

func attemptPath(target, attempt uuid.UUID) string {
	return "/databases/" + target.String() + "/migrations/" + attempt.String()
}

// Saat error, pengguna memeriksa key yang sama lewat GET sebelum tindakan lain. Draft tidak perlu
// dibaca meski platform berhenti merespons setelah claim.
func (h *Handler) reviewError(w http.ResponseWriter, r *http.Request, layout httpx.Layout, target, draft uuid.UUID, input ReserveInput, err error) {
	status, code, message := httpx.ErrorInfo(err)
	if code == problem.SessionInvalid {
		httpx.WriteError(w, r, err)
		return
	}
	layout.Title = "Check migration request"
	h.render(w, r, layout, status, "review", ReviewPage{
		Layout: layout, Draft: DraftItem{ID: draft.String(), Revision: strconv.FormatInt(input.ExpectedRevision, 10)},
		RequestKey: input.RequestKey.String(), Error: message, Code: string(code), RequestID: httpx.RequestID(r.Context()),
		ReviewHref: reviewPath(target, draft, input), ReceiptHref: receiptPath(target, input.RequestKey),
	})
}

func (h *Handler) review(w http.ResponseWriter, r *http.Request) {
	layout, principal, target, err := h.prepare(r)
	var draftID uuid.UUID
	var values url.Values
	if err == nil {
		draftID, err = draftPathID(r, "draft_id")
	}
	if err == nil {
		values, err = httpx.ParseQuery(r, "expected_revision", "request_key")
	}
	if err == nil && values.Has("expected_revision") != values.Has("request_key") {
		err = attemptFailure(problem.InvalidRequest)
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	var input *ReserveInput
	if len(values) != 0 {
		parsed, parseErr := reviewInput(values)
		if parseErr != nil {
			h.fail(w, r, layout, parseErr)
			return
		}
		input = &parsed
	}
	review, err := h.service.Review(r.Context(), principal, target, draftID, input)
	if err != nil {
		if input == nil {
			h.fail(w, r, layout, err)
		} else {
			h.reviewError(w, r, layout, target, draftID, *input, err)
		}
		return
	}
	if input == nil {
		_ = httpx.SeeOther(w, r, reviewPath(target, draftID, review.Input))
		return
	}
	if review.Receipt != nil {
		_ = httpx.SeeOther(w, r, attemptPath(target, review.Receipt.ID))
		return
	}
	d := review.Draft
	layout.Title = "Review migration"
	h.render(w, r, layout, 200, "review", ReviewPage{
		Layout: layout, Draft: DraftItem{ID: d.ID.String(), Title: d.Title, SQL: d.SQL, Revision: strconv.FormatInt(d.Revision, 10)},
		RequestKey: review.Input.RequestKey.String(),
	})
}

func (h *Handler) execute(w http.ResponseWriter, r *http.Request) {
	layout, principal, target, err := h.prepareAction(r, access.MigrationExecute)
	var draftID uuid.UUID
	var input ReserveInput
	if err == nil {
		draftID, err = draftPathID(r, "draft_id")
	}
	if err == nil {
		err = httpx.ValidateFields(r.PostForm, []string{"_csrf", "expected_revision", "request_key"}, nil)
	}
	if err == nil {
		input, err = reviewInput(r.PostForm)
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	receipt, err := h.service.Execute(r.Context(), principal, target, draftID, input)
	if err != nil {
		_, code, _ := httpx.ErrorInfo(err)
		if code == problem.SessionInvalid || code == problem.DatabaseNotFound || code == problem.DatabaseAccessDenied {
			h.fail(w, r, layout, err)
			return
		}
	}
	if err != nil {
		h.reviewError(w, r, layout, target, draftID, input, err)
		return
	}
	_ = httpx.SeeOther(w, r, attemptPath(target, receipt.ID))
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	layout, principal, target, err := h.prepareAction(r, access.DatabaseView)
	var values url.Values
	if err == nil {
		values, err = httpx.ParseQuery(r, "limit", "offset", "request_key")
	}
	limit, offset := int64(50), int64(0)
	if err == nil && values.Has("limit") {
		limit, err = httpx.Int64Value(values, "limit", 1, 200)
	}
	if err == nil && values.Has("offset") {
		offset, err = httpx.Int64Value(values, "offset", 0, 100000)
	}
	var key uuid.UUID
	if err == nil && values.Has("request_key") {
		key, err = httpx.UUIDValue(values, "request_key")
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	layout.Title, layout.Active = "Migration history", "history"
	page := HistoryPage{Layout: layout}
	if key != uuid.Nil {
		page.SearchKey = key.String()
		receipt, err := h.service.Receipt(r.Context(), principal, target, key)
		if err != nil {
			h.fail(w, r, layout, err)
			return
		}
		if receipt != nil {
			_ = httpx.SeeOther(w, r, attemptPath(target, receipt.ID))
			return
		}
	} else {
		items, more, err := h.service.History(r.Context(), principal, target, int(limit), int(offset))
		if err != nil {
			h.fail(w, r, layout, err)
			return
		}
		for _, item := range items {
			page.Attempts = append(page.Attempts, attemptItem(item))
		}
		page.Pager = draftPager("/databases/"+target.String()+"/migrations", int(limit), int(offset), more)
	}
	h.render(w, r, layout, 200, "history", page)
}

func attemptTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format("2006-01-02 15:04:05")
}

func attemptItem(a AttemptSummary) AttemptItem {
	return AttemptItem{ID: a.ID.String(), Version: a.Version, Title: a.Title, TargetName: a.TargetName,
		Revision: strconv.FormatInt(a.DraftRevision, 10), Actor: a.ActorUsername, State: a.State,
		CreatedAt: attemptTime(&a.CreatedAt), StartedAt: attemptTime(a.StartedAt), FinishedAt: attemptTime(a.FinishedAt)}
}

func (h *Handler) attempt(w http.ResponseWriter, r *http.Request) {
	layout, _, target, err := h.prepareAction(r, access.DatabaseView)
	if err == nil {
		_, err = httpx.ParseQuery(r)
	}
	var id uuid.UUID
	if err == nil {
		id, err = draftPathID(r, "attempt_id")
	}
	var a *AttemptDetail
	if err == nil {
		a, err = h.service.Attempt(r.Context(), requestPrincipal(r), target, id)
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	layout.Title, layout.Active = "Migration result", "history"
	item := attemptItem(a.AttemptSummary)
	item.SQL, item.SQLHash = a.SQL, a.SQLHash
	item.CommitStartedAt, item.FirstUnknownAt, item.ResolvedAt = attemptTime(a.CommitStartedAt), attemptTime(a.FirstUnknownAt), attemptTime(a.ResolvedAt)
	item.ResolutionSummary = a.ResolutionSummary
	if a.Failure != nil {
		item.FailureCode, item.FailureMessage, item.SQLState, item.StatementIndex = a.Failure.Code, a.Failure.Message, a.Failure.SQLState, a.Failure.StatementIndex
	}
	h.render(w, r, layout, 200, "attempt", AttemptPage{Layout: layout, Attempt: item})
}
