package migration

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type DraftPager struct{ Prev, Next string }

// DraftItem is display data for Jordan's templates. List items leave SQL empty.
// Revision is text so a conflict can keep the submitted expected revision.
type DraftItem struct {
	ID, Title, SQL, Revision, UpdatedBy, UpdatedAt string
}

type DraftsPage struct {
	httpx.Layout
	Drafts []DraftItem
	Pager  DraftPager
	Form   DraftInput
	Error  string
}

type DraftPage struct {
	httpx.Layout
	Draft               DraftItem
	Error               string
	Conflict, CanReview bool
}

type Handler struct {
	store      *DraftStore
	authorizer access.Authorizer
	renderer   *httpx.Renderer
}

func NewHandler(store *DraftStore, authorizer access.Authorizer, renderer *httpx.Renderer) (*Handler, error) {
	if store == nil || store.pool == nil || authorizer == nil || !renderer.HasTemplate("drafts") || !renderer.HasTemplate("draft") || !renderer.HasTemplate("error") {
		return nil, draftFailure(problem.InternalError)
	}
	return &Handler{store, authorizer, renderer}, nil
}

func (h *Handler) Register(mux *http.ServeMux, middleware *auth.Middleware) {
	register := func(pattern string, post bool, fn http.HandlerFunc) {
		var next http.Handler = fn
		if post {
			next = httpx.LimitBody(httpx.MaxFormBody, middleware.RequireCSRF(next))
		}
		next = middleware.RequireSession(next)
		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		}))
	}
	register("GET /databases/{database_id}/drafts", false, h.list)
	register("POST /databases/{database_id}/drafts", true, h.create)
	register("GET /databases/{database_id}/drafts/{draft_id}", false, h.detail)
	register("POST /databases/{database_id}/drafts/{draft_id}", true, h.save)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, layout httpx.Layout, err error) {
	var p *problem.Error
	if errors.As(err, &p) && p.Code == problem.SessionInvalid {
		httpx.WriteError(w, r, err)
		return
	}
	h.renderer.RenderError(w, r, layout, err)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, layout httpx.Layout, status int, name string, page any) {
	if err := h.renderer.Render(w, r, status, name, page); err != nil {
		h.fail(w, r, layout, err)
	}
}

func draftPathID(r *http.Request, name string) (uuid.UUID, error) {
	return httpx.UUIDValue(url.Values{name: {r.PathValue(name)}}, name)
}

// prepare authorizes before any draft/display lookup. Identity is exclusively
// the session validated by Brain's middleware and rechecked by Authorizer.
func (h *Handler) prepare(r *http.Request) (httpx.Layout, access.Principal, uuid.UUID, error) {
	layout := httpx.Layout{Title: "Drafts", Active: "drafts"}
	state, ok := auth.SessionFromContext(r.Context())
	principal, present := access.FromContext(r.Context())
	if !ok || !present || state.Kind != auth.AuthenticatedSession || state.Principal != principal {
		return layout, principal, uuid.Nil, draftFailure(problem.SessionInvalid)
	}
	layout.CSRF = state.CSRF
	id, err := draftPathID(r, "database_id")
	if err != nil {
		return layout, principal, id, err
	}
	if _, err = h.authorizer.RequireDatabase(r.Context(), principal, id, access.MigrationWrite); err != nil {
		return layout, principal, id, err
	}
	lease, err := platformdb.Acquire(r.Context(), h.store.pool)
	if err != nil {
		return layout, principal, id, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	user := &httpx.LayoutUser{}
	database := &httpx.LayoutDatabase{ID: id.String(), CanEdit: true}
	err = lease.QueryRow(r.Context(), `SELECT u.display_name,u.username,u.is_platform_admin,t.name
	    FROM databrain.users u CROSS JOIN databrain.database_targets t
	    WHERE u.id=$1 AND u.is_active AND u.security_version=$2 AND t.id=$3 AND t.is_active`, principal.UserID, principal.SecurityVersion, id).
		Scan(&user.DisplayName, &user.Username, &user.IsAdmin, &database.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return layout, principal, id, draftFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return layout, principal, id, draftFailure(problem.PlatformUnavailable)
	}
	layout.User, layout.Database = user, database
	return layout, principal, id, nil
}

func draftPagination(r *http.Request) (int, int, error) {
	values, err := httpx.ParseQuery(r, "limit", "offset")
	if err != nil {
		return 0, 0, err
	}
	limit, offset := int64(50), int64(0)
	if values.Has("limit") {
		limit, err = httpx.Int64Value(values, "limit", 1, 200)
	}
	if err == nil && values.Has("offset") {
		offset, err = httpx.Int64Value(values, "offset", 0, 100000)
	}
	return int(limit), int(offset), err
}

func draftPager(path string, limit, offset int, more bool) DraftPager {
	link := func(offset int) string {
		return path + "?" + url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}.Encode()
	}
	var p DraftPager
	if offset > 0 {
		p.Prev = link(max(0, offset-limit))
	}
	if more && offset+limit <= 100000 {
		p.Next = link(offset + limit)
	}
	return p
}

func (h *Handler) display(ctx context.Context, drafts []DraftSummary) ([]DraftItem, error) {
	items := make([]DraftItem, 0, len(drafts))
	if len(drafts) == 0 {
		return items, nil
	}
	actors := make([]uuid.UUID, 0, len(drafts))
	for _, d := range drafts {
		actors = append(actors, d.UpdatedBy)
	}
	lease, err := platformdb.Acquire(ctx, h.store.pool)
	if err != nil {
		return nil, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	rows, err := lease.Query(ctx, `SELECT id,username FROM databrain.users WHERE id=ANY($1)`, actors)
	if err != nil {
		return nil, draftFailure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	names := make(map[uuid.UUID]string)
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, draftFailure(problem.PlatformUnavailable)
		}
		names[id] = name
	}
	if rows.Err() != nil {
		return nil, draftFailure(problem.PlatformUnavailable)
	}
	for _, d := range drafts {
		items = append(items, DraftItem{ID: d.ID.String(), Title: d.Title, Revision: strconv.FormatInt(d.Revision, 10), UpdatedBy: names[d.UpdatedBy], UpdatedAt: d.UpdatedAt.UTC().Format("2006-01-02 15:04:05")})
	}
	return items, nil
}

func (h *Handler) renderList(w http.ResponseWriter, r *http.Request, layout httpx.Layout, id uuid.UUID, limit, offset, status int, form DraftInput, message string) {
	drafts, more, err := h.store.List(r.Context(), id, limit, offset)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	items, err := h.display(r.Context(), drafts)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.render(w, r, layout, status, "drafts", DraftsPage{Layout: layout, Drafts: items, Pager: draftPager("/databases/"+id.String()+"/drafts", limit, offset, more), Form: form, Error: message})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	layout, _, id, err := h.prepare(r)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	limit, offset, err := draftPagination(r)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.renderList(w, r, layout, id, limit, offset, 200, DraftInput{}, "")
}

func (h *Handler) renderDraft(w http.ResponseWriter, r *http.Request, layout httpx.Layout, d Draft, status int, input *SaveDraftInput, message string, conflict bool) {
	items, err := h.display(r.Context(), []DraftSummary{d.DraftSummary})
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	item := items[0]
	item.SQL = d.SQL
	if input != nil {
		item.Title, item.SQL, item.Revision = input.Title, input.SQL, strconv.FormatInt(input.ExpectedRevision, 10)
	}
	layout.Title = "Edit draft"
	h.render(w, r, layout, status, "draft", DraftPage{Layout: layout, Draft: item, Error: message, Conflict: conflict})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	layout, _, id, err := h.prepare(r)
	if err == nil {
		_, err = httpx.ParseQuery(r)
	}
	var draftID uuid.UUID
	if err == nil {
		draftID, err = draftPathID(r, "draft_id")
	}
	var d Draft
	if err == nil {
		d, err = h.store.Get(r.Context(), id, draftID)
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.renderDraft(w, r, layout, d, 200, nil, "", false)
}

func submittedDraft(values url.Values, edit bool) (SaveDraftInput, error) {
	fields := []string{"_csrf", "title", "sql"}
	if edit {
		fields = append(fields, "expected_revision")
	}
	if err := httpx.ValidateFields(values, fields, nil); err != nil {
		return SaveDraftInput{}, err
	}
	safe := httpx.SafeFormValues(values, "title", "sql", "expected_revision")
	in := SaveDraftInput{DraftInput: DraftInput{Title: safe.Get("title"), SQL: safe.Get("sql")}}
	if edit {
		var err error
		in.ExpectedRevision, err = httpx.Int64Value(safe, "expected_revision", 1, math.MaxInt64)
		if err != nil {
			return SaveDraftInput{}, err
		}
	}
	return in, nil
}

func draftFormError(input DraftInput) string {
	if validateDraft(input) == nil {
		return ""
	}
	if !utf8.ValidString(input.Title) || strings.TrimSpace(input.Title) == "" || utf8.RuneCountInString(input.Title) > 120 || strings.ContainsRune(input.Title, 0) {
		return "Title must contain 1–120 characters, with nonblank text and no NUL."
	}
	return "SQL must contain 1 byte to 128 KiB of UTF-8 text without NUL. Invalid SQL syntax may be saved."
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	layout, principal, id, err := h.prepare(r)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	in, err := submittedDraft(r.PostForm, false)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	if message := draftFormError(in.DraftInput); message != "" {
		h.renderList(w, r, layout, id, 50, 0, 422, in.DraftInput, message)
		return
	}
	d, err := h.store.Create(r.Context(), id, principal.UserID, in.DraftInput)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	_ = httpx.SeeOther(w, r, "/databases/"+id.String()+"/drafts/"+d.ID.String())
}

func (h *Handler) save(w http.ResponseWriter, r *http.Request) {
	layout, principal, id, err := h.prepare(r)
	var draftID uuid.UUID
	if err == nil {
		draftID, err = draftPathID(r, "draft_id")
	}
	var in SaveDraftInput
	if err == nil {
		in, err = submittedDraft(r.PostForm, true)
	}
	var d Draft
	if err == nil {
		d, err = h.store.Get(r.Context(), id, draftID)
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	if message := draftFormError(in.DraftInput); message != "" {
		h.renderDraft(w, r, layout, d, 422, &in, message, false)
		return
	}
	_, err = h.store.Save(r.Context(), id, draftID, principal.UserID, in)
	if err != nil {
		status, code, message := httpx.ErrorInfo(err)
		if code == problem.RevisionConflict {
			h.renderDraft(w, r, layout, d, status, &in, message, true)
		} else {
			h.fail(w, r, layout, err)
		}
		return
	}
	_ = httpx.SeeOther(w, r, "/databases/"+id.String()+"/drafts/"+draftID.String())
}
