package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type pageRenderer interface {
	Render(http.ResponseWriter, *http.Request, int, string, any) error
	RenderError(http.ResponseWriter, *http.Request, httpx.Layout, error)
	HasTemplate(string) bool
}

type Handler struct {
	service    *Service
	middleware *auth.Middleware
	renderer   pageRenderer
}

func NewHandler(service *Service, sessions *auth.Service, renderer *httpx.Renderer) (*Handler, error) {
	if service == nil || sessions == nil || renderer == nil {
		return nil, failure(problem.InternalError)
	}
	for _, name := range []string{"admin_users", "admin_user", "admin_groups", "admin_group", "error"} {
		if !renderer.HasTemplate(name) {
			return nil, failure(problem.InternalError)
		}
	}
	middleware, err := auth.NewMiddleware(sessions)
	if err != nil {
		return nil, err
	}
	return &Handler{service: service, middleware: middleware, renderer: renderer}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /admin/users", h.middleware.RequireSession(http.HandlerFunc(h.users)))
	mux.Handle("GET /admin/users/{user_id}", h.middleware.RequireSession(http.HandlerFunc(h.user)))
	mux.Handle("GET /admin/groups", h.middleware.RequireSession(http.HandlerFunc(h.groups)))
	mux.Handle("GET /admin/groups/{group_id}", h.middleware.RequireSession(http.HandlerFunc(h.group)))
	post := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, h.middleware.RequireSession(httpx.LimitBody(httpx.NormalBodyLimit, h.middleware.RequireCSRF(handler))))
	}
	post("POST /admin/users", h.createUser)
	post("POST /admin/users/{user_id}", h.editUser)
	post("POST /admin/users/{user_id}/password", h.resetPassword)
	post("POST /admin/groups", h.createGroup)
	post("POST /admin/groups/{group_id}", h.editGroup)
	post("POST /admin/groups/{group_id}/members", h.addMember)
	post("POST /admin/groups/{group_id}/members/{user_id}/remove", h.removeMember)
	post("POST /admin/groups/{group_id}/databases", h.setGrant)
	post("POST /admin/groups/{group_id}/databases/{database_id}/remove", h.removeGrant)
}

func (h *Handler) layout(r *http.Request, title, active string) (httpx.Layout, access.Principal, error) {
	state, ok := auth.SessionFromContext(r.Context())
	principal, present := access.FromContext(r.Context())
	if !ok || !present || state.Kind != auth.AuthenticatedSession || state.Principal != principal {
		return httpx.Layout{}, access.Principal{}, failure(problem.SessionInvalid)
	}
	layout := httpx.Layout{Title: title, Active: active, CSRF: state.CSRF}
	if err := h.service.authorizer.RequireAdmin(r.Context(), principal); err != nil {
		return layout, principal, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, h.service.pool)
	if err != nil {
		return layout, principal, failure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	var user httpx.LayoutUser
	err = lease.QueryRow(ctx, `SELECT display_name,username,is_platform_admin FROM databrain.users
	    WHERE id=$1 AND is_active AND security_version=$2`, principal.UserID, principal.SecurityVersion).
		Scan(&user.DisplayName, &user.Username, &user.IsAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return layout, principal, failure(problem.SessionInvalid)
	}
	if err != nil {
		return layout, principal, failure(problem.PlatformUnavailable)
	}
	layout.User = &user
	return layout, principal, nil
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, layout httpx.Layout, err error) {
	var value *problem.Error
	if errors.As(err, &value) && value.Code == problem.SessionInvalid {
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

func formStatus(err error) (int, string, bool) {
	if err == nil {
		return http.StatusOK, "", false
	}
	status, code, message := httpx.ErrorInfo(err)
	return status, message, code == problem.RevisionConflict
}

func userForm(user UserItem) UserForm {
	return UserForm{DisplayName: user.DisplayName, ExpectedRevision: strconv.FormatInt(user.Revision, 10),
		IsActive: user.IsActive, IsAdmin: user.IsAdmin}
}

func submittedUserForm(r *http.Request) UserForm {
	values := httpx.SafeFormValues(r.PostForm, "username", "display_name", "expected_revision", "is_active", "is_platform_admin")
	return UserForm{Username: values.Get("username"), DisplayName: values.Get("display_name"),
		ExpectedRevision: values.Get("expected_revision"), IsActive: values.Get("is_active") == "true",
		IsAdmin: values.Get("is_platform_admin") == "true"}
}

func groupForm(group GroupItem) GroupForm {
	return GroupForm{Name: group.Name, ExpectedRevision: strconv.FormatInt(group.Revision, 10), IsActive: group.IsActive}
}

func submittedGroupForm(r *http.Request) GroupForm {
	values := httpx.SafeFormValues(r.PostForm, "name", "expected_revision", "is_active")
	return GroupForm{Name: values.Get("name"), ExpectedRevision: values.Get("expected_revision"), IsActive: values.Get("is_active") == "true"}
}

func (h *Handler) renderUsers(w http.ResponseWriter, r *http.Request, layout httpx.Layout, principal access.Principal, limit, offset int, form UserForm, formErr error) {
	users, more, err := h.service.ListUsers(r.Context(), principal, limit, offset)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	status, message, _ := formStatus(formErr)
	page := UsersPage{Layout: layout, Users: users, Pager: listPager("/admin/users", limit, offset, more), Form: form, Error: message}
	h.render(w, r, layout, status, "admin_users", page)
}

func (h *Handler) renderUser(w http.ResponseWriter, r *http.Request, layout httpx.Layout, principal access.Principal, id uuid.UUID, form *UserForm, formErr error) {
	user, groups, err := h.service.User(r.Context(), principal, id)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	pageForm := userForm(user)
	if form != nil {
		pageForm = *form
	}
	status, message, conflict := formStatus(formErr)
	page := UserPage{Layout: layout, UserProfile: user, Groups: groups, Form: pageForm, Error: message, Conflict: conflict}
	h.render(w, r, layout, status, "admin_user", page)
}

func (h *Handler) renderGroups(w http.ResponseWriter, r *http.Request, layout httpx.Layout, principal access.Principal, limit, offset int, form GroupForm, formErr error) {
	groups, more, err := h.service.ListGroups(r.Context(), principal, limit, offset)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	status, message, _ := formStatus(formErr)
	page := GroupsPage{Layout: layout, Groups: groups, Pager: listPager("/admin/groups", limit, offset, more), Form: form, Error: message}
	h.render(w, r, layout, status, "admin_groups", page)
}

func (h *Handler) renderGroup(w http.ResponseWriter, r *http.Request, layout httpx.Layout, principal access.Principal, id uuid.UUID, form *GroupForm, formErr error) {
	group, members, memberChoices, grants, databaseChoices, err := h.service.GroupDetails(r.Context(), principal, id)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	pageForm := groupForm(group)
	if form != nil {
		pageForm = *form
	}
	status, message, conflict := formStatus(formErr)
	page := GroupPage{Layout: layout, GroupProfile: group, Form: pageForm, Members: members,
		MemberChoices: memberChoices, Grants: grants, DatabaseChoices: databaseChoices, Error: message, Conflict: conflict}
	h.render(w, r, layout, status, "admin_group", page)
}

func pathID(r *http.Request, name string) (uuid.UUID, error) { return parsePathID(r.PathValue(name)) }

func (h *Handler) users(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "Users", "users")
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	limit, offset, err := parsePagination(r)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.renderUsers(w, r, layout, principal, limit, offset, UserForm{}, nil)
}

func (h *Handler) user(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "User", "users")
	if err == nil {
		_, err = httpx.ParseQuery(r)
	}
	var id uuid.UUID
	if err == nil {
		id, err = pathID(r, "user_id")
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.renderUser(w, r, layout, principal, id, nil, nil)
}

func (h *Handler) groups(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "Groups", "groups")
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	limit, offset, err := parsePagination(r)
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.renderGroups(w, r, layout, principal, limit, offset, GroupForm{}, nil)
}

func (h *Handler) group(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "Group", "groups")
	if err == nil {
		_, err = httpx.ParseQuery(r)
	}
	var id uuid.UUID
	if err == nil {
		id, err = pathID(r, "group_id")
	}
	if err != nil {
		h.fail(w, r, layout, err)
		return
	}
	h.renderGroup(w, r, layout, principal, id, nil, nil)
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "Users", "users")
	if err == nil {
		var input CreateUserInput
		input, err = parseCreateUser(r.PostForm)
		if err == nil {
			var id uuid.UUID
			id, err = h.service.CreateUser(r.Context(), principal, input)
			if err == nil {
				_ = httpx.SeeOther(w, r, "/admin/users/"+id.String())
				return
			}
		}
	}
	if err != nil {
		status, _, _ := httpx.ErrorInfo(err)
		if status == 400 || status == 409 || status == 422 {
			h.renderUsers(w, r, layout, principal, 50, 0, submittedUserForm(r), err)
			return
		}
	}
	h.fail(w, r, layout, err)
}

func (h *Handler) editUser(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "User", "users")
	var id uuid.UUID
	if err == nil {
		id, err = pathID(r, "user_id")
	}
	if err == nil {
		var input EditUserInput
		input, err = parseEditUser(r.PostForm)
		if err == nil {
			var login bool
			login, err = h.service.EditUser(r.Context(), principal, id, input)
			if err == nil {
				h.redirectMutation(w, r, login, "/admin/users/"+id.String())
				return
			}
		}
	}
	h.renderUserMutationError(w, r, layout, principal, id, err, true)
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "User", "users")
	var id uuid.UUID
	if err == nil {
		id, err = pathID(r, "user_id")
	}
	if err == nil {
		var input ResetPasswordInput
		input, err = parseResetPassword(r.PostForm)
		if err == nil {
			var login bool
			login, err = h.service.ResetPassword(r.Context(), principal, id, input)
			if err == nil {
				h.redirectMutation(w, r, login, "/admin/users/"+id.String())
				return
			}
		}
	}
	h.renderUserMutationError(w, r, layout, principal, id, err, false)
}

func (h *Handler) renderUserMutationError(w http.ResponseWriter, r *http.Request, layout httpx.Layout, principal access.Principal, id uuid.UUID, err error, preserve bool) {
	status, _, _ := httpx.ErrorInfo(err)
	if id != uuid.Nil && (status == 400 || status == 409 || status == 422) {
		if !preserve {
			h.renderUser(w, r, layout, principal, id, nil, err)
			return
		}
		form := submittedUserForm(r)
		h.renderUser(w, r, layout, principal, id, &form, err)
		return
	}
	h.fail(w, r, layout, err)
}

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "Groups", "groups")
	if err == nil {
		var input CreateGroupInput
		input, err = parseCreateGroup(r.PostForm)
		if err == nil {
			var id uuid.UUID
			id, err = h.service.CreateGroup(r.Context(), principal, input)
			if err == nil {
				_ = httpx.SeeOther(w, r, "/admin/groups/"+id.String())
				return
			}
		}
	}
	if err != nil {
		status, _, _ := httpx.ErrorInfo(err)
		if status == 400 || status == 409 || status == 422 {
			h.renderGroups(w, r, layout, principal, 50, 0, submittedGroupForm(r), err)
			return
		}
	}
	h.fail(w, r, layout, err)
}

func (h *Handler) editGroup(w http.ResponseWriter, r *http.Request) {
	layout, principal, err := h.layout(r, "Group", "groups")
	var id uuid.UUID
	if err == nil {
		id, err = pathID(r, "group_id")
	}
	if err == nil {
		var input EditGroupInput
		input, err = parseEditGroup(r.PostForm)
		if err == nil {
			var login bool
			login, err = h.service.EditGroup(r.Context(), principal, id, input)
			if err == nil {
				h.redirectMutation(w, r, login, "/admin/groups/"+id.String())
				return
			}
		}
	}
	h.renderGroupMutationError(w, r, layout, principal, id, err, true)
}

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	h.groupRelation(w, r, func(id uuid.UUID) (bool, error) {
		input, err := parseMember(r.PostForm)
		if err != nil {
			return false, err
		}
		principal, _ := access.FromContext(r.Context())
		return h.service.AddMember(r.Context(), principal, id, input.UserID)
	})
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	h.groupRelation(w, r, func(id uuid.UUID) (bool, error) {
		if err := parseNoFields(r.PostForm); err != nil {
			return false, err
		}
		userID, err := pathID(r, "user_id")
		if err != nil {
			return false, err
		}
		principal, _ := access.FromContext(r.Context())
		return h.service.RemoveMember(r.Context(), principal, id, userID)
	})
}

func (h *Handler) setGrant(w http.ResponseWriter, r *http.Request) {
	h.groupRelation(w, r, func(id uuid.UUID) (bool, error) {
		input, err := parseGrant(r.PostForm)
		if err != nil {
			return false, err
		}
		principal, _ := access.FromContext(r.Context())
		return h.service.SetGrant(r.Context(), principal, id, input.DatabaseID, input.Access)
	})
}

func (h *Handler) removeGrant(w http.ResponseWriter, r *http.Request) {
	h.groupRelation(w, r, func(id uuid.UUID) (bool, error) {
		if err := parseNoFields(r.PostForm); err != nil {
			return false, err
		}
		targetID, err := pathID(r, "database_id")
		if err != nil {
			return false, err
		}
		principal, _ := access.FromContext(r.Context())
		return h.service.RemoveGrant(r.Context(), principal, id, targetID)
	})
}

func (h *Handler) groupRelation(w http.ResponseWriter, r *http.Request, change func(uuid.UUID) (bool, error)) {
	layout, principal, err := h.layout(r, "Group", "groups")
	var id uuid.UUID
	if err == nil {
		id, err = pathID(r, "group_id")
	}
	if err == nil {
		var login bool
		login, err = change(id)
		if err == nil {
			h.redirectMutation(w, r, login, "/admin/groups/"+id.String())
			return
		}
	}
	h.renderGroupMutationError(w, r, layout, principal, id, err, false)
}

func (h *Handler) renderGroupMutationError(w http.ResponseWriter, r *http.Request, layout httpx.Layout, principal access.Principal, id uuid.UUID, err error, preserve bool) {
	status, _, _ := httpx.ErrorInfo(err)
	if id != uuid.Nil && (status == 400 || status == 409 || status == 422) {
		if preserve {
			form := submittedGroupForm(r)
			h.renderGroup(w, r, layout, principal, id, &form, err)
		} else {
			h.renderGroup(w, r, layout, principal, id, nil, err)
		}
		return
	}
	h.fail(w, r, layout, err)
}

func (h *Handler) redirectMutation(w http.ResponseWriter, r *http.Request, login bool, success string) {
	if login {
		_ = httpx.SeeOther(w, r, "/login")
		return
	}
	_ = httpx.SeeOther(w, r, success)
}
