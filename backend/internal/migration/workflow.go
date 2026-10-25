package migration

import (
	"math"
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

func reviewInput(values url.Values) (ReserveInput, error) {
	revision, err := httpx.Int64Value(values, "expected_revision", 1, math.MaxInt64)
	if err != nil {
		return ReserveInput{}, err
	}
	key, err := httpx.UUIDValue(values, "request_key")
	return ReserveInput{revision, key}, err
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
	if len(values) == 0 {
		d, err := h.store.Get(r.Context(), target, draftID)
		if err != nil {
			h.fail(w, r, layout, err)
			return
		}
		_ = httpx.SeeOther(w, r, reviewPath(target, draftID, ReserveInput{d.Revision, uuid.New()}))
		return
	}
	input, err := reviewInput(values)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	// Cocokkan receipt sebelum memeriksa revision draft terkini dan target sibuk.
	receipt, err := h.runner.replay(r.Context(), target, draftID, principal.UserID, input)
	if err == nil && receipt != nil {
		_ = httpx.SeeOther(w, r, attemptPath(target, receipt.ID))
		return
	}
	var d Draft
	if err == nil {
		d, err = h.store.Get(r.Context(), target, draftID)
	}
	if err == nil && d.Revision != input.ExpectedRevision {
		err = attemptFailure(problem.RevisionConflict)
	}
	if err != nil {
		h.reviewError(w, r, layout, target, draftID, input, err)
		return
	}
	layout.Title = "Review migration"
	h.render(w, r, layout, 200, "review", ReviewPage{
		Layout: layout, Draft: DraftItem{ID: d.ID.String(), Title: d.Title, SQL: d.SQL, Revision: strconv.FormatInt(d.Revision, 10)},
		RequestKey: input.RequestKey.String(),
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
	receipt, err := h.runner.Execute(r.Context(), principal, target, draftID, input)
	if err != nil {
		_, code, _ := httpx.ErrorInfo(err)
		if code == problem.SessionInvalid || code == problem.DatabaseNotFound || code == problem.DatabaseAccessDenied {
			h.fail(w, r, layout, err)
			return
		}
		// Response hilang atau error persistence masih dapat menyisakan receipt permanen. Baca sekali dengan
		// batas waktu cleanup; jangan mengulang SQL atau mengganti key.
		end, _ := r.Context().Deadline()
		ctx, cancel := cleanupExecutionContext(end, 2*time.Second)
		stored, lookupErr := h.runner.replay(ctx, target, draftID, principal.UserID, input)
		cancel()
		if lookupErr == nil && stored != nil {
			receipt, err = *stored, nil
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
		receipt, err := h.runner.store.Receipt(r.Context(), target, principal.UserID, key)
		if err != nil {
			h.fail(w, r, layout, err)
			return
		}
		if receipt != nil {
			_ = httpx.SeeOther(w, r, attemptPath(target, receipt.ID))
			return
		}
	} else {
		items, more, err := h.runner.store.List(r.Context(), target, int(limit), int(offset))
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
		a, err = h.runner.store.Get(r.Context(), target, id)
		if err == nil && a == nil {
			err = attemptFailure(problem.AttemptNotFound)
		}
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
